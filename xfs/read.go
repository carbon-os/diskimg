package xfs

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

// ── extent lookup ─────────────────────────────────────────────────────────────

// extentList returns all bmbt extents from the data fork of in.
// Handles both FMT_EXTENTS (inline array) and FMT_BTREE (one B+tree level).
func (v *Volume) extentList(in *inode) ([]bmbtRec, error) {
	switch in.format {
	case fmtLocal:
		// Data stored inline; no extent records.
		return nil, nil
	case fmtExtents:
		return v.parseInlineExtents(in)
	case fmtBtree:
		return v.parseBtreeExtents(in)
	default:
		return nil, fmt.Errorf("xfs: unsupported data fork format %d", in.format)
	}
}

// parseInlineExtents reads the extent array from the literal area.
func (v *Volume) parseInlineExtents(in *inode) ([]bmbtRec, error) {
	n := int(in.nextents)
	if n == 0 {
		return nil, nil
	}
	needed := n * 16
	if needed > in.litSize {
		return nil, fmt.Errorf("xfs: inline extents overflow literal area")
	}
	recs := make([]bmbtRec, n)
	for i := 0; i < n; i++ {
		recs[i] = parseBmbt(in.literal[i*16:])
	}
	return recs, nil
}

// parseBtreeExtents reads extents from a one-level bmbt B+tree.
// The literal area holds the B+tree root (keys + pointers), and leaves
// are in on-disk blocks.  For simplicity this performs a full tree walk.
func (v *Volume) parseBtreeExtents(in *inode) ([]bmbtRec, error) {
	// The B+tree root is at the start of the literal area.
	raw := in.literal[:in.litSize]
	be := binary.BigEndian
	// First 4 bytes: magic; next 2: level; next 2: numrecs.
	if len(raw) < 8 {
		return nil, fmt.Errorf("xfs: btree root too small")
	}
	level   := be.Uint16(raw[4:6])
	numrecs := be.Uint16(raw[6:8])

	if level == 0 {
		// Inline leaf (unusual but handle it).
		var recs []bmbtRec
		off := 8 // after magic+level+numrecs+padding
		// v5 has an extra 4 bytes padding in the header for alignment.
		if v.sb.hasV5CRC {
			off = 72 // bmbt leaf block header is 72 bytes on v5
		}
		for i := 0; i < int(numrecs); i++ {
			if off+16 > len(raw) {
				break
			}
			recs = append(recs, parseBmbt(raw[off:]))
			off += 16
		}
		return recs, nil
	}

	// Internal node: keys start at offset 72 (v5) or 16 (v4), ptrs after keys.
	// For simplicity, compute the key/ptr split from numrecs.
	hdrSize := 16
	if v.sb.hasV5CRC {
		hdrSize = 72
	}
	// Keys are 8 bytes each (logical file block offset).
	// Ptrs are 8 bytes each (absolute block address in v5, or AG-relative in v4).
	ptrOff := hdrSize + int(numrecs)*8
	var recs []bmbtRec
	for i := 0; i < int(numrecs); i++ {
		if ptrOff+i*8+8 > len(raw) {
			break
		}
		childBlk := be.Uint64(raw[ptrOff+i*8:])
		leafRecs, err := v.readBtreeLeaf(childBlk)
		if err != nil {
			return nil, err
		}
		recs = append(recs, leafRecs...)
	}
	return recs, nil
}

// readBtreeLeaf reads all bmbt records from a leaf block.
func (v *Volume) readBtreeLeaf(blk uint64) ([]bmbtRec, error) {
	data, err := v.readBlock(blk)
	if err != nil {
		return nil, err
	}
	be := binary.BigEndian
	// magic: 4 bytes, level: 2 bytes, numrecs: 2 bytes (big-endian)
	numrecs := int(be.Uint16(data[6:8]))
	hdrSize := 16
	if v.sb.hasV5CRC {
		hdrSize = 72 // v5 has UUID, LSN, owner, blkno in the header
	}
	recs := make([]bmbtRec, 0, numrecs)
	for i := 0; i < numrecs; i++ {
		off := hdrSize + i*16
		if off+16 > len(data) {
			break
		}
		recs = append(recs, parseBmbt(data[off:]))
	}
	return recs, nil
}

// logicalToPhysical maps a logical file block to an absolute physical block.
// Returns 0 for holes (unallocated).
func logicalToPhysical(recs []bmbtRec, logBlk uint64) uint64 {
	for _, r := range recs {
		start := r.startOff()
		cnt   := uint64(r.blockCount())
		if logBlk >= start && logBlk < start+cnt {
			return r.startBlock() + (logBlk - start)
		}
	}
	return 0 // hole
}

