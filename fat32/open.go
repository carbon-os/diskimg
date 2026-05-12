// Package fat32 implements FAT12/16/32 filesystem read/write support.
package fat32

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	volfs "github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/fs/fstype"
)

// ── constants ────────────────────────────────────────────────────────────────

const (
	fatFREE = uint32(0x00000000)

	attrReadOnly  = uint8(0x01)
	attrHidden    = uint8(0x02)
	attrSystem    = uint8(0x04)
	attrVolumeID  = uint8(0x08)
	attrDirectory = uint8(0x10)
	attrArchive   = uint8(0x20)
	attrLFN       = uint8(0x0F)
)

// ── BPB ──────────────────────────────────────────────────────────────────────

type bpb struct {
	bytesPerSec uint16
	secPerClus  uint8
	rsvdSecCnt  uint16
	numFATs     uint8
	rootEntCnt  uint16 // FAT12/16 only; 0 for FAT32
	fatSz16     uint16
	fatSz32     uint32
	rootClus    uint32 // FAT32 root cluster
	totSec16    uint16
	totSec32    uint32

	// Derived.
	fatType      fstype.Type
	fatSz        uint32 // sectors per FAT copy
	firstDataSec uint32 // first sector of cluster 2
	cntClusters  uint32
	rootDirSec   uint32 // FAT12/16: sector count of fixed root dir
	firstRootDir uint32 // FAT12/16: first root-dir sector; FAT32: = rootClus
	clusterSize  uint32 // bytes per cluster
}

// ── fatDirEntry ───────────────────────────────────────────────────────────────

// fatDirEntry is a fully parsed in-memory directory entry.
// dirCluster is the specific cluster that physically holds the short entry
// (0 = FAT12/16 fixed root dir). dirOffset is the byte offset of the short
// entry within that cluster (or within the fixed root dir buffer for FAT12/16).
type fatDirEntry struct {
	name      string   // LFN if present, otherwise decoded 8.3
	shortName [11]byte // raw 8.3 name bytes
	attr      uint8
	cluster   uint32 // full 32-bit first data cluster
	size      uint32

	wrtDate uint16
	wrtTime uint16
	crtDate uint16
	crtTime uint16

	// Location — used by writeDirEntry.
	dirCluster uint32 // cluster that holds the short entry (0 = fixed root)
	dirOffset  uint32 // byte offset of the short entry within dirCluster
}

func (e *fatDirEntry) isDir() bool { return e.attr&attrDirectory != 0 }

// ── Volume ────────────────────────────────────────────────────────────────────

// Volume implements fs.Volume for a FAT filesystem.
type Volume struct {
	mu    sync.Mutex
	rw    io.WriterAt       // full-image writer — used by flush
	sr    *io.SectionReader // partition-scoped reader
	start int64             // byte offset of partition within the image file
	b     bpb
	ft    fstype.Type
	fat1  []byte            // FAT table 1, cached in memory
	dirty map[uint32][]byte // partition-relative sector number → sector data
}

