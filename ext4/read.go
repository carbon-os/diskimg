package ext4

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// ── extent tree traversal ─────────────────────────────────────────────────────

// parseExtentHeader decodes the 12-byte extent header from b.
func parseExtentHeader(b []byte) extentHeader {
	le := binary.LittleEndian
	return extentHeader{
		magic:      le.Uint16(b[0:2]),
		entries:    le.Uint16(b[2:4]),
		max:        le.Uint16(b[4:6]),
		depth:      le.Uint16(b[6:8]),
		generation: le.Uint32(b[8:12]),
	}
}

// parseExtentIdx decodes the 12-byte index node from b.
func parseExtentIdx(b []byte) extentIdx {
	le := binary.LittleEndian
	return extentIdx{
		block:  le.Uint32(b[0:4]),
		leafLo: le.Uint32(b[4:8]),
		leafHi: le.Uint16(b[8:10]),
	}
}

// parseExtentLeaf decodes the 12-byte leaf from b.
func parseExtentLeaf(b []byte) extentLeaf {
	le := binary.LittleEndian
	return extentLeaf{
		block:   le.Uint32(b[0:4]),
		len:     le.Uint16(b[4:6]),
		startHi: le.Uint16(b[6:8]),
		startLo: le.Uint32(b[8:12]),
	}
}

// physBlock returns the full 48-bit physical block address of a leaf.
func (e *extentLeaf) physBlock() uint64 {
	return uint64(e.startLo) | uint64(e.startHi)<<32
}

// numBlocks returns the actual number of blocks in the extent
// (strips the initialized/uninitialized bit).
func (e *extentLeaf) numBlocks() uint16 {
	return e.len & 0x7FFF
}

// logicalToPhysical maps a logical file block to a physical block via the
// extent tree rooted in iBlock.  Returns 0 for holes (unallocated ranges).
func (v *Volume) logicalToPhysical(in *inode, logicalBlock uint32) (uint64, error) {
	if in.flags&inodeFlagExtents == 0 {
		return 0, fmt.Errorf("ext4: inode uses legacy block map (not supported)")
	}
	return v.searchExtentNode(in.iBlock[:], logicalBlock, true)
}

// searchExtentNode recursively descends the extent tree.
// If inline is true, the node is stored in iBlock; otherwise read from block physNode.
func (v *Volume) searchExtentNode(raw []byte, logBlk uint32, inline bool) (uint64, error) {
	hdr := parseExtentHeader(raw)
	if hdr.magic != extentMagic {
		return 0, fmt.Errorf("ext4: extent magic 0x%04X (want 0x%04X)", hdr.magic, extentMagic)
	}

	if hdr.depth == 0 {
		// Leaf node: linear search through ext4_extent entries.
		for i := uint16(0); i < hdr.entries; i++ {
			off := 12 + int(i)*12
			leaf := parseExtentLeaf(raw[off:])
			first := leaf.block
			last := first + uint32(leaf.numBlocks()) - 1
			if logBlk >= first && logBlk <= last {
				offset := logBlk - first
				return leaf.physBlock() + uint64(offset), nil
			}
		}
		return 0, nil // hole
	}

	// Internal node: find the right child to descend into.
	// The last index whose block ≤ logBlk is the correct child.
	childBlock := uint64(0)
	found := false
	for i := uint16(0); i < hdr.entries; i++ {
		off := 12 + int(i)*12
		idx := parseExtentIdx(raw[off:])
		if idx.block <= logBlk {
			childBlock = uint64(idx.leafLo) | uint64(idx.leafHi)<<32
			found = true
		} else {
			break
		}
	}
	if !found {
		return 0, nil // hole
	}

	childData, err := v.readBlock(childBlock)
	if err != nil {
		return 0, err
	}
	return v.searchExtentNode(childData, logBlk, false)
}

// readFileRange reads len(buf) bytes starting at byteOffset from an inode.
func (v *Volume) readFileRange(in *inode, byteOffset int64, buf []byte) (int, error) {
	fileSize := inodeFileSize(in)
	if byteOffset >= fileSize {
		return 0, io.EOF
	}
	if int64(len(buf)) > fileSize-byteOffset {
		buf = buf[:fileSize-byteOffset]
	}

	bs := int64(v.sb.blockSize)
	total := 0
	for len(buf) > 0 {
		logBlk := uint32(byteOffset / bs)
		inBlockOff := byteOffset % bs

		physBlk, err := v.logicalToPhysical(in, logBlk)
		if err != nil {
			return total, err
		}

		var blockData []byte
		if physBlk == 0 {
			// Sparse hole: return zeros.
			blockData = make([]byte, bs)
		} else {
			blockData, err = v.readBlock(physBlk)
			if err != nil {
				return total, err
			}
		}

		canRead := bs - inBlockOff
		if int64(len(buf)) < canRead {
			canRead = int64(len(buf))
		}
		n := copy(buf, blockData[inBlockOff:inBlockOff+canRead])
		buf = buf[n:]
		byteOffset += int64(n)
		total += n
	}
	return total, nil
}

