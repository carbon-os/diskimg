package xfs

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	volfs "github.com/carbon-os/diskimg/fs"
)

// ── file write ────────────────────────────────────────────────────────────────

// linearToFsb converts an absolute linear block number back to an xfs_fsblock_t.
func (v *Volume) linearToFsb(blk uint64) uint64 {
	ag := blk / uint64(v.sb.agBlocks)
	agbno := blk % uint64(v.sb.agBlocks)
	return (ag << uint64(v.sb.agBlkLog)) | agbno
}

// packBmbt encodes a single bmbt extent record from its four fields.
func packBmbt(logOff, fsb uint64, count uint32, unwritten bool) bmbtRec {
	var r bmbtRec
	// l0: flag(1) + logOff(54) + fsb_hi(9)
	r.l0 = (logOff << 9) | (fsb >> 43)
	if unwritten {
		r.l0 |= 1 << 63
	}
	// l1: fsb_lo(43) + count(21)
	r.l1 = (fsb & ((1 << 43) - 1)) << 21 | uint64(count&0x1FFFFF)
	return r
}

// writeFileRange writes data into a file at byteOffset, extending as needed.
func (v *Volume) writeFileRange(ino uint64, in *inode, byteOffset int64, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Transition from local to extent format if needed.
	if in.format == fmtLocal {
		if err := v.promoteLocalToExtents(ino, in); err != nil {
			return 0, err
		}
	}

	bs := int64(v.sb.blockSize)
	total := 0
	off := byteOffset

	recs, err := v.extentList(in)
	if err != nil {
		return 0, err
	}

	for len(data) > 0 {
		logBlk   := uint64(off / bs)
		inBlkOff := off % bs

		physBlk := v.logicalToPhysical(recs, logBlk)
		var blockData []byte
		if physBlk == 0 {
			// Allocate a new block.
			physBlk, err = v.allocBlock(ino)
			if err != nil {
				return total, err
			}
			blockData = make([]byte, bs)
			// Convert to structured FSB before packing the BMBT record
			rec := packBmbt(logBlk, v.linearToFsb(physBlk), 1, false)
			recs = append(recs, rec)
			in.nextents++
			if err := v.storeExtents(ino, in, recs); err != nil {
				return total, err
			}
		} else {
			blockData, err = v.readBlock(physBlk)
			if err != nil {
				return total, err
			}
		}

		canWrite := bs - inBlkOff
		if int64(len(data)) < canWrite {
			canWrite = int64(len(data))
		}
		copy(blockData[inBlkOff:], data[:canWrite])
		v.writeBlock(physBlk, blockData)
		data = data[canWrite:]
		off += canWrite
		total += int(canWrite)
	}

	newSize := byteOffset + int64(total)
	if newSize > in.size {
		in.size = newSize
		in.mtime = nowSec()
		in.ctime = nowSec()
		if err := v.writeInode(ino, in); err != nil {
			return total, err
		}
	}
	return total, nil
}

// promoteLocalToExtents converts an inline (local) inode to extent format.
func (v *Volume) promoteLocalToExtents(ino uint64, in *inode) error {
	if in.size == 0 {
		in.format = fmtExtents
		in.nextents = 0
		in.litSize = int(v.sb.inodeSize) - in.coreSize()
		return v.writeInode(ino, in)
	}

	// Allocate a block and copy inline data into it.
	physBlk, err := v.allocBlock(ino)
	if err != nil {
		return err
	}
	blk := make([]byte, v.sb.blockSize)
	copy(blk, in.literal[:in.size])
	v.writeBlock(physBlk, blk)

	// Store a single extent record, converting linear to FSB
	rec := packBmbt(0, v.linearToFsb(physBlk), 1, false)
	be := binary.BigEndian
	in.format = fmtExtents
	in.nextents = 1
	in.litSize = int(v.sb.inodeSize) - in.coreSize()
	be.PutUint64(in.literal[0:8], rec.l0)
	be.PutUint64(in.literal[8:16], rec.l1)
	return v.writeInode(ino, in)
}