// Open mounts the FAT partition at [start, start+size) inside rw.
// rw must implement both io.ReaderAt and io.WriterAt (e.g. *os.File).
// ft is the type hint from fstype.Detect; the final type is always re-derived
// from the cluster count per the Microsoft FAT specification.
func Open(rw interface {
	io.ReaderAt
	io.WriterAt
}, start, size int64, ft fstype.Type) (*Volume, error) {
	sr := io.NewSectionReader(rw, start, size)
	v := &Volume{
		rw:    rw,
		sr:    sr,
		start: start,
		ft:    ft,
		dirty: make(map[uint32][]byte),
	}
	if err := v.readBPB(); err != nil {
		return nil, err
	}
	if err := v.readFATTable(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *Volume) Type() fstype.Type { return v.ft }

// ── BPB ──────────────────────────────────────────────────────────────────────

func (v *Volume) readBPB() error {
	buf := make([]byte, 512)
	if _, err := v.sr.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("fat32: read boot sector: %w", err)
	}
	le := binary.LittleEndian
	b := &v.b

	b.bytesPerSec = le.Uint16(buf[11:13])
	b.secPerClus  = buf[13]
	b.rsvdSecCnt  = le.Uint16(buf[14:16])
	b.numFATs     = buf[16]
	b.rootEntCnt  = le.Uint16(buf[17:19])
	b.totSec16    = le.Uint16(buf[19:21])
	b.fatSz16     = le.Uint16(buf[22:24])
	b.fatSz32     = le.Uint32(buf[36:40])
	b.rootClus    = le.Uint32(buf[44:48])
	b.totSec32    = le.Uint32(buf[32:36])

	if b.bytesPerSec == 0 || b.secPerClus == 0 {
		return fmt.Errorf("fat32: invalid BPB: bytesPerSec=%d secPerClus=%d",
			b.bytesPerSec, b.secPerClus)
	}

	if b.fatSz16 != 0 {
		b.fatSz = uint32(b.fatSz16)
	} else {
		b.fatSz = b.fatSz32
	}
	totSec := uint32(b.totSec16)
	if totSec == 0 {
		totSec = b.totSec32
	}

	b.rootDirSec   = (uint32(b.rootEntCnt)*32 + uint32(b.bytesPerSec) - 1) / uint32(b.bytesPerSec)
	b.firstDataSec = uint32(b.rsvdSecCnt) + uint32(b.numFATs)*b.fatSz + b.rootDirSec
	dataSec        := totSec - b.firstDataSec
	b.cntClusters  = dataSec / uint32(b.secPerClus)
	b.clusterSize  = uint32(b.bytesPerSec) * uint32(b.secPerClus)

	switch {
	case b.cntClusters < 4085:
		b.fatType = fstype.FAT12
	case b.cntClusters < 65525:
		b.fatType = fstype.FAT16
	default:
		b.fatType = fstype.FAT32
	}
	v.ft = b.fatType

	if b.fatType == fstype.FAT32 {
		b.firstRootDir = b.rootClus
	} else {
		b.firstRootDir = uint32(b.rsvdSecCnt) + uint32(b.numFATs)*b.fatSz
	}
	return nil
}

// ── FAT table ────────────────────────────────────────────────────────────────

func (v *Volume) readFATTable() error {
	fatStart := int64(v.b.rsvdSecCnt) * int64(v.b.bytesPerSec)
	fatBytes := int64(v.b.fatSz) * int64(v.b.bytesPerSec)
	v.fat1 = make([]byte, fatBytes)
	_, err := v.sr.ReadAt(v.fat1, fatStart)
	return err
}

func (v *Volume) fatEntry(n uint32) uint32 {
	switch v.b.fatType {
	case fstype.FAT12:
		off := n + n/2
		w := binary.LittleEndian.Uint16(v.fat1[off : off+2])
		if n%2 == 0 {
			return uint32(w) & 0xFFF
		}
		return uint32(w) >> 4
	case fstype.FAT16:
		off := n * 2
		return uint32(binary.LittleEndian.Uint16(v.fat1[off : off+2]))
	default: // FAT32
		off := n * 4
		return binary.LittleEndian.Uint32(v.fat1[off:off+4]) & 0x0FFFFFFF
	}
}

func (v *Volume) setFATEntry(n, val uint32) {
	switch v.b.fatType {
	case fstype.FAT12:
		off := n + n/2
		w := binary.LittleEndian.Uint16(v.fat1[off : off+2])
		if n%2 == 0 {
			w = (w & 0xF000) | uint16(val&0xFFF)
		} else {
			w = (w & 0x000F) | uint16((val&0xFFF)<<4)
		}
		binary.LittleEndian.PutUint16(v.fat1[off:off+2], w)
	case fstype.FAT16:
		binary.LittleEndian.PutUint16(v.fat1[n*2:n*2+2], uint16(val))
	default: // FAT32
		existing := binary.LittleEndian.Uint32(v.fat1[n*4 : n*4+4])
		existing = (existing & 0xF0000000) | (val & 0x0FFFFFFF)
		binary.LittleEndian.PutUint32(v.fat1[n*4:n*4+4], existing)
	}
}

func (v *Volume) isEOC(val uint32) bool {
	switch v.b.fatType {
	case fstype.FAT12:
		return val >= 0xFF8
	case fstype.FAT16:
		return val >= 0xFFF8
	default:
		return val >= 0x0FFFFFF8
	}
}

func (v *Volume) eocValue() uint32 {
	switch v.b.fatType {
	case fstype.FAT12:
		return 0xFFF
	case fstype.FAT16:
		return 0xFFFF
	default:
		return 0x0FFFFFFF
	}
}

// ── cluster I/O ───────────────────────────────────────────────────────────────

