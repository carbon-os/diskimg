package fat32

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/carbon-os/diskimg/fs/fstype"
)

// ── directory reading ─────────────────────────────────────────────────────────

// readDirFromCluster reads all valid directory entries from a cluster chain.
// Pass firstCluster=0 for the FAT12/16 fixed root directory.
func (v *Volume) readDirFromCluster(firstCluster uint32) ([]fatDirEntry, error) {
	var entries []fatDirEntry
	var lfnParts []uint16
	var lfnSeq  int

	process := func(raw []byte, dirClus, byteOff uint32) bool {
		if raw[0] == 0x00 {
			return false
		}
		if raw[0] == 0xE5 {
			lfnParts = nil
			lfnSeq   = 0
			return true
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
			runes := utf16.Decode(lfnParts)
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

	// FAT12/16 fixed root directory.
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
			clus = 0 // sentinel: use fixed root-dir sector path
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
	// For FAT12/16 curCluster stays 0; readDirFromCluster(0) handles the fixed root.

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

// fatFile is the handle returned by Open, Create, and OpenFile.
// FAT has no kernel page cache, so we buffer the entire file in memory;
// dirty writes are flushed back on Close.
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