// storeExtents writes an updated extent list back into the inode's literal area.
// Switches to B+tree if the list no longer fits (basic two-level promotion).
func (v *Volume) storeExtents(ino uint64, in *inode, recs []bmbtRec) error {
	maxInline := in.litSize / 16
	if len(recs) <= maxInline {
		in.format = fmtExtents
		in.nextents = uint32(len(recs))
		for i, r := range recs {
			binary.BigEndian.PutUint64(in.literal[i*16:], r.l0)
			binary.BigEndian.PutUint64(in.literal[i*16+8:], r.l1)
		}
		return v.writeInode(ino, in)
	}
	// Overflow: spill to a leaf block, store a minimal B+tree root.
	return v.spillExtentsToBtree(ino, in, recs)
}

// spillExtentsToBtree allocates a leaf block for extents and rewrites the inode
// with a depth-1 B+tree root.
func (v *Volume) spillExtentsToBtree(ino uint64, in *inode, recs []bmbtRec) error {
	leafBlk, err := v.allocBlock(ino)
	if err != nil {
		return err
	}
	bs := int(v.sb.blockSize)
	leafData := make([]byte, bs)
	be := binary.BigEndian

	hdrSize := 16
	if v.sb.hasV5CRC {
		hdrSize = 72
	}
	maxRecs := (bs - hdrSize) / 16

	// Write leaf block header.
	if v.sb.hasV5CRC {
		be.PutUint32(leafData[0:4], bmbtMagicLeaf)
	} else {
		be.PutUint32(leafData[0:4], bmbtMagicLeafV4)
	}
	be.PutUint16(leafData[4:6], 0) // level = 0 (leaf)
	n := len(recs)
	if n > maxRecs {
		n = maxRecs
	}
	be.PutUint16(leafData[6:8], uint16(n))
	for i := 0; i < n; i++ {
		off := hdrSize + i*16
		be.PutUint64(leafData[off:], recs[i].l0)
		be.PutUint64(leafData[off+8:], recs[i].l1)
	}
	v.writeBlock(leafBlk, leafData)

	// Write B+tree root into inode literal area (depth=1, 1 key + 1 ptr).
	be.PutUint32(in.literal[0:4], bmbtMagicNode)
	be.PutUint16(in.literal[4:6], 1) // level = 1
	be.PutUint16(in.literal[6:8], 1) // numrecs = 1
	// key: logical block 0
	be.PutUint64(in.literal[8:16], 0)
	
	rootHdr := 16
	if v.sb.hasV5CRC {
		rootHdr = 72
	}
	
	// Convert linear block to FSB for the child pointer
	be.PutUint64(in.literal[rootHdr:rootHdr+8], v.linearToFsb(leafBlk))
	in.format = fmtBtree
	in.nextents = uint32(n)
	return v.writeInode(ino, in)
}

// ── directory modification ────────────────────────────────────────────────────

// addDirEntry appends name→childIno to directory dirIno.
// For short-form directories: appends to the inline sf structure.
// For extent-format directories: appends to the block-format data.
func (v *Volume) addDirEntry(dirIno uint64, name string, childIno uint64, ft uint8) error {
	dirIn, err := v.readInode(dirIno)
	if err != nil {
		return err
	}
	if dirIn.format == fmtLocal {
		return v.addDirEntryShortForm(dirIno, &dirIn, name, childIno, ft)
	}
	return v.addDirEntryBlock(dirIno, &dirIn, name, childIno, ft)
}

// addDirEntryShortForm appends a new entry to a short-form directory.
func (v *Volume) addDirEntryShortForm(dirIno uint64, dirIn *inode, name string, childIno uint64, ft uint8) error {
	raw := dirIn.literal[:dirIn.litSize]
	hasFType := v.sb.featIncompat&featIncompatFType != 0

	// Entry size: 1(nameLen) + 2(offset) + len(name) + 8(ino) + 1(ftype)
	entSize := 1 + 2 + len(name) + 8
	if hasFType {
		entSize++
	}

	end := int(dirIn.size) // current sf size (excluding inode header)
	if end+entSize > dirIn.litSize {
		// Promote to extent format.
		if err := v.promoteDirToExtents(dirIno, dirIn); err != nil {
			return err
		}
		return v.addDirEntryBlock(dirIno, dirIn, name, childIno, ft)
	}

	be := binary.BigEndian
	pos := end
	raw[pos] = byte(len(name))
	pos++
	be.PutUint16(raw[pos:pos+2], 0) // offset placeholder BEFORE name
	pos += 2
	copy(raw[pos:], name)
	pos += len(name)
	be.PutUint64(raw[pos:pos+8], childIno) // ino BEFORE ftype
	pos += 8
	if hasFType {
		raw[pos] = ft
		pos++
	}
	raw[0]++ // increment count
	dirIn.size = int64(pos)
	return v.writeInode(dirIno, dirIn)
}