// clusterByteOff returns the byte offset from partition start of cluster n.
func (v *Volume) clusterByteOff(n uint32) int64 {
	sec := int64(v.b.firstDataSec) + int64(n-2)*int64(v.b.secPerClus)
	return sec * int64(v.b.bytesPerSec)
}

// readCluster reads cluster n, overlaying any sectors that have been written
// to v.dirty but not yet flushed to disk. This ensures that writes made
// earlier in the same transaction are visible to subsequent reads without
// requiring an intervening flush.
func (v *Volume) readCluster(n uint32) ([]byte, error) {
	if n < 2 {
		return nil, fmt.Errorf("fat32: invalid cluster %d", n)
	}
	buf := make([]byte, v.b.clusterSize)
	if _, err := v.sr.ReadAt(buf, v.clusterByteOff(n)); err != nil {
		return nil, fmt.Errorf("fat32: read cluster %d: %w", n, err)
	}
	// Overlay dirty sectors so that unflushed writes are immediately visible.
	secSize  := int(v.b.bytesPerSec)
	firstSec := uint32(v.b.firstDataSec) + (n-2)*uint32(v.b.secPerClus)
	for i := 0; i < int(v.b.secPerClus); i++ {
		if data, ok := v.dirty[firstSec+uint32(i)]; ok {
			copy(buf[i*secSize:], data)
		}
	}
	return buf, nil
}

// writeCluster stages all sectors of cluster n into the dirty map.
func (v *Volume) writeCluster(n uint32, data []byte) {
	secSize  := int(v.b.bytesPerSec)
	firstSec := uint32(v.b.firstDataSec) + (n-2)*uint32(v.b.secPerClus)
	for i := 0; i < int(v.b.secPerClus); i++ {
		buf := make([]byte, secSize)
		copy(buf, data[i*secSize:(i+1)*secSize])
		v.dirty[firstSec+uint32(i)] = buf
	}
}

// ── StatFS ────────────────────────────────────────────────────────────────────

func (v *Volume) StatFS() (volfs.VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	var free uint32
	for n := uint32(2); n < v.b.cntClusters+2; n++ {
		if v.fatEntry(n) == fatFREE {
			free++
		}
	}
	total := v.b.cntClusters
	cs := int64(v.b.clusterSize)
	return volfs.VolumeInfo{
		TotalBytes: int64(total) * cs,
		FreeBytes:  int64(free) * cs,
		UsedBytes:  int64(total-free) * cs,
		BlockSize:  int64(v.b.bytesPerSec),
	}, nil
}

// ── directory reading ─────────────────────────────────────────────────────────

