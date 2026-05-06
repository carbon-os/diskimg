package fat32

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	volfs "github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/fs/fstype"
)

// ── cluster allocation ────────────────────────────────────────────────────────

func (v *Volume) allocCluster() (uint32, error) {
	for n := uint32(2); n < v.b.cntClusters+2; n++ {
		if v.fatEntry(n) == fatFREE {
			v.setFATEntry(n, v.eocValue())
			return n, nil
		}
	}
	return 0, fmt.Errorf("fat32: disk full")
}

func (v *Volume) appendCluster(last uint32) (uint32, error) {
	n, err := v.allocCluster()
	if err != nil {
		return 0, err
	}
	v.setFATEntry(last, n)
	return n, nil
}

func (v *Volume) freeChain(first uint32) {
	cur := first
	for cur >= 2 && !v.isEOC(v.fatEntry(cur)) {
		next := v.fatEntry(cur)
		v.setFATEntry(cur, fatFREE)
		cur = next
	}
	if cur >= 2 {
		v.setFATEntry(cur, fatFREE) // free the EOC cluster itself
	}
}

func (v *Volume) clusterChain(first uint32) []uint32 {
	var chain []uint32
	for cur := first; cur >= 2; cur = v.fatEntry(cur) {
		chain = append(chain, cur)
		if v.isEOC(v.fatEntry(cur)) {
			break
		}
	}
	return chain
}

// ── writeDirEntry ─────────────────────────────────────────────────────────────

// writeDirEntry writes the short directory entry back to its on-disk location.
// e.dirCluster and e.dirOffset must be set (populated by lookupPath /
// appendEntryToDir). For FAT12/16 root, dirCluster == 0.
func (v *Volume) writeDirEntry(e *fatDirEntry) error {
	secSize := int(v.b.bytesPerSec)

	if e.dirCluster == 0 && v.b.fatType != fstype.FAT32 {
		// FAT12/16 fixed root directory: find and rewrite the containing sector.
		targetSec := v.b.firstRootDir + e.dirOffset/uint32(secSize)
		inSecOff  := e.dirOffset % uint32(secSize)
		secBuf    := make([]byte, secSize)
		if _, err := v.sr.ReadAt(secBuf, int64(targetSec)*int64(secSize)); err != nil {
			return fmt.Errorf("fat32: writeDirEntry read sector %d: %w", targetSec, err)
		}
		encodeDirEntry32(secBuf[inSecOff:], e)
		v.dirty[targetSec] = secBuf
		return nil
	}

	// FAT32 or subdirectory: e.dirCluster is the exact cluster, e.dirOffset
	// is the byte offset of the short entry within that cluster.
	clus, err := v.readCluster(e.dirCluster)
	if err != nil {
		return err
	}
	encodeDirEntry32(clus[e.dirOffset:], e)
	v.writeCluster(e.dirCluster, clus)
	return nil
}

func encodeDirEntry32(buf []byte, e *fatDirEntry) {
	le := binary.LittleEndian
	copy(buf[0:11], e.shortName[:])
	buf[11] = e.attr
	buf[12] = 0
	buf[13] = 0
	le.PutUint16(buf[14:16], e.crtTime)
	le.PutUint16(buf[16:18], e.crtDate)
	le.PutUint16(buf[18:20], 0) // last access date (ignored)
	le.PutUint16(buf[20:22], uint16(e.cluster>>16))
	le.PutUint16(buf[22:24], e.wrtTime)
	le.PutUint16(buf[24:26], e.wrtDate)
	le.PutUint16(buf[26:28], uint16(e.cluster&0xFFFF))
	le.PutUint32(buf[28:32], e.size)
}

// ── writeFileData ─────────────────────────────────────────────────────────────

// writeFileData writes data as the content of file e, extending or shrinking
// the cluster chain as needed, then updates the directory entry.
func (v *Volume) writeFileData(e *fatDirEntry, data []byte) error {
	size         := uint32(len(data))
	clusterSize  := int(v.b.clusterSize)
	needed       := (int(size) + clusterSize - 1) / clusterSize
	if needed == 0 {
		needed = 1
	}

	var clusters []uint32
	if e.cluster < 2 {
		// New file: allocate first cluster.
		first, err := v.allocCluster()
		if err != nil {
			return err
		}
		e.cluster = first
		clusters  = []uint32{first}
	} else {
		clusters = v.clusterChain(e.cluster)
	}

	for len(clusters) < needed {
		next, err := v.appendCluster(clusters[len(clusters)-1])
		if err != nil {
			return err
		}
		clusters = append(clusters, next)
	}
	// Truncate excess clusters.
	if len(clusters) > needed {
		v.setFATEntry(clusters[needed-1], v.eocValue())
		for _, c := range clusters[needed:] {
			v.setFATEntry(c, fatFREE)
		}
		clusters = clusters[:needed]
	}

	// Write data cluster by cluster.
	for i, c := range clusters {
		buf := make([]byte, clusterSize)
		off := i * clusterSize
		n   := clusterSize
		if off+n > len(data) {
			n = len(data) - off
		}
		copy(buf, data[off:off+n])
		v.writeCluster(c, buf)
	}

	now         := time.Now()
	e.size      = size
	e.wrtDate   = fatDate(now)
	e.wrtTime   = fatTime(now)
	return v.writeDirEntry(e)
}

