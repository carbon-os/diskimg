package btrfs

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

// ── B-tree traversal ──────────────────────────────────────────────────────────

// walkTree visits every (key, itemData) pair in the tree rooted at rootLogical,
// in ascending key order.  fn returning a non-nil error stops the walk.
func (v *Volume) walkTree(rootLogical uint64, fn func(btrfsKey, []byte) error) error {
	data, err := v.readNode(rootLogical)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	nItems := int(le.Uint32(data[96:100]))
	level := data[100]

	if level > 0 {
		// Internal node: recurse into each child.
		for i := 0; i < nItems; i++ {
			off := nodeHdrSize + i*keyPtrSize
			child := le.Uint64(data[off+keySize:])
			if err := v.walkTree(child, fn); err != nil {
				return err
			}
		}
		return nil
	}

	// Leaf node: iterate items.
	for i := 0; i < nItems; i++ {
		off := nodeHdrSize + i*leafItemSize
		k := decodeKey(data[off:])
		dataOff := int(le.Uint32(data[off+17:])) + nodeHdrSize
		dataSize := int(le.Uint32(data[off+21:]))
		if dataOff+dataSize > len(data) {
			continue
		}
		if err := fn(k, data[dataOff:dataOff+dataSize]); err != nil {
			return err
		}
	}
	return nil
}