// readDirFromCluster reads all valid directory entries from a cluster chain.
// Pass firstCluster=0 for the FAT12/16 fixed root directory.
func (v *Volume) readDirFromCluster(firstCluster uint32) ([]fatDirEntry, error) {
	var entries []fatDirEntry
	var lfnParts []uint16
	var lfnSeq  int

	process := func(raw []byte, dirClus, byteOff uint32) bool {
		if raw[0] == 0x00 {
			return false // end of directory
		}
		if raw[0] == 0xE5 {
			lfnParts = nil
			lfnSeq   = 0
			return true // deleted
		}
		attr := raw[11]
		if attr == attrLFN {
			seq := int(raw[0] & 0x3F)
			if raw[0]&0x40 != 0 {
				lfnParts = make([]uint16, seq*13)
				lfnSeq   = seq
			}
			if seq > 0 && seq <= lfnSeq && lfnParts != nil {
				base    := (seq - 1) * 13
				offsets := [13]int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
				for i, o := range offsets {
					lfnParts[base+i] = binary.LittleEndian.Uint16(raw[o : o+2])
				}
			}
			return true
		}
		if attr&attrVolumeID != 0 {
			lfnParts = nil
			return true
		}

		var sn [11]byte
		copy(sn[:], raw[:11])
		de := fatDirEntry{
			shortName:  sn,
			attr:       attr,
			crtTime:    binary.LittleEndian.Uint16(raw[14:16]),
			crtDate:    binary.LittleEndian.Uint16(raw[16:18]),
			wrtTime:    binary.LittleEndian.Uint16(raw[22:24]),
			wrtDate:    binary.LittleEndian.Uint16(raw[24:26]),
			size:       binary.LittleEndian.Uint32(raw[28:32]),
			dirCluster: dirClus,
			dirOffset:  byteOff,
		}
		hi := binary.LittleEndian.Uint16(raw[20:22])
		lo := binary.LittleEndian.Uint16(raw[26:28])
		de.cluster = uint32(hi)<<16 | uint32(lo)

		if lfnParts != nil {
			runes := utf16Decode(lfnParts)
			name  := string(runes)
			if idx := strings.IndexFunc(name, func(r rune) bool {
				return r == 0x0000 || r == 0xFFFF
			}); idx >= 0 {
				name = name[:idx]
			}
			de.name   = name
			lfnParts = nil
			lfnSeq   = 0
		} else {
			de.name = decodeShortName(sn)
		}
		entries = append(entries, de)
		return true
	}

	// FAT12/16 fixed root directory: read sectors and overlay any dirty ones.
	if v.b.fatType != fstype.FAT32 && firstCluster == 0 {
		secSize  := int(v.b.bytesPerSec)
		rootSize := int64(v.b.rootEntCnt) * 32
		buf      := make([]byte, rootSize)
		sec      := int64(v.b.firstRootDir) * int64(secSize)
		if _, err := v.sr.ReadAt(buf, sec); err != nil {
			return nil, err
		}
		// Overlay dirty root-dir sectors.
		for i := uint32(0); i < v.b.rootDirSec; i++ {
			if data, ok := v.dirty[v.b.firstRootDir+i]; ok {
				copy(buf[int(i)*secSize:], data)
			}
		}
		for i := int64(0); i < rootSize; i += 32 {
			if !process(buf[i:i+32], 0, uint32(i)) {
				break
			}
		}
		return entries, nil
	}

	// FAT32 or subdirectory: walk cluster chain.
	cur := firstCluster
	for cur >= 2 && !v.isEOC(v.fatEntry(cur)) {
		clus, err := v.readCluster(cur)
		if err != nil {
			return nil, err
		}
		cont := true
		for i := uint32(0); i+32 <= uint32(len(clus)); i += 32 {
			if !process(clus[i:i+32], cur, i) {
				cont = false
				break
			}
		}
		if !cont {
			break
		}
		cur = v.fatEntry(cur)
	}
	// Read the final EOC cluster.
	if cur >= 2 && v.isEOC(v.fatEntry(cur)) {
		clus, err := v.readCluster(cur)
		if err != nil {
			return nil, err
		}
		for i := uint32(0); i+32 <= uint32(len(clus)); i += 32 {
			if !process(clus[i:i+32], cur, i) {
				break
			}
		}
	}
	return entries, nil
}

// ── path resolution ───────────────────────────────────────────────────────────

func (v *Volume) lookupPath(p string) (*fatDirEntry, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		clus := v.b.firstRootDir
		if v.b.fatType != fstype.FAT32 {
			clus = 0
		}
		return &fatDirEntry{
			name:    "/",
			attr:    attrDirectory,
			cluster: clus,
		}, nil
	}

	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")

	var curCluster uint32
	if v.b.fatType == fstype.FAT32 {
		curCluster = v.b.rootClus
	}

	var found *fatDirEntry
	for i, part := range parts {
		entries, err := v.readDirFromCluster(curCluster)
		if err != nil {
			return nil, err
		}
		var match *fatDirEntry
		for j := range entries {
			if strings.EqualFold(entries[j].name, part) {
				match = &entries[j]
				break
			}
		}
		if match == nil {
			return nil, fmt.Errorf("fat32: %q: no such file or directory", p)
		}
		if i == len(parts)-1 {
			found = match
		} else {
			if !match.isDir() {
				return nil, fmt.Errorf("fat32: %q: not a directory", part)
			}
			curCluster = match.cluster
		}
	}
	return found, nil
}

// ── ReadFile ──────────────────────────────────────────────────────────────────

func (v *Volume) ReadFile(name string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	if e.isDir() {
		return nil, fmt.Errorf("fat32: %q is a directory", name)
	}
	return v.readAllClusters(e)
}

func (v *Volume) readAllClusters(e *fatDirEntry) ([]byte, error) {
	if e.size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, e.size)
	off := 0
	cur := e.cluster
	for cur >= 2 && off < int(e.size) {
		clus, err := v.readCluster(cur)
		if err != nil {
			return nil, err
		}
		n := len(clus)
		if off+n > int(e.size) {
			n = int(e.size) - off
		}
		copy(buf[off:], clus[:n])
		off += n
		next := v.fatEntry(cur)
		if v.isEOC(next) {
			break
		}
		cur = next
	}
	return buf, nil
}