// ── directory modification ────────────────────────────────────────────────────

// addDirEntry inserts a new entry named name into the directory at dirCluster.
func (v *Volume) addDirEntry(dirCluster uint32, name string, attr uint8, cluster uint32, size uint32) error {
	sn, fits := toShortName(name)
	if !fits {
		// Generate a lossy 8.3 alias; a proper implementation would check for
		// collisions and increment the numeric suffix.
		base := strings.ToUpper(name)
		ext  := ""
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			ext  = strings.ToUpper(name[dot+1:])
			base = strings.ToUpper(name[:dot])
		}
		if len(base) > 6 {
			base = base[:6]
		}
		alias := base + "~1"
		for i := range sn {
			sn[i] = ' '
		}
		copy(sn[:8], alias)
		if len(ext) > 3 {
			ext = ext[:3]
		}
		copy(sn[8:11], ext)
	}

	now := time.Now()
	e   := &fatDirEntry{
		name:      name,
		shortName: sn,
		attr:      attr,
		cluster:   cluster,
		size:      size,
		wrtDate:   fatDate(now),
		wrtTime:   fatTime(now),
		crtDate:   fatDate(now),
		crtTime:   fatTime(now),
	}
	needsLFN := !fits || name != decodeShortName(sn)
	return v.appendEntryToDir(dirCluster, e, needsLFN)
}

// appendEntryToDir finds free slots and writes LFN + short entry to the directory.
func (v *Volume) appendEntryToDir(dirCluster uint32, e *fatDirEntry, writeLFN bool) error {
	var lfnEntries [][]byte
	if writeLFN {
		lfnEntries = buildLFNEntries(e.name, e.shortName)
	}
	total  := len(lfnEntries) + 1
	needed := uint32(total * 32)

	// ── FAT12/16 fixed root directory ───────────────────────────────────────
	if dirCluster == 0 && v.b.fatType != fstype.FAT32 {
		sec      := int64(v.b.firstRootDir) * int64(v.b.bytesPerSec)
		rootSize := int64(v.b.rootEntCnt) * 32
		rootBuf  := make([]byte, rootSize)
		if _, err := v.sr.ReadAt(rootBuf, sec); err != nil {
			return err
		}
		off, err := findFreeSlot(rootBuf, needed)
		if err != nil {
			return fmt.Errorf("fat32: root directory full")
		}
		writeEntriesAt(rootBuf[off:], lfnEntries, e)
		// Record the short entry's location.
		e.dirCluster = 0
		e.dirOffset  = uint32(off) + uint32((total-1)*32)
		// Stage all modified root-dir sectors.
		secSize  := int(v.b.bytesPerSec)
		for i := 0; i < int(v.b.rootDirSec); i++ {
			buf := make([]byte, secSize)
			copy(buf, rootBuf[i*secSize:(i+1)*secSize])
			v.dirty[v.b.firstRootDir+uint32(i)] = buf
		}
		return nil
	}

	// ── FAT32 or subdirectory cluster chain ──────────────────────────────────
	cur     := dirCluster
	var prev uint32
	for cur >= 2 {
		clus, err := v.readCluster(cur)
		if err != nil {
			return err
		}
		off, err := findFreeSlot(clus, needed)
		if err == nil {
			writeEntriesAt(clus[off:], lfnEntries, e)
			v.writeCluster(cur, clus)
			// dirOffset is within this specific cluster (not chain-relative).
			e.dirCluster = cur
			e.dirOffset  = uint32(off) + uint32((total-1)*32)
			return nil
		}
		prev = cur
		next := v.fatEntry(cur)
		if v.isEOC(next) {
			break
		}
		cur = next
	}

	// No free slot found: grow the directory by one cluster.
	newClus, err := v.appendCluster(prev)
	if err != nil {
		return err
	}
	clus := make([]byte, v.b.clusterSize)
	writeEntriesAt(clus, lfnEntries, e)
	v.writeCluster(newClus, clus)
	e.dirCluster = newClus
	e.dirOffset  = uint32((total - 1) * 32)
	return nil
}