// ── path resolution ───────────────────────────────────────────────────────────

// dirEntry is a parsed directory entry.
type dirEntry struct {
	inode    uint32
	name     string
	fileType uint8
}

// readDir returns all non-empty directory entries from directory inode dirIn.
func (v *Volume) readDir(dirIn *inode) ([]dirEntry, error) {
	fileSize := inodeFileSize(dirIn)
	bs := int64(v.sb.blockSize)
	var entries []dirEntry

	for off := int64(0); off < fileSize; off += bs {
		logBlk := uint32(off / bs)
		physBlk, err := v.logicalToPhysical(dirIn, logBlk)
		if err != nil {
			return nil, err
		}
		var blockData []byte
		if physBlk == 0 {
			blockData = make([]byte, bs)
		} else {
			blockData, err = v.readBlock(physBlk)
			if err != nil {
				return nil, err
			}
		}

		pos := 0
		for pos < len(blockData) {
			if pos+8 > len(blockData) {
				break
			}
			le := binary.LittleEndian
			ino := le.Uint32(blockData[pos : pos+4])
			recLen := int(le.Uint16(blockData[pos+4 : pos+6]))
			nameLen := int(blockData[pos+6])
			ft := blockData[pos+7]

			if recLen == 0 {
				break
			}
			if ino != 0 && nameLen > 0 {
				name := string(blockData[pos+8 : pos+8+nameLen])
				entries = append(entries, dirEntry{inode: ino, name: name, fileType: ft})
			}
			pos += recLen
		}
	}
	return entries, nil
}

// lookupPath resolves a path to an inode number, starting from the root.
// Path must use '/' as separator.  Symlinks are NOT followed (use Stat for that).
func (v *Volume) lookupPath(p string) (uint32, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return inodeRoot, nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := uint32(inodeRoot)

	for _, part := range parts {
		if part == "" {
			continue
		}
		in, err := v.readInode(cur)
		if err != nil {
			return 0, err
		}
		if in.mode&modeFmt != modeDir {
			return 0, fmt.Errorf("ext4: %q is not a directory", p)
		}
		entries, err := v.readDir(&in)
		if err != nil {
			return 0, err
		}
		found := false
		for _, e := range entries {
			if e.name == part {
				cur = e.inode
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("ext4: %q: no such file or directory", p)
		}
	}
	return cur, nil
}

// lookupPathFollow is like lookupPath but follows symlinks.
func (v *Volume) lookupPathFollow(p string, depth int) (uint32, error) {
	if depth > 40 {
		return 0, fmt.Errorf("ext4: too many symlinks")
	}
	num, err := v.lookupPath(p)
	if err != nil {
		return 0, err
	}
	in, err := v.readInode(num)
	if err != nil {
		return 0, err
	}
	if in.mode&modeFmt == modeLnk {
		target, err := v.readlink(&in)
		if err != nil {
			return 0, err
		}
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(p), target)
		}
		return v.lookupPathFollow(target, depth+1)
	}
	return num, nil
}