// promoteDirToExtents converts a local (sf) directory to extent format by
// allocating a directory block and rewriting its entries.
func (v *Volume) promoteDirToExtents(dirIno uint64, dirIn *inode) error {
	// Read existing entries before we clobber the literal area.
	entries, err := v.readDirShortForm(dirIn)
	if err != nil {
		return err
	}

	physBlk, err := v.allocBlock(dirIno)
	if err != nil {
		return err
	}
	blkData := make([]byte, v.sb.blockSize)
	hasFType := v.sb.featIncompat&featIncompatFType != 0

	hdrSize := 16
	if v.sb.hasV5CRC {
		hdrSize = 64
	}
	pos := hdrSize

	be := binary.BigEndian
	for _, e := range entries {
		if e.name == "." || e.name == ".." {
			continue
		}
		be.PutUint64(blkData[pos:], e.ino)
		pos += 8
		blkData[pos] = byte(len(e.name))
		pos++
		copy(blkData[pos:], e.name)
		pos += len(e.name)
		if hasFType {
			blkData[pos] = e.fileType
			pos++
		}
		pos = (pos + 1) &^ 1
	}
	v.writeBlock(physBlk, blkData)

	// Convert linear block to structured FSB
	rec := packBmbt(0, v.linearToFsb(physBlk), 1, false)
	dirIn.format = fmtExtents
	dirIn.nextents = 1
	dirIn.litSize = int(v.sb.inodeSize) - dirIn.coreSize()
	be.PutUint64(dirIn.literal[0:8], rec.l0)
	be.PutUint64(dirIn.literal[8:16], rec.l1)
	dirIn.size = int64(v.sb.blockSize)
	return v.writeInode(dirIno, dirIn)
}

// addDirEntryBlock appends an entry to an extent-format directory block.
func (v *Volume) addDirEntryBlock(dirIno uint64, dirIn *inode, name string, childIno uint64, ft uint8) error {
	recs, err := v.extentList(dirIn)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("xfs: dir has no extents")
	}

	// Use last extent's first block.
	last := recs[len(recs)-1]
	physBlk := v.fsbToLinear(last.startBlock())
	blkData, err := v.readBlock(physBlk)
	if err != nil {
		return err
	}

	hasFType := v.sb.featIncompat&featIncompatFType != 0
	hdrSize := 16
	if v.sb.hasV5CRC {
		hdrSize = 64
	}

	// Find end of entries (scan for first zero ino after header).
	be := binary.BigEndian
	pos := hdrSize
	for pos+8 < len(blkData) {
		ino := be.Uint64(blkData[pos : pos+8])
		if ino == 0 {
			break
		}
		pos += 8
		nameLen := int(blkData[pos])
		pos++
		pos += nameLen
		if hasFType {
			pos++
		}
		pos = (pos + 1) &^ 1
	}

	// Write new entry.
	be.PutUint64(blkData[pos:], childIno)
	pos += 8
	blkData[pos] = byte(len(name))
	pos++
	copy(blkData[pos:], name)
	pos += len(name)
	if hasFType {
		blkData[pos] = ft
		pos++
	}
	pos = (pos + 1) &^ 1

	v.writeBlock(physBlk, blkData)
	dirIn.mtime = nowSec()
	dirIn.ctime = nowSec()
	return v.writeInode(dirIno, dirIn)
}

// removeDirEntry zeroes the entry with the given name from a directory.
func (v *Volume) removeDirEntry(dirIno uint64, name string) error {
	dirIn, err := v.readInode(dirIno)
	if err != nil {
		return err
	}
	if dirIn.format == fmtLocal {
		return v.removeDirEntryShortForm(dirIno, &dirIn, name)
	}
	return v.removeDirEntryBlock(dirIno, &dirIn, name)
}