// searchTree performs a B-tree lookup for an exact key.
// Returns (data, true, nil) on hit, (nil, false, nil) on miss.
func (v *Volume) searchTree(rootLogical uint64, key btrfsKey) ([]byte, bool, error) {
	logical := rootLogical
	for {
		data, err := v.readNode(logical)
		if err != nil {
			return nil, false, err
		}
		le := binary.LittleEndian
		nItems := int(le.Uint32(data[96:100]))
		level := data[100]

		if level == 0 {
			// Leaf: binary search.
			lo, hi := 0, nItems
			for lo < hi {
				mid := (lo + hi) / 2
				off := nodeHdrSize + mid*leafItemSize
				k := decodeKey(data[off:])
				c := cmpKey(k, key)
				if c == 0 {
					dOff := int(le.Uint32(data[off+17:])) + nodeHdrSize
					dSz := int(le.Uint32(data[off+21:]))
					cpy := make([]byte, dSz)
					copy(cpy, data[dOff:dOff+dSz])
					return cpy, true, nil
				} else if c < 0 {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			return nil, false, nil
		}

		// Internal node: find rightmost key pointer where ptr.key <= search key.
		slot := -1
		for i := 0; i < nItems; i++ {
			off := nodeHdrSize + i*keyPtrSize
			k := decodeKey(data[off:])
			if cmpKey(k, key) <= 0 {
				slot = i
			} else {
				break
			}
		}
		if slot < 0 {
			return nil, false, nil
		}
		off := nodeHdrSize + slot*keyPtrSize
		logical = le.Uint64(data[off+keySize:])
	}
}

// scanItems returns all leaf items in the tree where objectID and itemType
// match, sorted by offset ascending.
func (v *Volume) scanItems(rootLogical uint64, objectID uint64, itemType uint8) ([]struct {
	offset uint64
	data   []byte
}, error) {
	var out []struct {
		offset uint64
		data   []byte
	}
	err := v.walkTree(rootLogical, func(k btrfsKey, d []byte) error {
		if k.objectID == objectID && k.itemType == itemType {
			cpy := make([]byte, len(d))
			copy(cpy, d)
			out = append(out, struct {
				offset uint64
				data   []byte
			}{k.offset, cpy})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].offset < out[j].offset })
	return out, nil
}

// ── inode read ────────────────────────────────────────────────────────────────

func (v *Volume) readInodeItem(objectID uint64) (inodeItem, error) {
	key := btrfsKey{objectID: objectID, itemType: typeInodeItem, offset: 0}
	data, ok, err := v.searchTree(v.fsTreeRoot, key)
	if err != nil {
		return inodeItem{}, err
	}
	if !ok || len(data) < inodeItemSize {
		return inodeItem{}, fmt.Errorf("btrfs: inode %d not found", objectID)
	}
	return decodeInodeItem(data), nil
}

// ── path resolution ───────────────────────────────────────────────────────────

// dirEntry is a decoded directory entry from a DIR_INDEX item.
type dirEntry struct {
	name     string
	location btrfsKey // key of the child inode item
	dtype    uint8
}

// readDirEntries returns all non-dot entries from a directory inode.
func (v *Volume) readDirEntries(dirObjID uint64) ([]dirEntry, error) {
	items, err := v.scanItems(v.fsTreeRoot, dirObjID, typeDirIndex)
	if err != nil {
		return nil, err
	}
	var out []dirEntry
	le := binary.LittleEndian
	for _, it := range items {
		d := it.data
		if len(d) < dirItemHdr {
			continue
		}
		loc := decodeKey(d[0:keySize])
		nameLen := int(le.Uint16(d[27:]))
		dtype := d[29]
		if 30+nameLen > len(d) {
			continue
		}
		name := string(d[30 : 30+nameLen])
		if name == "." || name == ".." {
			continue
		}
		out = append(out, dirEntry{name: name, location: loc, dtype: dtype})
	}
	return out, nil
}

// lookupInDir searches for name in directory dirObjID using DIR_INDEX scan.
func (v *Volume) lookupInDir(dirObjID uint64, name string) (btrfsKey, error) {
	entries, err := v.readDirEntries(dirObjID)
	if err != nil {
		return btrfsKey{}, err
	}
	for _, e := range entries {
		if e.name == name {
			return e.location, nil
		}
	}
	return btrfsKey{}, fmt.Errorf("btrfs: %q not found in dir %d", name, dirObjID)
}

// lookupPath resolves an absolute path to an objectID (no symlink follow).
func (v *Volume) lookupPath(p string) (uint64, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return objFirstFree, nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := objFirstFree

	for _, part := range parts {
		if part == "" {
			continue
		}
		loc, err := v.lookupInDir(cur, part)
		if err != nil {
			return 0, fmt.Errorf("btrfs: %q: %w", p, err)
		}
		cur = loc.objectID
	}
	return cur, nil
}

// lookupPathFollow resolves a path following symlinks (depth-limited).
func (v *Volume) lookupPathFollow(p string, depth int) (uint64, error) {
	if depth > 40 {
		return 0, fmt.Errorf("btrfs: too many symlinks")
	}
	objID, err := v.lookupPath(p)
	if err != nil {
		return 0, err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return 0, err
	}
	if in.mode&ifmt == iflnk {
		target, err := v.readlink(objID, &in)
		if err != nil {
			return 0, err
		}
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(p), target)
		}
		return v.lookupPathFollow(target, depth+1)
	}
	return objID, nil
}

// ── readlink ──────────────────────────────────────────────────────────────────