// readlink reads the symlink target from an inode.
func (v *Volume) readlink(in *inode) (string, error) {
	size := inodeFileSize(in)
	if size < 60 {
		// Fast symlink: stored in i_block directly.
		return string(in.iBlock[:size]), nil
	}
	buf := make([]byte, size)
	if _, err := v.readFileRange(in, 0, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// ── Volume.ReadFile ───────────────────────────────────────────────────────────

func (v *Volume) ReadFile(name string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(num)
	if err != nil {
		return nil, err
	}
	size := inodeFileSize(&in)
	buf := make([]byte, size)
	if _, err := v.readFileRange(&in, 0, buf); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// ── Volume.Open ───────────────────────────────────────────────────────────────

func (v *Volume) Open(name string) (fs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(num)
	if err != nil {
		return nil, err
	}
	return &ext4File{v: v, num: num, in: in, offset: 0, writable: false}, nil
}

// ── Volume.ReadDir ────────────────────────────────────────────────────────────

func (v *Volume) ReadDir(name string) ([]fs.DirEntry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(num)
	if err != nil {
		return nil, err
	}
	if in.mode&modeFmt != modeDir {
		return nil, fmt.Errorf("ext4: %q is not a directory", name)
	}
	raw, err := v.readDir(&in)
	if err != nil {
		return nil, err
	}

	var out []fs.DirEntry
	for _, e := range raw {
		if e.name == "." || e.name == ".." {
			continue
		}
		ein, err := v.readInode(e.inode)
		if err != nil {
			continue
		}
		out = append(out, &ext4DirEntry{name: e.name, in: ein, inum: e.inode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// ── Volume.Stat / Lstat ───────────────────────────────────────────────────────

func (v *Volume) Stat(name string) (fs.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	num, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(num)
	if err != nil {
		return nil, err
	}
	return &ext4FileInfo{name: pathBase(name), in: in, inum: num}, nil
}

func (v *Volume) Lstat(name string) (fs.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	num, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(num)
	if err != nil {
		return nil, err
	}
	return &ext4FileInfo{name: pathBase(name), in: in, inum: num}, nil
}

// ── Volume.Readlink ───────────────────────────────────────────────────────────

func (v *Volume) Readlink(name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	num, err := v.lookupPath(name)
	if err != nil {
		return "", err
	}
	in, err := v.readInode(num)
	if err != nil {
		return "", err
	}
	if in.mode&modeFmt != modeLnk {
		return "", fmt.Errorf("ext4: %q is not a symlink", name)
	}
	return v.readlink(&in)
}

// ── fs.FileInfo / fs.DirEntry implementations ────────────────────────────────

type ext4FileInfo struct {
	name string
	in   inode
	inum uint32
}

func (fi *ext4FileInfo) Name() string { return fi.name }
func (fi *ext4FileInfo) Size() int64  { return inodeFileSize(&fi.in) }
func (fi *ext4FileInfo) Mode() fs.FileMode {
	return inodeMode(&fi.in)
}
func (fi *ext4FileInfo) ModTime() time.Time { return ext4ToTime(fi.in.mtime) }
func (fi *ext4FileInfo) IsDir() bool        { return fi.in.mode&modeFmt == modeDir }
func (fi *ext4FileInfo) Sys() any           { return fi.inum }

func inodeMode(in *inode) fs.FileMode {
	perm := fs.FileMode(in.mode & 0x1FF)
	switch in.mode & modeFmt {
	case modeDir:
		perm |= fs.ModeDir
	case modeLnk:
		perm |= fs.ModeSymlink
	case modeChr:
		perm |= fs.ModeDevice | fs.ModeCharDevice
	case modeBlk:
		perm |= fs.ModeDevice
	case modeFifo:
		perm |= fs.ModeNamedPipe
	case modeSock:
		perm |= fs.ModeSocket
	}
	return perm
}

type ext4DirEntry struct {
	name string
	in   inode
	inum uint32
}

func (d *ext4DirEntry) Name() string               { return d.name }
func (d *ext4DirEntry) IsDir() bool                { return d.in.mode&modeFmt == modeDir }
func (d *ext4DirEntry) Type() fs.FileMode          { return inodeMode(&d.in).Type() }
func (d *ext4DirEntry) Info() (fs.FileInfo, error) {
	return &ext4FileInfo{name: d.name, in: d.in, inum: d.inum}, nil
}

// ── ext4File ──────────────────────────────────────────────────────────────────

// ext4File implements fs.File and fileBackend.
type ext4File struct {
	v        *Volume
	num      uint32
	in       inode
	offset   int64
	writable bool
	flags    int
}

func (f *ext4File) Stat() (fs.FileInfo, error) {
	return &ext4FileInfo{name: fmt.Sprintf("inode_%d", f.num), in: f.in, inum: f.num}, nil
}

func (f *ext4File) Read(p []byte) (int, error) {
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	n, err := f.v.readFileRange(&f.in, f.offset, p)
	f.offset += int64(n)
	return n, err
}

func (f *ext4File) Write(p []byte) (int, error) {
	if !f.writable {
		return 0, fmt.Errorf("ext4: file not open for writing")
	}
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	n, err := f.v.writeFileRange(f.num, &f.in, f.offset, p)
	f.offset += int64(n)
	// Refresh inode (size may have changed).
	if in, e2 := f.v.readInode(f.num); e2 == nil {
		f.in = in
	}
	return n, err
}

func (f *ext4File) Seek(offset int64, whence int) (int64, error) {
	size := inodeFileSize(&f.in)
	switch whence {
	case io.SeekStart:
		f.offset = offset
	case io.SeekCurrent:
		f.offset += offset
	case io.SeekEnd:
		f.offset = size + offset
	}
	if f.offset < 0 {
		f.offset = 0
	}
	return f.offset, nil
}

func (f *ext4File) Close() error {
	// Flush is handled at Unmount; nothing to do per-file.
	return nil
}

func (f *ext4File) ReadDir(n int) ([]fs.DirEntry, error) {
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	entries, err := f.v.readDir(&f.in)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, e := range entries {
		if e.name == "." || e.name == ".." {
			continue
		}
		ein, err := f.v.readInode(e.inode)
		if err != nil {
			continue
		}
		out = append(out, &ext4DirEntry{name: e.name, in: ein, inum: e.inode})
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// pathBase returns the last path component (like path.Base but simpler).
func pathBase(p string) string {
	p = path.Clean(p)
	i := strings.LastIndex(p, "/")
	if i >= 0 {
		return p[i+1:]
	}
	return p
}