func (v *Volume) removeDirEntryShortForm(dirIno uint64, dirIn *inode, name string) error {
	entries, err := v.readDirShortForm(dirIn)
	if err != nil {
		return err
	}
	found := false
	for _, e := range entries {
		if e.name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("xfs: %q: not found in directory", name)
	}
	// Rebuild short-form without the entry.
	keep := entries[:0]
	for _, e := range entries {
		if e.name != name {
			keep = append(keep, e)
		}
	}
	return v.rebuildShortFormDir(dirIno, dirIn, keep)
}

func (v *Volume) rebuildShortFormDir(dirIno uint64, dirIn *inode, entries []dirEntry) error {
	hasFType := v.sb.featIncompat&featIncompatFType != 0
	be := binary.BigEndian
	raw := dirIn.literal[:]
	raw[0] = byte(len(entries))
	
	// Set i8count to 1 since we are unconditionally writing 64-bit inodes
	raw[1] = 1 
	be.PutUint64(raw[2:10], v.sb.rootIno) // 8-byte parent
	pos := 10
	
	for _, e := range entries {
		raw[pos] = byte(len(e.name))
		pos++
		be.PutUint16(raw[pos:pos+2], 0) // offset BEFORE name
		pos += 2
		copy(raw[pos:], e.name)
		pos += len(e.name)
		be.PutUint64(raw[pos:pos+8], e.ino) // ino BEFORE ftype
		pos += 8
		if hasFType {
			raw[pos] = e.fileType
			pos++
		}
	}
	dirIn.size = int64(pos)
	return v.writeInode(dirIno, dirIn)
}

func (v *Volume) removeDirEntryBlock(dirIno uint64, dirIn *inode, name string) error {
	recs, err := v.extentList(dirIn)
	if err != nil {
		return err
	}
	hasFType := v.sb.featIncompat&featIncompatFType != 0
	hdrSize := 16
	if v.sb.hasV5CRC {
		hdrSize = 64
	}
	be := binary.BigEndian

	for _, r := range recs {
		cnt := uint64(r.blockCount())
		for blkIdx := uint64(0); blkIdx < cnt; blkIdx++ {
			physBlk := v.fsbToLinear(r.startBlock() + blkIdx)
			blkData, err := v.readBlock(physBlk)
			if err != nil {
				return err
			}
			pos := hdrSize
			modified := false
			for pos+8 < len(blkData) {
				ino := be.Uint64(blkData[pos : pos+8])
				if ino == 0 {
					break
				}
				entStart := pos
				pos += 8
				nameLen := int(blkData[pos])
				pos++
				entName := string(blkData[pos : pos+nameLen])
				pos += nameLen
				if hasFType {
					pos++
				}
				pos = (pos + 1) &^ 1
				if entName == name {
					// Zero out the ino to mark deleted.
					be.PutUint64(blkData[entStart:entStart+8], 0)
					modified = true
					break
				}
			}
			if modified {
				v.writeBlock(physBlk, blkData)
				dirIn.mtime = nowSec()
				dirIn.ctime = nowSec()
				return v.writeInode(dirIno, dirIn)
			}
		}
	}
	return fmt.Errorf("xfs: %q: not found in directory", name)
}

// ── Volume.WriteFile ──────────────────────────────────────────────────────────

func (v *Volume) WriteFile(name string, data []byte, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPath(name)
	if err != nil {
		return v.createFile(name, data, perm)
	}
	in, err := v.readInode(ino)
	if err != nil {
		return err
	}
	in.size = 0
	in.mtime = nowSec()
	if err := v.writeInode(ino, &in); err != nil {
		return err
	}
	if len(data) > 0 {
		_, err = v.writeFileRange(ino, &in, 0, data)
	}
	return err
}