func findFreeSlot(buf []byte, needed uint32) (int, error) {
	var count uint32
	start := -1
	for i := 0; i+32 <= len(buf); i += 32 {
		if buf[i] == 0x00 || buf[i] == 0xE5 {
			if count == 0 {
				start = i
			}
			count += 32
			if count >= needed {
				return start, nil
			}
		} else {
			count = 0
			start = -1
		}
	}
	return 0, fmt.Errorf("fat32: no free slot")
}

func writeEntriesAt(buf []byte, lfn [][]byte, e *fatDirEntry) {
	for i, l := range lfn {
		copy(buf[i*32:], l)
	}
	encodeDirEntry32(buf[len(lfn)*32:], e)
}

// buildLFNEntries returns LFN 32-byte entries in physical disk order:
// highest sequence number first (with the 0x40 flag), down to sequence 1.
func buildLFNEntries(name string, sn [11]byte) [][]byte {
	runes := []rune(name)
	// Append null terminator + 0xFFFF padding to the next multiple of 13,
	// unless the name is already an exact multiple of 13 (no terminator needed).
	if len(runes)%13 != 0 {
		runes = append(runes, 0x0000)
		for len(runes)%13 != 0 {
			runes = append(runes, 0xFFFF)
		}
	}
	nSeq    := len(runes) / 13
	cksum   := lfnChecksum(sn)
	entries := make([][]byte, nSeq)

	// Physical order: entries[0] has the highest sequence (with 0x40 flag),
	// entries[nSeq-1] has sequence 1.
	for i := 0; i < nSeq; i++ {
		seq := nSeq - i // nSeq, nSeq-1, ..., 1
		buf := make([]byte, 32)
		buf[0] = byte(seq)
		if i == 0 {
			buf[0] |= 0x40 // first physical = last logical
		}
		buf[11] = attrLFN
		buf[12] = 0
		buf[13] = cksum
		// Characters for logical sequence seq are at runes[(seq-1)*13:(seq)*13].
		base    := (seq - 1) * 13
		chars   := runes[base : base+13]
		offsets := [13]int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
		for j, o := range offsets {
			c       := uint16(chars[j])
			buf[o]   = byte(c)
			buf[o+1] = byte(c >> 8)
		}
		entries[i] = buf
	}
	return entries
}

func lfnChecksum(sn [11]byte) byte {
	var sum byte
	for _, b := range sn {
		sum = (sum>>1) | (sum<<7)
		sum += b
	}
	return sum
}

// removeDirEntry marks the short entry for name in dirCluster as deleted (0xE5).
// Note: LFN entries preceding the short entry are not currently marked deleted;
// they are harmless (readers skip them) but waste directory space.
func (v *Volume) removeDirEntry(dirCluster uint32, name string) error {
	entries, err := v.readDirFromCluster(dirCluster)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.EqualFold(e.name, name) {
			continue
		}
		if e.dirCluster == 0 && v.b.fatType != fstype.FAT32 {
			// FAT12/16 fixed root dir.
			secSize   := int(v.b.bytesPerSec)
			targetSec := v.b.firstRootDir + e.dirOffset/uint32(secSize)
			inSecOff  := e.dirOffset % uint32(secSize)
			secBuf    := make([]byte, secSize)
			if _, serr := v.sr.ReadAt(secBuf, int64(targetSec)*int64(secSize)); serr != nil {
				return serr
			}
			secBuf[inSecOff] = 0xE5
			v.dirty[targetSec] = secBuf
		} else {
			clus, cerr := v.readCluster(e.dirCluster)
			if cerr != nil {
				return cerr
			}
			clus[e.dirOffset] = 0xE5
			v.writeCluster(e.dirCluster, clus)
		}
		return nil
	}
	return fmt.Errorf("fat32: %q not found in directory", name)
}

// ── WriteFile ─────────────────────────────────────────────────────────────────

func (v *Volume) WriteFile(name string, data []byte, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, err := v.lookupPath(name)
	if err != nil {
		if cerr := v.createFile(name, perm); cerr != nil {
			return cerr
		}
		e, err = v.lookupPath(name)
		if err != nil {
			return err
		}
	}
	return v.writeFileData(e, data)
}

// createFile adds an empty directory entry for name. Data is written
// separately via writeFileData so that dirOffset/dirCluster are always
// populated from a fresh lookupPath.
func (v *Volume) createFile(name string, perm fs.FileMode) error {
	dir, base := path.Split(path.Clean("/" + name))
	parent, err := v.lookupPath(dir)
	if err != nil {
		return err
	}
	return v.addDirEntry(parent.cluster, base, attrArchive, 0, 0)
}

// ── Create ────────────────────────────────────────────────────────────────────

func (v *Volume) Create(name string) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := v.createFile(name, 0644); err != nil {
		return nil, err
	}
	e, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	f := &fatFile{v: v, e: *e, data: []byte{}}
	return volfs.NewFile(f), nil
}