func (v *Volume) readlink(objID uint64, in *inodeItem) (string, error) {
	data, err := v.readFileData(objID, in)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ── file data read ────────────────────────────────────────────────────────────

// readFileData reads the complete data of a regular file or symlink.
func (v *Volume) readFileData(objID uint64, in *inodeItem) ([]byte, error) {
	if in.size == 0 {
		return nil, nil
	}
	items, err := v.scanItems(v.fsTreeRoot, objID, typeExtentData)
	if err != nil {
		return nil, err
	}
	out := make([]byte, in.size)
	le := binary.LittleEndian
	for _, it := range items {
		fileOff := it.offset // byte offset in file
		d := it.data
		if len(d) < extentDataHdr {
			continue
		}
		compression := d[16]
		extType := d[20]

		switch extType {
		case extInline:
			if compression != 0 {
				return nil, fmt.Errorf("btrfs: compressed inline extent (unsupported)")
			}
			end := fileOff + uint64(len(d)-extentDataHdr)
			if end > in.size {
				end = in.size
			}
			copy(out[fileOff:end], d[extentDataHdr:])

		case extRegular, extPrealloc:
			if len(d) < extentRegSize {
				continue
			}
			diskByteNr := le.Uint64(d[21:])
			// diskNumBytes := le.Uint64(d[29:])
			extOff := le.Uint64(d[37:])
			numBytes := le.Uint64(d[45:])
			if diskByteNr == 0 {
				// Hole: already zeroed in `out`.
				continue
			}
			if compression != 0 {
				return nil, fmt.Errorf("btrfs: compressed regular extent (unsupported)")
			}
			phys := int64(diskByteNr + extOff)
			readLen := numBytes
			if fileOff+readLen > in.size {
				readLen = in.size - fileOff
			}
			if _, err := v.sr.ReadAt(out[fileOff:fileOff+readLen], phys); err != nil && err != io.EOF {
				return nil, fmt.Errorf("btrfs: read extent @%#x: %w", diskByteNr, err)
			}
		}
	}
	return out, nil
}

// ── readFileRange: streaming read for ext4File-style use ─────────────────────

func (v *Volume) readFileRange(objID uint64, in *inodeItem, offset int64, buf []byte) (int, error) {
	if uint64(offset) >= in.size {
		return 0, io.EOF
	}
	all, err := v.readFileData(objID, in)
	if err != nil {
		return 0, err
	}
	if int64(len(all)) <= offset {
		return 0, io.EOF
	}
	n := copy(buf, all[offset:])
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// ── Volume.ReadFile ───────────────────────────────────────────────────────────

func (v *Volume) ReadFile(name string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return nil, err
	}
	return v.readFileData(objID, &in)
}

// ── Volume.Open ───────────────────────────────────────────────────────────────

func (v *Volume) Open(name string) (fs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return nil, err
	}
	return &btrfsFile{v: v, objID: objID, in: in}, nil
}

// ── Volume.ReadDir ────────────────────────────────────────────────────────────

func (v *Volume) ReadDir(name string) ([]fs.DirEntry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return nil, err
	}
	if in.mode&ifmt != ifdir {
		return nil, fmt.Errorf("btrfs: %q is not a directory", name)
	}
	entries, err := v.readDirEntries(objID)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		var ein inodeItem
		if e.location.itemType == typeRootItem {
			// It is a subvolume link. Fake a directory inode so it shows up in ls.
			ein = inodeItem{
				mode: uint32(ifdir) | 0755,
				size: 0,
			}
		} else {
			var err error
			ein, err = v.readInodeItem(e.location.objectID)
			if err != nil {
				continue
			}
		}
		out = append(out, &btrfsDirEntry{name: e.name, in: ein})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// ── Volume.Stat / Lstat ───────────────────────────────────────────────────────

func (v *Volume) Stat(name string) (fs.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return nil, err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return nil, err
	}
	return &btrfsFileInfo{name: pathBase(name), in: in}, nil
}

func (v *Volume) Lstat(name string) (fs.FileInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return nil, err
	}
	return &btrfsFileInfo{name: pathBase(name), in: in}, nil
}

// ── Volume.Readlink ───────────────────────────────────────────────────────────

func (v *Volume) Readlink(name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPath(name)
	if err != nil {
		return "", err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return "", err
	}
	if in.mode&ifmt != iflnk {
		return "", fmt.Errorf("btrfs: %q is not a symlink", name)
	}
	return v.readlink(objID, &in)
}

// ── fs.FileInfo / fs.DirEntry / fs.File implementations ──────────────────────

type btrfsFileInfo struct {
	name string
	in   inodeItem
}

func (fi *btrfsFileInfo) Name() string      { return fi.name }
func (fi *btrfsFileInfo) Size() int64       { return int64(fi.in.size) }
func (fi *btrfsFileInfo) Mode() fs.FileMode { return inodeMode(&fi.in) }
func (fi *btrfsFileInfo) ModTime() time.Time { return toTime(fi.in.mtime) }
func (fi *btrfsFileInfo) IsDir() bool        { return fi.in.mode&ifmt == ifdir }
func (fi *btrfsFileInfo) Sys() any           { return &fi.in }