func (v *Volume) createFile(name string, data []byte, perm fs.FileMode) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return fmt.Errorf("xfs: create %q: parent: %w", name, err)
	}

	newIno, err := v.allocInode(dirIno)
	if err != nil {
		return err
	}
	now := nowSec()
	newIn := inode{
		magic:   inodeMagic,
		mode:    uint16(modeReg) | uint16(perm&0x1FF),
		version: 3,
		format:  fmtExtents,
		uid:     0,
		gid:     0,
		nlink:   1,
		mtime:   now,
		atime:   now,
		ctime:   now,
		btime:   now,
		inum:    newIno,
		litSize: int(v.sb.inodeSize) - 176,
	}
	if err := v.writeInode(newIno, &newIn); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := v.writeFileRange(newIno, &newIn, 0, data); err != nil {
			return err
		}
	}
	return v.addDirEntry(dirIno, base, newIno, ftReg)
}

// ── Volume.Create ─────────────────────────────────────────────────────────────

func (v *Volume) Create(name string) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := v.createFile(name, nil, 0644); err != nil {
		ino, lerr := v.lookupPath(name)
		if lerr != nil {
			return nil, err
		}
		in, _ := v.readInode(ino)
		in.size = 0
		_ = v.writeInode(ino, &in)
	}
	ino, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return nil, err
	}
	f := &xfsFile{v: v, ino: ino, in: in, writable: true}
	return volfs.NewFile(f), nil
}

// ── Volume.OpenFile ───────────────────────────────────────────────────────────

func (v *Volume) OpenFile(name string, flag int, perm fs.FileMode) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	writable    := flag&(os.O_WRONLY|os.O_RDWR) != 0
	create      := flag&os.O_CREATE != 0
	trunc       := flag&os.O_TRUNC != 0
	excl        := flag&os.O_EXCL != 0
	appendMode  := flag&os.O_APPEND != 0

	ino, err := v.lookupPath(name)
	if err != nil {
		if !create {
			return nil, err
		}
		if e2 := v.createFile(name, nil, perm); e2 != nil {
			return nil, e2
		}
		ino, _ = v.lookupPath(name)
	} else if excl {
		return nil, fmt.Errorf("xfs: %q already exists", name)
	}

	in, err := v.readInode(ino)
	if err != nil {
		return nil, err
	}
	offset := int64(0)
	if trunc && writable {
		in.size = 0
		in.mtime = nowSec()
		_ = v.writeInode(ino, &in)
	}
	if appendMode {
		offset = in.size
	}
	f := &xfsFile{v: v, ino: ino, in: in, offset: offset, writable: writable}
	return volfs.NewFile(f), nil
}

// ── Volume.Mkdir / MkdirAll ───────────────────────────────────────────────────

func (v *Volume) Mkdir(name string, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mkdir(name, perm)
}

func (v *Volume) mkdir(name string, perm fs.FileMode) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return fmt.Errorf("xfs: mkdir %q: parent: %w", name, err)
	}

	newIno, err := v.allocInode(dirIno)
	if err != nil {
		return err
	}
	now := nowSec()
	newIn := inode{
		magic:   inodeMagic,
		mode:    uint16(modeDir) | uint16(perm&0x1FF),
		version: 3,
		format:  fmtLocal,
		nlink:   2,
		mtime:   now,
		atime:   now,
		ctime:   now,
		btime:   now,
		inum:    newIno,
		litSize: int(v.sb.inodeSize) - 176,
	}
	// Initialise a minimal short-form directory (count=0, parent=dirIno).
	binary.BigEndian.PutUint64(newIn.literal[2:10], dirIno)
	newIn.size = 10 // 2 byte header + 8 byte parent ino
	if err := v.writeInode(newIno, &newIn); err != nil {
		return err
	}
	return v.addDirEntry(dirIno, base, newIno, ftDir)
}

func (v *Volume) MkdirAll(p string, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	p = path.Clean("/" + p)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := "/"
	for _, part := range parts {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		if _, err := v.lookupPath(cur); err != nil {
			if e2 := v.mkdir(cur, perm); e2 != nil {
				return e2
			}
		}
	}
	return nil
}

// ── Volume.Remove / RemoveAll ─────────────────────────────────────────────────

func (v *Volume) Remove(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.remove(name, false)
}