// ── OpenFile ──────────────────────────────────────────────────────────────────

func (v *Volume) OpenFile(name string, flag int, perm fs.FileMode) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	create := flag&os.O_CREATE != 0
	trunc  := flag&os.O_TRUNC != 0
	app    := flag&os.O_APPEND != 0

	e, err := v.lookupPath(name)
	if err != nil {
		if !create {
			return nil, err
		}
		if cerr := v.createFile(name, perm); cerr != nil {
			return nil, cerr
		}
		e, err = v.lookupPath(name)
		if err != nil {
			return nil, err
		}
	}

	var data []byte
	if trunc {
		data = []byte{}
	} else {
		data, err = v.readAllClusters(e)
		if err != nil {
			return nil, err
		}
	}

	offset := int64(0)
	if app {
		offset = int64(len(data))
	}

	f := &fatFile{v: v, e: *e, data: data, offset: offset}
	return volfs.NewFile(f), nil
}

// ── Mkdir / MkdirAll ──────────────────────────────────────────────────────────

func (v *Volume) Mkdir(name string, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mkdir(name)
}

func (v *Volume) mkdir(name string) error {
	dir, base := path.Split(path.Clean("/" + name))
	parent, err := v.lookupPath(dir)
	if err != nil {
		return err
	}
	newClus, err := v.allocCluster()
	if err != nil {
		return err
	}
	// Write . and .. entries.
	now  := time.Now()
	clus := make([]byte, v.b.clusterSize)
	dot := fatDirEntry{
		name:      ".",
		attr:      attrDirectory,
		cluster:   newClus,
		wrtDate:   fatDate(now),
		wrtTime:   fatTime(now),
	}
	copy(dot.shortName[:], ".          ")
	dotdot := fatDirEntry{
		name:      "..",
		attr:      attrDirectory,
		cluster:   parent.cluster,
		wrtDate:   fatDate(now),
		wrtTime:   fatTime(now),
	}
	copy(dotdot.shortName[:], "..         ")
	encodeDirEntry32(clus[0:32], &dot)
	encodeDirEntry32(clus[32:64], &dotdot)
	v.writeCluster(newClus, clus)
	return v.addDirEntry(parent.cluster, base, attrDirectory, newClus, 0)
}

func (v *Volume) MkdirAll(p string, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path.Clean("/"+p), "/"), "/") {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		if _, err := v.lookupPath(cur); err != nil {
			if merr := v.mkdir(cur); merr != nil {
				return merr
			}
		}
	}
	return nil
}

// ── Remove / RemoveAll ────────────────────────────────────────────────────────

func (v *Volume) Remove(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.remove(name, false)
}

func (v *Volume) remove(name string, allowDir bool) error {
	e, err := v.lookupPath(name)
	if err != nil {
		return err
	}
	if e.isDir() && !allowDir {
		return fmt.Errorf("fat32: %q is a directory", name)
	}
	dir, base := path.Split(path.Clean("/" + name))
	parent, err := v.lookupPath(dir)
	if err != nil {
		return err
	}
	if e.cluster >= 2 {
		v.freeChain(e.cluster)
	}
	return v.removeDirEntry(parent.cluster, base)
}

func (v *Volume) RemoveAll(p string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.removeAll(p)
}

func (v *Volume) removeAll(p string) error {
	e, err := v.lookupPath(p)
	if err != nil {
		return nil // already gone
	}
	if e.isDir() {
		children, err := v.readDirFromCluster(e.cluster)
		if err != nil {
			return err
		}
		for _, child := range children {
			if child.name == "." || child.name == ".." {
				continue
			}
			if err := v.removeAll(path.Join(p, child.name)); err != nil {
				return err
			}
		}
	}
	return v.remove(p, true)
}

// ── Rename ────────────────────────────────────────────────────────────────────

func (v *Volume) Rename(oldpath, newpath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, err := v.lookupPath(oldpath)
	if err != nil {
		return err
	}

	oldDir, oldBase := path.Split(path.Clean("/" + oldpath))
	newDir, newBase := path.Split(path.Clean("/" + newpath))

	oldParent, err := v.lookupPath(oldDir)
	if err != nil {
		return err
	}
	newParent, err := v.lookupPath(newDir)
	if err != nil {
		return err
	}

	if err := v.removeDirEntry(oldParent.cluster, oldBase); err != nil {
		return err
	}
	return v.addDirEntry(newParent.cluster, newBase, e.attr, e.cluster, e.size)
}

// ── Symlink / Link ────────────────────────────────────────────────────────────

func (v *Volume) Symlink(oldname, newname string) error {
	return fmt.Errorf("fat32: symlinks not supported")
}

func (v *Volume) Link(oldname, newname string) error {
	return fmt.Errorf("fat32: hard links not supported")
}