// ── file data reading ─────────────────────────────────────────────────────────

// readFileRange reads len(buf) bytes from file inode in at byteOffset.
func (v *Volume) readFileRange(in *inode, byteOffset int64, buf []byte) (int, error) {
	if in.format == fmtLocal {
		// Small file or directory stored inline.
		src := in.literal[:in.size]
		if byteOffset >= int64(len(src)) {
			return 0, io.EOF
		}
		n := copy(buf, src[byteOffset:])
		return n, nil
	}

	if byteOffset >= in.size {
		return 0, io.EOF
	}
	if int64(len(buf)) > in.size-byteOffset {
		buf = buf[:in.size-byteOffset]
	}

	recs, err := v.extentList(in)
	if err != nil {
		return 0, err
	}

	bs := int64(v.sb.blockSize)
	total := 0
	for len(buf) > 0 {
		logBlk    := uint64(byteOffset / bs)
		inBlkOff  := byteOffset % bs
		physBlk   := logicalToPhysical(recs, logBlk)

		var blockData []byte
		if physBlk == 0 {
			blockData = make([]byte, bs) // hole → zeros
		} else {
			blockData, err = v.readBlock(physBlk)
			if err != nil {
				return total, err
			}
		}

		canRead := bs - inBlkOff
		if int64(len(buf)) < canRead {
			canRead = int64(len(buf))
		}
		n := copy(buf, blockData[inBlkOff:inBlkOff+canRead])
		buf = buf[n:]
		byteOffset += int64(n)
		total += n
	}
	return total, nil
}

// ── path resolution ───────────────────────────────────────────────────────────

// dirEntry is a parsed XFS directory entry.
type dirEntry struct {
	ino      uint64
	name     string
	fileType uint8
}

// readDir returns all non-empty directory entries from directory inode dirIn.
// Handles sf (short-form / local), block, leaf, and node/btree formats.
func (v *Volume) readDir(dirIn *inode) ([]dirEntry, error) {
	switch dirIn.format {
	case fmtLocal:
		return v.readDirShortForm(dirIn)
	case fmtExtents, fmtBtree:
		return v.readDirBlock(dirIn)
	default:
		return nil, fmt.Errorf("xfs: unsupported dir format %d", dirIn.format)
	}
}

// readDirShortForm parses a short-form (inline) XFS directory.
//
// Short-form layout in the literal area:
//   2 bytes : count of entries
//   1 byte  : count of entries that need 8-byte ino (i8count)
//   8 bytes : parent ino  (or 4 bytes when i8count==0)
//   then entries:
//     1 byte  : name length
//     <name>  : nameLen bytes
//     2 bytes : offset (ignored here)
//     4 or 8  : inode number
//     1 byte  : file type (v5 with FTYPE feature)
func (v *Volume) readDirShortForm(dirIn *inode) ([]dirEntry, error) {
	raw := dirIn.literal[:dirIn.litSize]
	if len(raw) < 6 {
		return nil, nil
	}
	be := binary.BigEndian
	count   := int(raw[0])
	i8count := int(raw[1])
	// parent ino: 8 bytes if i8count > 0, else 4 bytes
	var pos int
	if i8count > 0 {
		pos = 2 + 8 // count(1) + i8count(1) + parent(8)
	} else {
		pos = 2 + 4
	}

	hasFType := v.sb.featIncompat&featIncompatFType != 0

	entries := make([]dirEntry, 0, count)
	for i := 0; i < count && pos < len(raw); i++ {
		nameLen := int(raw[pos])
		pos++
		if pos+nameLen > len(raw) {
			break
		}
		name := string(raw[pos : pos+nameLen])
		pos += nameLen
		pos += 2 // skip offset field

		var ino uint64
		if i8count > 0 && i < i8count {
			if pos+8 > len(raw) {
				break
			}
			ino = be.Uint64(raw[pos : pos+8])
			pos += 8
		} else {
			if pos+4 > len(raw) {
				break
			}
			ino = uint64(be.Uint32(raw[pos : pos+4]))
			pos += 4
		}
		var ft uint8
		if hasFType {
			if pos >= len(raw) {
				break
			}
			ft = raw[pos]
			pos++
		}
		entries = append(entries, dirEntry{ino: ino, name: name, fileType: ft})
	}
	return entries, nil
}