func (v *Volume) remove(name string, allowDir bool) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return err
	}
	ino, err := v.lookupPath(name)
	if err != nil {
		return err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return err
	}
	if in.mode&modeFmt == modeDir && !allowDir {
		return fmt.Errorf("xfs: %q is a directory", name)
	}
	if err := v.removeDirEntry(dirIno, base); err != nil {
		return err
	}
	in.nlink--
	in.ctime = nowSec()
	if in.nlink == 0 {
		v.freeInode(ino)
	}
	return v.writeInode(ino, &in)
}

func (v *Volume) RemoveAll(p string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPath(p)
	if err != nil {
		return nil
	}
	in, err := v.readInode(ino)
	if err != nil {
		return err
	}
	if in.mode&modeFmt == modeDir {
		entries, err := v.readDir(&in)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.name == "." || e.name == ".." {
				continue
			}
			if err := v.RemoveAll(path.Join(p, e.name)); err != nil {
				return err
			}
		}
	}
	return v.remove(p, true)
}

// ── Volume.Rename ─────────────────────────────────────────────────────────────

func (v *Volume) Rename(oldpath, newpath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	oldDir, oldBase := path.Split(path.Clean("/" + oldpath))
	newDir, newBase := path.Split(path.Clean("/" + newpath))

	oldDirIno, err := v.lookupPathFollow(oldDir, 0)
	if err != nil {
		return err
	}
	newDirIno, err := v.lookupPathFollow(newDir, 0)
	if err != nil {
		return err
	}
	targetIno, err := v.lookupPath(oldpath)
	if err != nil {
		return err
	}
	in, err := v.readInode(targetIno)
	if err != nil {
		return err
	}
	ft := ftReg
	if in.mode&modeFmt == modeDir {
		ft = ftDir
	} else if in.mode&modeFmt == modeLnk {
		ft = ftSymlink
	}

	// Remove destination if it exists.
	if _, err := v.lookupPath(newpath); err == nil {
		_ = v.removeDirEntry(newDirIno, newBase)
	}
	if err := v.addDirEntry(newDirIno, newBase, targetIno, uint8(ft)); err != nil {
		return err
	}
	if err := v.removeDirEntry(oldDirIno, oldBase); err != nil {
		return err
	}
	in.ctime = nowSec()
	return v.writeInode(targetIno, &in)
}

// ── Volume.Symlink / Link ─────────────────────────────────────────────────────

func (v *Volume) Symlink(oldname, newname string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	dir, base := path.Split(path.Clean("/" + newname))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return err
	}
	newIno, err := v.allocInode(dirIno)
	if err != nil {
		return err
	}
	now := nowSec()
	newIn := inode{
		magic:   inodeMagic,
		mode:    uint16(modeLnk | 0777),
		version: 3,
		nlink:   1,
		mtime:   now,
		atime:   now,
		ctime:   now,
		btime:   now,
		inum:    newIno,
		litSize: int(v.sb.inodeSize) - 176,
	}
	target := []byte(oldname)
	if len(target) <= newIn.litSize {
		newIn.format = fmtLocal
		copy(newIn.literal[:], target)
		newIn.size = int64(len(target))
		if err := v.writeInode(newIno, &newIn); err != nil {
			return err
		}
	} else {
		newIn.format = fmtExtents
		if err := v.writeInode(newIno, &newIn); err != nil {
			return err
		}
		if _, err := v.writeFileRange(newIno, &newIn, 0, target); err != nil {
			return err
		}
	}
	return v.addDirEntry(dirIno, base, newIno, ftSymlink)
}

func (v *Volume) Link(oldname, newname string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPath(oldname)
	if err != nil {
		return err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return err
	}
	if in.mode&modeFmt == modeDir {
		return fmt.Errorf("xfs: link: cannot hard-link directories")
	}
	newDir, newBase := path.Split(path.Clean("/" + newname))
	newDirIno, err := v.lookupPathFollow(newDir, 0)
	if err != nil {
		return err
	}
	ft := ftReg
	if in.mode&modeFmt == modeLnk {
		ft = ftSymlink
	}
	if err := v.addDirEntry(newDirIno, newBase, ino, uint8(ft)); err != nil {
		return err
	}
	in.nlink++
	in.ctime = nowSec()
	return v.writeInode(ino, &in)
}