// ── Open ──────────────────────────────────────────────────────────────────────

func (v *Volume) Open(name string) (fs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	data, err := v.readAllClusters(e)
	if err != nil {
		return nil, err
	}
	return &fatFile{v: v, e: *e, data: data}, nil
}

// ── ReadDir ───────────────────────────────────────────────────────────────────

func (v *Volume) ReadDir(name string) ([]fs.DirEntry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	if !e.isDir() {
		return nil, fmt.Errorf("fat32: %q is not a directory", name)
	}
	raw, err := v.readDirFromCluster(e.cluster)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, de := range raw {
		if de.name == "." || de.name == ".." {
			continue
		}
		cpy := de
		out = append(out, &fatDirEntryFS{e: cpy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// ── Stat / Lstat ──────────────────────────────────────────────────────────────

func (v *Volume) Stat(name string) (fs.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	return &fatFileInfo{name: pathBase(name), e: *e}, nil
}

func (v *Volume) Lstat(name string) (fs.FileInfo, error) {
	return v.Stat(name) // FAT has no symlinks
}

func (v *Volume) Readlink(name string) (string, error) {
	return "", fmt.Errorf("fat32: symlinks not supported")
}

// ── fs.FileInfo / DirEntry ────────────────────────────────────────────────────

type fatFileInfo struct {
	name string
	e    fatDirEntry
}

func (fi *fatFileInfo) Name() string { return fi.name }
func (fi *fatFileInfo) Size() int64  { return int64(fi.e.size) }
func (fi *fatFileInfo) Mode() fs.FileMode {
	if fi.e.isDir() {
		return fs.ModeDir | 0755
	}
	if fi.e.attr&attrReadOnly != 0 {
		return 0444
	}
	return 0644
}
func (fi *fatFileInfo) ModTime() time.Time { return fatToTime(fi.e.wrtDate, fi.e.wrtTime) }
func (fi *fatFileInfo) IsDir() bool        { return fi.e.isDir() }
func (fi *fatFileInfo) Sys() any           { return &fi.e }

type fatDirEntryFS struct{ e fatDirEntry }

func (d *fatDirEntryFS) Name() string      { return d.e.name }
func (d *fatDirEntryFS) IsDir() bool       { return d.e.isDir() }
func (d *fatDirEntryFS) Type() fs.FileMode { if d.e.isDir() { return fs.ModeDir }; return 0 }
func (d *fatDirEntryFS) Info() (fs.FileInfo, error) {
	return &fatFileInfo{name: d.e.name, e: d.e}, nil
}

// ── fatFile ───────────────────────────────────────────────────────────────────

type fatFile struct {
	v      *Volume
	e      fatDirEntry
	data   []byte
	offset int64
	dirty  bool
}

func (f *fatFile) Stat() (fs.FileInfo, error) {
	return &fatFileInfo{name: f.e.name, e: f.e}, nil
}

func (f *fatFile) Read(p []byte) (int, error) {
	if f.offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *fatFile) Write(p []byte) (int, error) {
	end := f.offset + int64(len(p))
	if end > int64(len(f.data)) {
		grown := make([]byte, end)
		copy(grown, f.data)
		f.data = grown
	}
	n := copy(f.data[f.offset:], p)
	f.offset += int64(n)
	f.dirty = true
	return n, nil
}

func (f *fatFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.offset = offset
	case io.SeekCurrent:
		f.offset += offset
	case io.SeekEnd:
		f.offset = int64(len(f.data)) + offset
	}
	if f.offset < 0 {
		f.offset = 0
	}
	return f.offset, nil
}

func (f *fatFile) Close() error {
	if !f.dirty {
		return nil
	}
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	return f.v.writeFileData(&f.e, f.data)
}

func (f *fatFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !f.e.isDir() {
		return nil, fmt.Errorf("fat32: not a directory")
	}
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	entries, err := f.v.readDirFromCluster(f.e.cluster)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, de := range entries {
		if de.name == "." || de.name == ".." {
			continue
		}
		cpy := de
		out = append(out, &fatDirEntryFS{e: cpy})
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func pathBase(p string) string {
	p = path.Clean(p)
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// utf16Decode wraps unicode/utf16.Decode without importing the package at
// the call site — keeps the import list clean since utf16 is only needed here.
func utf16Decode(s []uint16) []rune {
	return []rune(string(utf16.Decode(s)))
}