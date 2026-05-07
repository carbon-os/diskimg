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

// fsbToLinear converts an XFS structured block number (xfs_fsblock_t)
// to a linear absolute filesystem block number.
func (v *Volume) fsbToLinear(fsb uint64) uint64 {
	ag := fsb >> v.sb.agBlkLog
	agbno := fsb & ((uint64(1) << v.sb.agBlkLog) - 1)
	return ag*uint64(v.sb.agBlocks) + agbno
}

// logicalToPhysical maps a logical file block to an absolute physical block.
// Returns 0 for holes (unallocated).
func (v *Volume) logicalToPhysical(recs []bmbtRec, logBlk uint64) uint64 {
	for _, r := range recs {
		start := r.startOff()
		cnt   := uint64(r.blockCount())
		if logBlk >= start && logBlk < start+cnt {
			fsb := r.startBlock() + (logBlk - start)
			return v.fsbToLinear(fsb)
		}
	}
	return 0 // hole
}

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

	// nextents can be 0 on disk when the journal was destroyed before replay:
	// size/nblocks were already flushed but the nextents increment was only
	// in the log. If the literal area contains valid-looking bmbt records,
	// infer the count rather than returning nothing.
	if n == 0 && in.size > 0 && in.nblocks > 0 {
		n = v.inferNextents(in)
	}

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

// inferNextents scans the data-fork literal area of an fmtExtents inode for
// leading valid bmbt records when di_nextents is zero but size/nblocks say
// data exists. This recovers from a zeroed journal whose replay was skipped.
func (v *Volume) inferNextents(in *inode) int {
	// Data fork ends at forkoff*8 bytes into the literal area.
	// forkoff == 0 means no attr fork → whole literal area is data fork.
	dataForkBytes := in.litSize
	if in.forkoff > 0 {
		df := int(in.forkoff) * 8
		if df < in.litSize {
			dataForkBytes = df
		}
	}

	maxRecs := dataForkBytes / 16
	if maxRecs > 4096 {
		maxRecs = 4096 // sanity cap
	}

	var totalBlocks uint64
	for i := 0; i < maxRecs; i++ {
		if i*16+16 > in.litSize {
			break
		}
		r := parseBmbt(in.literal[i*16:])

		// A record with zero blockCount is an empty slot — stop here.
		if r.blockCount() == 0 {
			break
		}
		// startBlock of 0 would be the AG superblock — definitely invalid.
		fsb := r.startBlock()
		if fsb == 0 {
			break
		}
		// The linear block must fall within the filesystem.
		lin := v.fsbToLinear(fsb)
		if lin == 0 || lin >= v.sb.dblocks {
			break
		}
		totalBlocks += uint64(r.blockCount())
		// Stop as soon as the cumulative block count matches nblocks.
		// This prevents reading into attr-fork or garbage data.
		if totalBlocks >= in.nblocks {
			return i + 1
		}
	}

	// Fallback: return however many looked valid.
	// (Reached here only if nblocks accounting didn't align cleanly.)
	return 0
}