func inodeMode(in *inodeItem) fs.FileMode {
	perm := fs.FileMode(in.mode & 0x1FF)
	switch in.mode & ifmt {
	case ifdir:
		perm |= fs.ModeDir
	case iflnk:
		perm |= fs.ModeSymlink
	case ifchr:
		perm |= fs.ModeDevice | fs.ModeCharDevice
	case ifblk:
		perm |= fs.ModeDevice
	case ififo:
		perm |= fs.ModeNamedPipe
	case ifsock:
		perm |= fs.ModeSocket
	}
	return perm
}

type btrfsDirEntry struct {
	name string
	in   inodeItem
}

func (d *btrfsDirEntry) Name() string               { return d.name }
func (d *btrfsDirEntry) IsDir() bool                { return d.in.mode&ifmt == ifdir }
func (d *btrfsDirEntry) Type() fs.FileMode          { return inodeMode(&d.in).Type() }
func (d *btrfsDirEntry) Info() (fs.FileInfo, error) {
	return &btrfsFileInfo{name: d.name, in: d.in}, nil
}

// btrfsFile implements fs.File (read-only handle returned by Open).
type btrfsFile struct {
	v      *Volume
	objID  uint64
	in     inodeItem
	offset int64
}

func (f *btrfsFile) Stat() (fs.FileInfo, error) {
	return &btrfsFileInfo{name: fmt.Sprintf("inode_%d", f.objID), in: f.in}, nil
}

func (f *btrfsFile) Read(p []byte) (int, error) {
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	n, err := f.v.readFileRange(f.objID, &f.in, f.offset, p)
	f.offset += int64(n)
	return n, err
}

func (f *btrfsFile) Close() error { return nil }

func (f *btrfsFile) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := f.v.readDirEntries(f.objID)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, e := range entries {
		ein, err := f.v.readInodeItem(e.location.objectID)
		if err != nil {
			continue
		}
		out = append(out, &btrfsDirEntry{name: e.name, in: ein})
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

// findRootItem searches the given tree for a ROOT_ITEM matching objectID.
// It ignores the key's offset, which Btrfs uses for transids in ROOT_ITEMs.
func (v *Volume) findRootItem(treeRoot uint64, objectID uint64) (btrfsKey, []byte, error) {
	var matchKey btrfsKey
	var matchData []byte
	err := v.walkTree(treeRoot, func(k btrfsKey, d []byte) error {
		if k.objectID == objectID && k.itemType == typeRootItem {
			// In case of multiple snapshots/generations, take the highest offset
			if matchData == nil || k.offset > matchKey.offset {
				matchKey = k
				cpy := make([]byte, len(d))
				copy(cpy, d)
				matchData = cpy
			}
		}
		return nil
	})
	if err != nil {
		return btrfsKey{}, nil, err
	}
	if matchData == nil {
		return btrfsKey{}, nil, fmt.Errorf("ROOT_ITEM for objectID %d not found", objectID)
	}
	return matchKey, matchData, nil
}

// ListSubvols implements the subvollister interface for the diskimg CLI.
// It scans the filesystem tree for all subvolume references and returns their names.
func (v *Volume) ListSubvols() ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	var names []string
	le := binary.LittleEndian

	err := v.walkTree(v.fsTreeRoot, func(k btrfsKey, d []byte) error {
		// Subvolume links are stored as DIR_INDEX entries pointing to a ROOT_ITEM
		if k.itemType != typeDirIndex || len(d) < dirItemHdr {
			return nil
		}
		
		loc := decodeKey(d[0:keySize])
		if loc.itemType != typeRootItem { 
			return nil
		}

		nameLen := int(le.Uint16(d[27:]))
		if 30+nameLen > len(d) {
			return nil
		}
		
		names = append(names, string(d[30:30+nameLen]))
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("btrfs: failed to list subvolumes: %w", err)
	}
	return names, nil
}