// readDirBlock reads entries from extent-mapped (block/leaf/node) directory.
func (v *Volume) readDirBlock(dirIn *inode) ([]dirEntry, error) {
	recs, err := v.extentList(dirIn)
	if err != nil {
		return nil, err
	}

	dirBlkBytes := int64(v.sb.blockSize) << uint(v.sb.dirBlkLog)
	bs := int64(v.sb.blockSize)
	hasFType := v.sb.featIncompat&featIncompatFType != 0

	var entries []dirEntry
	for fileBlk := uint64(0); int64(fileBlk)*dirBlkBytes < dirIn.size; fileBlk++ {
		// Read all FS blocks that make up this directory block.
		var blkData []byte
		for fsBlk := uint64(0); fsBlk < uint64(1<<v.sb.dirBlkLog); fsBlk++ {
			logBlk := fileBlk*uint64(1<<v.sb.dirBlkLog) + fsBlk
			physBlk := logicalToPhysical(recs, logBlk)
			var chunk []byte
			if physBlk == 0 {
				chunk = make([]byte, bs)
			} else {
				chunk, err = v.readBlock(physBlk)
				if err != nil {
					return nil, err
				}
			}
			blkData = append(blkData, chunk...)
		}

		entries = append(entries, parseDirBlock(blkData, hasFType)...)
	}
	return entries, nil
}

// parseDirBlock parses XFS directory entries from a raw directory block.
// Works for both v4 (dir2) and v5 (dir3) formats.
func parseDirBlock(data []byte, hasFType bool) []dirEntry {
	be := binary.BigEndian
	// Skip block header: 48 bytes for v5 dir3, 16 bytes for v4 dir2.
	// We detect by magic: 0x58444233 = "XDB3" (v5), 0x58443242 = "XD2B" (v4).
	hdrSize := 16
	if len(data) >= 4 {
		m := be.Uint32(data[0:4])
		if m == 0x58444233 { // XDB3
			hdrSize = 64 // dir3 data block header
		} else if m == 0x58443242 { // XD2B
			hdrSize = 16
		}
	}

	var entries []dirEntry
	pos := hdrSize
	for pos+12 <= len(data) {
		ino := be.Uint64(data[pos : pos+8])
		pos += 8
		nameLen := int(data[pos])
		pos++
		if ino == 0 || nameLen == 0 {
			// Unused entry; the record length is stored at the end.
			// Skip to next 8-byte boundary (simple scan).
			pos = (pos + 7) &^ 7
			continue
		}
		if pos+nameLen > len(data) {
			break
		}
		name := string(data[pos : pos+nameLen])
		pos += nameLen
		var ft uint8
		if hasFType {
			ft = data[pos]
			pos++
		}
		// tag (2 bytes) for alignment
		pos = (pos + 1 + 1) &^ 1 // align to 2 bytes
		_ = ft
		entries = append(entries, dirEntry{ino: ino, name: name, fileType: ft})
	}
	return entries
}