// parseBtreeExtents reads extents from a one-level bmbt B+tree.
func (v *Volume) parseBtreeExtents(in *inode) ([]bmbtRec, error) {
	raw := in.literal[:in.litSize]
	be := binary.BigEndian
	
	// Inode B-tree roots (xfs_bmdr_block) are exactly 4 bytes
	if len(raw) < 4 {
		return nil, fmt.Errorf("xfs: btree root too small")
	}
	level   := be.Uint16(raw[0:2])
	numrecs := be.Uint16(raw[2:4])

	if level == 0 {
		return nil, fmt.Errorf("xfs: inode btree root cannot be level 0")
	}

	// XFS stores B-Tree pointers AFTER the maximum possible number of keys, 
	// not the current number of keys. Each key + ptr pair is 16 bytes.
	maxrecs := (in.litSize - 4) / 16
	ptrOff := 4 + (maxrecs * 8)
	
	var recs []bmbtRec
	for i := 0; i < int(numrecs); i++ {
		if ptrOff+i*8+8 > len(raw) {
			break
		}
		childFsb := be.Uint64(raw[ptrOff+i*8:])
		leafRecs, err := v.readBtreeLeaf(v.fsbToLinear(childFsb)) // Convert FSB to linear
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
		physBlk   := v.logicalToPhysical(recs, logBlk)

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

// readDirShortForm reads entries from a short-form (inline) directory.
// In read.go
func (v *Volume) readDirShortForm(dirIn *inode) ([]dirEntry, error) {
	raw := dirIn.literal[:dirIn.litSize]
	if len(raw) < 6 {
		return nil, nil
	}
	be := binary.BigEndian
	count := int(raw[0])
	i8count := int(raw[1])
	
	var pos int
	if i8count > 0 {
		pos = 2 + 8
	} else {
		pos = 2 + 4
	}

	hasFType := v.sb.featIncompat&featIncompatFType != 0

	entries := make([]dirEntry, 0, count)
	for i := 0; i < count && pos < len(raw); i++ {
		nameLen := int(raw[pos])
		pos++
		
		pos += 2 // Offset comes BEFORE the name
		
		if pos+nameLen > len(raw) {
			break
		}
		name := string(raw[pos : pos+nameLen])
		pos += nameLen

		// REVERTED: ftype comes BEFORE the inode!
		var ft uint8
		if hasFType {
			if pos < len(raw) {
				ft = raw[pos]
			}
			pos++
		}

		var ino uint64
		if i8count > 0 {
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
	
	// Ensure we don't try to parse 32GB leaf metadata as directory entries
	limit := dirIn.size
	if limit > 34359738368 {
		limit = 34359738368
	}

	for fileBlk := uint64(0); int64(fileBlk)*dirBlkBytes < limit; fileBlk++ {
		// Read all FS blocks that make up this directory block.
		var blkData []byte
		for fsBlk := uint64(0); fsBlk < uint64(1<<v.sb.dirBlkLog); fsBlk++ {
			logBlk := fileBlk*uint64(1<<v.sb.dirBlkLog) + fsBlk
			physBlk := v.logicalToPhysical(recs, logBlk)
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

		if ents := parseDirBlock(blkData, hasFType); ents != nil {
			entries = append(entries, ents...)
		}
	}
	return entries, nil
}

// parseDirBlock parses XFS directory entries from a raw directory block.
func parseDirBlock(data []byte, hasFType bool) []dirEntry {
	if len(data) < 4 {
		return nil
	}
	be := binary.BigEndian
	m := be.Uint32(data[0:4])

	hdrSize := 16
	// Match both single-block (XDB3) and multi-block (XDD3) v5 magic strings
	if m == 0x58444233 || m == 0x58444433 {
		hdrSize = 64
	} else if m == 0x58443242 || m == 0x58443244 { // XD2B or XD2D (v4)
		hdrSize = 16
	} else {
		// Not a valid directory block (likely an unallocated hole of zeroes)
		return nil
	}

	var entries []dirEntry
	pos := hdrSize
	for pos+12 <= len(data) {
		entStart := pos

		// 1. Check for unused space (freetag == 0xFFFF)
		freetag := be.Uint16(data[pos : pos+2])
		if freetag == 0xFFFF {
			length := int(be.Uint16(data[pos+2 : pos+4]))
			if length == 0 {
				break
			}
			pos += length
			continue
		}

		ino := be.Uint64(data[pos : pos+8])
		pos += 8
		nameLen := int(data[pos])
		pos++
		if pos+nameLen > len(data) {
			break
		}
		name := string(data[pos : pos+nameLen])
		pos += nameLen

		var ft uint8
		if hasFType && pos < len(data) {
			ft = data[pos]
			// The position increment for ft is handled implicitly in entSize calculation
		}

		// 2. Calculate exact total size of this entry and round up to 8
		entSize := 8 + 1 + nameLen + 2 // ino + namelen + name + tag
		if hasFType {
			entSize++
		}
		entSize = (entSize + 7) &^ 7 // Align to 8-byte boundary

		pos = entStart + entSize
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