// lookupPath resolves a path to an absolute inode number.
func (v *Volume) lookupPath(p string) (uint64, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return v.sb.rootIno, nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := v.sb.rootIno

	for _, part := range parts {
		if part == "" {
			continue
		}
		in, err := v.readInode(cur)
		if err != nil {
			return 0, err
		}
		if in.mode&modeFmt != modeDir {
			return 0, fmt.Errorf("xfs: %q: not a directory", p)
		}
		entries, err := v.readDir(&in)
		if err != nil {
			return 0, err
		}
		found := false
		for _, e := range entries {
			if e.name == part {
				cur = e.ino
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("xfs: %q: no such file or directory", p)
		}
	}
	return cur, nil
}

// lookupPathFollow resolves a path following symlinks (up to 40 deep).
func (v *Volume) lookupPathFollow(p string, depth int) (uint64, error) {
	if depth > 40 {
		return 0, fmt.Errorf("xfs: too many symlinks")
	}
	ino, err := v.lookupPath(p)
	if err != nil {
		return 0, err
	}
	in, err := v.readInode(ino)
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
	return ino, nil
}

// readlink reads the symlink target from an inode.
func (v *Volume) readlink(in *inode) (string, error) {
	if in.format == fmtLocal {
		return string(in.literal[:in.size]), nil
	}
	buf := make([]byte, in.size)
	if _, err := v.readFileRange(in, 0, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// ── Volume.ReadFile ───────────────────────────────────────────────────────────

func (v *Volume) ReadFile(name string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, in.size)
	if _, err := v.readFileRange(&in, 0, buf); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// ── Volume.Open ───────────────────────────────────────────────────────────────

func (v *Volume) Open(name string) (fs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return nil, err
	}
	return &xfsFile{v: v, ino: ino, in: in}, nil
}

// ── Volume.ReadDir ────────────────────────────────────────────────────────────

func (v *Volume) ReadDir(name string) ([]fs.DirEntry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return nil, err
	}
	if in.mode&modeFmt != modeDir {
		return nil, fmt.Errorf("xfs: %q is not a directory", name)
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
		ein, err := v.readInode(e.ino)
		if err != nil {
			continue
		}
		out = append(out, &xfsDirEntry{name: e.name, in: ein})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// ── Volume.Stat / Lstat ───────────────────────────────────────────────────────

func (v *Volume) Stat(name string) (fs.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ino, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return nil, err
	}
	return &xfsFileInfo{name: pathBase(name), in: in}, nil
}

func (v *Volume) Lstat(name string) (fs.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ino, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return nil, err
	}
	return &xfsFileInfo{name: pathBase(name), in: in}, nil
}

// ── Volume.Readlink ───────────────────────────────────────────────────────────

func (v *Volume) Readlink(name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ino, err := v.lookupPath(name)
	if err != nil {
		return "", err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return "", err
	}
	if in.mode&modeFmt != modeLnk {
		return "", fmt.Errorf("xfs: %q is not a symlink", name)
	}
	return v.readlink(&in)
}

// ── fs.FileInfo / fs.DirEntry implementations ─────────────────────────────────

type xfsFileInfo struct {
	name string
	in   inode
}

func (fi *xfsFileInfo) Name() string    { return fi.name }
func (fi *xfsFileInfo) Size() int64     { return fi.in.size }
func (fi *xfsFileInfo) Mode() fs.FileMode { return inodeMode(&fi.in) }
func (fi *xfsFileInfo) ModTime() time.Time {
	return xfsToTime(fi.in.mtime, fi.in.mtimeNsec)
}
func (fi *xfsFileInfo) IsDir() bool { return fi.in.mode&modeFmt == modeDir }
func (fi *xfsFileInfo) Sys() any    { return fi.in.inum }

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

type xfsDirEntry struct {
	name string
	in   inode
}

func (d *xfsDirEntry) Name() string               { return d.name }
func (d *xfsDirEntry) IsDir() bool                { return d.in.mode&modeFmt == modeDir }
func (d *xfsDirEntry) Type() fs.FileMode          { return inodeMode(&d.in).Type() }
func (d *xfsDirEntry) Info() (fs.FileInfo, error) {
	return &xfsFileInfo{name: d.name, in: d.in}, nil
}

// ── xfsFile ───────────────────────────────────────────────────────────────────

type xfsFile struct {
	v        *Volume
	ino      uint64
	in       inode
	offset   int64
	writable bool
}

func (f *xfsFile) Stat() (fs.FileInfo, error) {
	return &xfsFileInfo{name: fmt.Sprintf("inode_%d", f.ino), in: f.in}, nil
}

func (f *xfsFile) Read(p []byte) (int, error) {
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	n, err := f.v.readFileRange(&f.in, f.offset, p)
	f.offset += int64(n)
	return n, err
}

func (f *xfsFile) Write(p []byte) (int, error) {
	if !f.writable {
		return 0, fmt.Errorf("xfs: file not open for writing")
	}
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	n, err := f.v.writeFileRange(f.ino, &f.in, f.offset, p)
	f.offset += int64(n)
	if in, e2 := f.v.readInode(f.ino); e2 == nil {
		f.in = in
	}
	return n, err
}

func (f *xfsFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.offset = offset
	case io.SeekCurrent:
		f.offset += offset
	case io.SeekEnd:
		f.offset = f.in.size + offset
	}
	if f.offset < 0 {
		f.offset = 0
	}
	return f.offset, nil
}

func (f *xfsFile) Close() error { return nil }

func (f *xfsFile) ReadDir(n int) ([]fs.DirEntry, error) {
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
		ein, err := f.v.readInode(e.ino)
		if err != nil {
			continue
		}
		out = append(out, &xfsDirEntry{name: e.name, in: ein})
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func pathBase(p string) string {
	p = path.Clean(p)
	i := strings.LastIndex(p, "/")
	if i >= 0 {
		return p[i+1:]
	}
	return p
}