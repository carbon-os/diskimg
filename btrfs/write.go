package btrfs

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	volfs "github.com/carbon-os/diskimg/fs"
)

// ── leaf-node helpers ─────────────────────────────────────────────────────────

// leafNItems returns the number of items in a leaf node.
func leafNItems(data []byte) int {
	return int(binary.LittleEndian.Uint32(data[96:100]))
}

// leafFree returns the number of free bytes available for a new (descriptor + data) pair.
func leafFree(data []byte, nodeSize int) int {
	n := leafNItems(data)
	usedDesc := n * leafItemSize
	le := binary.LittleEndian
	usedData := 0
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*leafItemSize
		usedData += int(le.Uint32(data[off+21:]))
	}
	return nodeSize - nodeHdrSize - usedDesc - usedData
}

// leafDataStart returns the physical byte offset of the first data byte
// (data grows from the end of the node downward).
func leafDataStart(data []byte, nodeSize int) int {
	n := leafNItems(data)
	le := binary.LittleEndian
	total := 0
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*leafItemSize
		total += int(le.Uint32(data[off+21:]))
	}
	return nodeSize - total
}

// leafInsert inserts key+itemData into an existing leaf node in-place.
// The caller must have verified there is enough free space (leafFree >= leafItemSize+len(itemData)).
func leafInsert(data []byte, nodeSize int, key btrfsKey, itemData []byte) {
	le := binary.LittleEndian
	n := leafNItems(data)

	// Binary search for insertion point (first slot where existing key > key).
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		off := nodeHdrSize + mid*leafItemSize
		k := decodeKey(data[off:])
		if cmpKey(k, key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	insertPos := lo

	// Shift existing descriptors right by one slot.
	if insertPos < n {
		src := nodeHdrSize + insertPos*leafItemSize
		copy(data[src+leafItemSize:src+leafItemSize+(n-insertPos)*leafItemSize],
			data[src:src+(n-insertPos)*leafItemSize])
	}

	// Place item data just before the current data block.
	ds := leafDataStart(data, nodeSize)
	newDataAbs := ds - len(itemData)
	copy(data[newDataAbs:], itemData)

	// Write item descriptor.
	descOff := nodeHdrSize + insertPos*leafItemSize
	encodeKey(key, data[descOff:])
	newDataRelOff := uint32(newDataAbs - nodeHdrSize)
	le.PutUint32(data[descOff+17:], newDataRelOff)
	le.PutUint32(data[descOff+21:], uint32(len(itemData)))

	// Update nritems.
	le.PutUint32(data[96:100], uint32(n+1))
}

// leafDelete removes the item at the given index from a leaf node in-place.
// Data bytes of the removed item are compacted out; all higher items' offsets
// are adjusted accordingly.
func leafDelete(data []byte, nodeSize int, idx int) {
	le := binary.LittleEndian
	n := leafNItems(data)
	if idx < 0 || idx >= n {
		return
	}

	descOff := nodeHdrSize + idx*leafItemSize
	removedRelOff := int(le.Uint32(data[descOff+17:]))
	removedSize := int(le.Uint32(data[descOff+21:]))
	removedAbsOff := nodeHdrSize + removedRelOff

	// All items with dataOffset >= removedRelOff need their offset increased by removedSize
	// (their data will be shifted down after the removed data is removed).
	for i := 0; i < n; i++ {
		if i == idx {
			continue
		}
		off := nodeHdrSize + i*leafItemSize
		relOff := int(le.Uint32(data[off+17:]))
		if relOff < removedRelOff {
			// This item's data is below the removed item's data in the
			// data area (closer to the end of node). Shift it down.
			le.PutUint32(data[off+17:], uint32(relOff+removedSize))
		}
	}

	// Compact data area: move data above removed item down by removedSize.
	dataStart := nodeHdrSize + leafDataStart(data, nodeSize) - nodeHdrSize
	_ = dataStart
	// Items above removedAbsOff (toward start of node) move down.
	dStart := leafDataStart(data, nodeSize)
	if removedAbsOff > dStart {
		copy(data[dStart+removedSize:], data[dStart:removedAbsOff])
	}

	// Remove descriptor: shift remaining descriptors left.
	if idx < n-1 {
		src := nodeHdrSize + (idx+1)*leafItemSize
		copy(data[nodeHdrSize+idx*leafItemSize:], data[src:nodeHdrSize+n*leafItemSize])
	}
	// Zero out the last descriptor slot.
	last := nodeHdrSize + (n-1)*leafItemSize
	for i := last; i < last+leafItemSize; i++ {
		data[i] = 0
	}
	le.PutUint32(data[96:100], uint32(n-1))
}

// leafFind returns the index of the item with exactly key, or -1.
func leafFind(data []byte, key btrfsKey) int {
	n := leafNItems(data)
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		off := nodeHdrSize + mid*leafItemSize
		k := decodeKey(data[off:])
		c := cmpKey(k, key)
		if c == 0 {
			return mid
		} else if c < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return -1
}

// ── B-tree insert/delete (FS tree) ────────────────────────────────────────────

// insertItem inserts or replaces a (key, data) pair in the FS tree.
// If the target leaf is full, it is split.  If the split propagates to
// the root, *v.fsTreeRoot is updated.
func (v *Volume) insertItem(key btrfsKey, itemData []byte) error {
	return v.btreeInsert(&v.fsTreeRoot, key, itemData, v.sb.generation+1)
}

// btreeInsert is the recursive COW insert into the tree rooted at *rootLogical.
func (v *Volume) btreeInsert(rootLogical *uint64, key btrfsKey, itemData []byte, gen uint64) error {
	data, err := v.readNode(*rootLogical)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	level := data[100]

	if level == 0 {
		// Leaf node.
		needed := leafItemSize + len(itemData)
		free := leafFree(data, int(v.sb.nodeSize))

		// If item already exists, replace in-place.
		if idx := leafFind(data, key); idx >= 0 {
			descOff := nodeHdrSize + idx*leafItemSize
			oldSz := int(le.Uint32(data[descOff+21:]))
			if oldSz == len(itemData) {
				// Same size: overwrite data bytes directly.
				relOff := int(le.Uint32(data[descOff+17:]))
				copy(data[nodeHdrSize+relOff:], itemData)
				return v.writeNode(*rootLogical, data)
			}
			// Different size: delete then re-insert.
			leafDelete(data, int(v.sb.nodeSize), idx)
			// Recompute free after deletion.
			free = leafFree(data, int(v.sb.nodeSize))
		}

		if free >= needed {
			leafInsert(data, int(v.sb.nodeSize), key, itemData)
			return v.writeNode(*rootLogical, data)
		}

		// Need to split the leaf.
		return v.splitLeaf(rootLogical, data, key, itemData, gen)
	}

	// Internal node: find child and recurse.
	nItems := int(le.Uint32(data[96:100]))
	slot := nItems - 1
	for i := 0; i < nItems; i++ {
		off := nodeHdrSize + i*keyPtrSize
		k := decodeKey(data[off:])
		if cmpKey(k, key) > 0 {
			if i > 0 {
				slot = i - 1
			} else {
				slot = 0
			}
			break
		}
		slot = i
	}

	childOff := nodeHdrSize + slot*keyPtrSize
	childLogical := le.Uint64(data[childOff+keySize:])

	// Recurse, passing the child's logical address.
	newChildRoot := childLogical
	if err := v.btreeInsert(&newChildRoot, key, itemData, gen); err != nil {
		return err
	}
	if newChildRoot != childLogical {
		// Child split: a new sibling was created; we need to add a key pointer.
		// The returned newChildRoot is actually the left child (same as before
		// after split); the right child logical is recorded in splitSibling.
		// This path is simplified: re-read child to get first key of right sibling.
		// For now: update existing pointer to left child and add right sibling entry.
		// Full implementation would track the split key. Return error for now.
		return fmt.Errorf("btrfs: internal node split required (tree too deep for current workload)")
	}
	// Update the first key of the child if needed.
	childData, err := v.readNode(childLogical)
	if err != nil {
		return err
	}
	if leafNItems(childData) > 0 {
		firstKey := decodeKey(childData[nodeHdrSize:])
		existingKey := decodeKey(data[childOff:])
		if cmpKey(firstKey, existingKey) != 0 {
			encodeKey(firstKey, data[childOff:])
			le.PutUint64(data[childOff+keySize:], childLogical)
			le.PutUint64(data[childOff+keySize+8:], gen)
			if err := v.writeNode(*rootLogical, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitLeaf splits a full leaf, inserts key+itemData into the correct half,
// and promotes the split key into the parent (or creates a new root).
func (v *Volume) splitLeaf(rootLogical *uint64, leafData []byte, key btrfsKey, itemData []byte, gen uint64) error {
	le := binary.LittleEndian
	n := leafNItems(leafData)
	mid := n / 2

	// Allocate new right leaf.
	rightLogical, rightData := v.allocNode()
	// Set header fields.
	le.PutUint64(rightData[48:], rightLogical) // bytenr
	le.PutUint64(rightData[80:], gen)           // generation
	le.PutUint64(rightData[88:], objFSTree)     // owner
	rightData[100] = 0                          // level = leaf

	// Move upper half of items to right leaf.
	for i := mid; i < n; i++ {
		srcDescOff := nodeHdrSize + i*leafItemSize
		k := decodeKey(leafData[srcDescOff:])
		relOff := int(le.Uint32(leafData[srcDescOff+17:]))
		sz := int(le.Uint32(leafData[srcDescOff+21:]))
		absOff := nodeHdrSize + relOff
		leafInsert(rightData, int(v.sb.nodeSize), k, leafData[absOff:absOff+sz])
	}
	// Truncate left leaf to first mid items.
	for i := n - 1; i >= mid; i-- {
		leafDelete(leafData, int(v.sb.nodeSize), i)
	}

	// Insert the new key into the appropriate half.
	splitKey := decodeKey(rightData[nodeHdrSize:]) // first key of right leaf
	if cmpKey(key, splitKey) < 0 {
		leafInsert(leafData, int(v.sb.nodeSize), key, itemData)
		if err := v.writeNode(*rootLogical, leafData); err != nil {
			return err
		}
	} else {
		leafInsert(rightData, int(v.sb.nodeSize), key, itemData)
	}
	if err := v.writeNode(rightLogical, rightData); err != nil {
		return err
	}

	// Promote splitKey into the parent.  Since we only track the root and
	// the current node is the root (or we need to create a new root):
	leftLogical := *rootLogical
	leftFirstKey := decodeKey(leafData[nodeHdrSize:])

	// Create new internal root.
	newRootLogical, newRootData := v.allocNode()
	le.PutUint64(newRootData[48:], newRootLogical)
	le.PutUint64(newRootData[80:], gen)
	le.PutUint64(newRootData[88:], objFSTree)
	newRootData[100] = 1 // level 1
	le.PutUint32(newRootData[96:100], 2)

	// Left key pointer.
	off0 := nodeHdrSize
	encodeKey(leftFirstKey, newRootData[off0:])
	le.PutUint64(newRootData[off0+keySize:], leftLogical)
	le.PutUint64(newRootData[off0+keySize+8:], gen)

	// Right key pointer.
	off1 := nodeHdrSize + keyPtrSize
	encodeKey(splitKey, newRootData[off1:])
	le.PutUint64(newRootData[off1+keySize:], rightLogical)
	le.PutUint64(newRootData[off1+keySize+8:], gen)

	if err := v.writeNode(newRootLogical, newRootData); err != nil {
		return err
	}
	*rootLogical = newRootLogical
	return nil
}

// deleteItem removes the item with the given key from the FS tree.
func (v *Volume) deleteItem(key btrfsKey) error {
	return v.btreeDelete(&v.fsTreeRoot, key)
}

func (v *Volume) btreeDelete(rootLogical *uint64, key btrfsKey) error {
	data, err := v.readNode(*rootLogical)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	level := data[100]

	if level == 0 {
		idx := leafFind(data, key)
		if idx < 0 {
			return nil // not found → no-op
		}
		leafDelete(data, int(v.sb.nodeSize), idx)
		return v.writeNode(*rootLogical, data)
	}

	// Internal node: find child and recurse.
	nItems := int(le.Uint32(data[96:100]))
	slot := 0
	for i := nItems - 1; i >= 0; i-- {
		off := nodeHdrSize + i*keyPtrSize
		k := decodeKey(data[off:])
		if cmpKey(k, key) <= 0 {
			slot = i
			break
		}
	}
	off := nodeHdrSize + slot*keyPtrSize
	childLogical := le.Uint64(data[off+keySize:])
	child := childLogical
	return v.btreeDelete(&child, key)
}

// ── inode write ───────────────────────────────────────────────────────────────

func (v *Volume) writeInodeItem(objID uint64, in *inodeItem) error {
	return v.insertItem(
		btrfsKey{objectID: objID, itemType: typeInodeItem, offset: 0},
		encodeInodeItem(in),
	)
}

// ── directory writes ──────────────────────────────────────────────────────────

// encodeDirItem encodes a btrfs_dir_item pointing to childKey.
func encodeDirItem(childKey btrfsKey, dtype uint8, name string) []byte {
	le := binary.LittleEndian
	nameBytes := []byte(name)
	buf := make([]byte, dirItemHdr+len(nameBytes))
	encodeKey(childKey, buf[0:keySize])
	le.PutUint64(buf[17:], 0)                   // transid = 0
	le.PutUint16(buf[25:], 0)                   // data_len = 0
	le.PutUint16(buf[27:], uint16(len(nameBytes)))
	buf[29] = dtype
	copy(buf[30:], nameBytes)
	return buf
}

func (v *Volume) addDirEntry(dirObjID uint64, name string, childObjID uint64, dtype uint8) error {
	childKey := btrfsKey{objectID: childObjID, itemType: typeInodeItem, offset: 0}
	dirItem := encodeDirItem(childKey, dtype, name)

	// DIR_ITEM keyed by name hash.
	if err := v.insertItem(
		btrfsKey{objectID: dirObjID, itemType: typeDirItem, offset: nameHash(name)},
		dirItem,
	); err != nil {
		return err
	}

	// DIR_INDEX keyed by sequence number.
	v.initDirSeq(dirObjID)
	seq := v.nextDirSeq(dirObjID)
	if err := v.insertItem(
		btrfsKey{objectID: dirObjID, itemType: typeDirIndex, offset: seq},
		dirItem,
	); err != nil {
		return err
	}

	// INODE_REF: (childObjID, INODE_REF, dirObjID) → seq + name.
	le := binary.LittleEndian
	nameBytes := []byte(name)
	ref := make([]byte, inodeRefHdr+len(nameBytes))
	le.PutUint64(ref[0:], seq)
	le.PutUint16(ref[8:], uint16(len(nameBytes)))
	copy(ref[10:], nameBytes)
	return v.insertItem(
		btrfsKey{objectID: childObjID, itemType: typeInodeRef, offset: dirObjID},
		ref,
	)
}

func (v *Volume) removeDirEntry(dirObjID uint64, name string, childObjID uint64) error {
	// Remove DIR_ITEM.
	_ = v.deleteItem(btrfsKey{objectID: dirObjID, itemType: typeDirItem, offset: nameHash(name)})

	// Find and remove DIR_INDEX.
	items, _ := v.scanItems(v.fsTreeRoot, dirObjID, typeDirIndex)
	le := binary.LittleEndian
	for _, it := range items {
		d := it.data
		if len(d) < dirItemHdr {
			continue
		}
		nameLen := int(le.Uint16(d[27:]))
		if 30+nameLen > len(d) {
			continue
		}
		if string(d[30:30+nameLen]) == name {
			_ = v.deleteItem(btrfsKey{objectID: dirObjID, itemType: typeDirIndex, offset: it.offset})
			break
		}
	}

	// Remove INODE_REF.
	_ = v.deleteItem(btrfsKey{objectID: childObjID, itemType: typeInodeRef, offset: dirObjID})
	return nil
}

// ── extent data writes ────────────────────────────────────────────────────────

// writeExtentData writes file content for objID. Small files use inline
// extents; larger files use regular extents.
func (v *Volume) writeExtentData(objID uint64, in *inodeItem, data []byte) error {
	// Remove existing EXTENT_DATA items first.
	existing, _ := v.scanItems(v.fsTreeRoot, objID, typeExtentData)
	for _, it := range existing {
		_ = v.deleteItem(btrfsKey{objectID: objID, itemType: typeExtentData, offset: it.offset})
	}
	if len(data) == 0 {
		return nil
	}
	le := binary.LittleEndian

	if len(data) <= inlineMax {
		// Inline extent.
		buf := make([]byte, extentDataHdr+len(data))
		le.PutUint64(buf[0:], in.generation)
		le.PutUint64(buf[8:], uint64(len(data)))
		buf[16] = 0 // no compression
		buf[17] = 0 // no encryption
		le.PutUint16(buf[18:], 0)
		buf[20] = extInline
		copy(buf[extentDataHdr:], data)
		return v.insertItem(
			btrfsKey{objectID: objID, itemType: typeExtentData, offset: 0},
			buf,
		)
	}

	// Regular extent: allocate data block.
	dataLogical := v.allocDataBlock(uint64(len(data)))
	// Write data to dirty cache at physical location.
	phys, err := v.logToPhys(dataLogical)
	if err != nil {
		return err
	}
	aligned := (len(data) + int(v.sb.sectorSize) - 1) / int(v.sb.sectorSize) * int(v.sb.sectorSize)
	blk := make([]byte, aligned)
	copy(blk, data)
	cpy := make([]byte, aligned)
	copy(cpy, blk)
	v.dirty[phys] = cpy

	// Regular extent descriptor.
	buf := make([]byte, extentRegSize)
	le.PutUint64(buf[0:], in.generation)
	le.PutUint64(buf[8:], uint64(len(data)))
	buf[16] = 0 // no compression
	buf[17] = 0
	le.PutUint16(buf[18:], 0)
	buf[20] = extRegular
	le.PutUint64(buf[21:], dataLogical)           // disk_bytenr
	le.PutUint64(buf[29:], uint64(aligned))        // disk_num_bytes
	le.PutUint64(buf[37:], 0)                      // offset in extent
	le.PutUint64(buf[45:], uint64(len(data)))       // num_bytes
	return v.insertItem(
		btrfsKey{objectID: objID, itemType: typeExtentData, offset: 0},
		buf,
	)
}

// ── Volume.WriteFile ──────────────────────────────────────────────────────────

func (v *Volume) WriteFile(name string, data []byte, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Check if the file already exists.
	objID, err := v.lookupPath(name)
	if err != nil {
		return v.createFile(name, data, perm)
	}

	// Overwrite existing.
	in, err := v.readInodeItem(objID)
	if err != nil {
		return err
	}
	in.size = uint64(len(data))
	in.mtime = nowSec()
	in.ctime = nowSec()
	if err := v.writeInodeItem(objID, &in); err != nil {
		return err
	}
	return v.writeExtentData(objID, &in, data)
}

// createFile creates a new regular file with content.
func (v *Volume) createFile(name string, data []byte, perm fs.FileMode) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirObjID, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return fmt.Errorf("btrfs: create %q: parent: %w", name, err)
	}

	now := nowSec()
	newObjID := v.allocObjID()
	in := inodeItem{
		generation: v.sb.generation + 1,
		transID:    v.sb.generation + 1,
		size:       uint64(len(data)),
		nbytes:     uint64(len(data)),
		nlink:      1,
		mode:       uint32(ifreg) | uint32(perm&0x1FF),
		atime:      now,
		ctime:      now,
		mtime:      now,
		otime:      now,
	}
	if err := v.writeInodeItem(newObjID, &in); err != nil {
		return err
	}
	if err := v.writeExtentData(newObjID, &in, data); err != nil {
		return err
	}
	return v.addDirEntry(dirObjID, base, newObjID, dirReg)
}

// ── Volume.Create ─────────────────────────────────────────────────────────────

func (v *Volume) Create(name string) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := v.createFile(name, nil, 0644); err != nil {
		// Might already exist – truncate it.
		objID, lerr := v.lookupPath(name)
		if lerr != nil {
			return nil, err
		}
		in, _ := v.readInodeItem(objID)
		in.size = 0
		_ = v.writeInodeItem(objID, &in)
		_ = v.writeExtentData(objID, &in, nil)
	}

	objID, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return nil, err
	}
	f := &btrfsWriteFile{v: v, objID: objID, in: in, writable: true}
	return volfs.NewFile(f), nil
}

// ── Volume.OpenFile ───────────────────────────────────────────────────────────

func (v *Volume) OpenFile(name string, flag int, perm fs.FileMode) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	create := flag&os.O_CREATE != 0
	trunc := flag&os.O_TRUNC != 0
	excl := flag&os.O_EXCL != 0
	appendMode := flag&os.O_APPEND != 0

	objID, err := v.lookupPath(name)
	if err != nil {
		if !create {
			return nil, err
		}
		if e2 := v.createFile(name, nil, perm); e2 != nil {
			return nil, e2
		}
		objID, _ = v.lookupPath(name)
	} else if excl {
		return nil, fmt.Errorf("btrfs: %q already exists", name)
	}

	in, err := v.readInodeItem(objID)
	if err != nil {
		return nil, err
	}
	if trunc && writable {
		in.size = 0
		in.mtime = nowSec()
		in.ctime = nowSec()
		_ = v.writeInodeItem(objID, &in)
		_ = v.writeExtentData(objID, &in, nil)
	}
	offset := int64(0)
	if appendMode {
		offset = int64(in.size)
	}
	f := &btrfsWriteFile{v: v, objID: objID, in: in, offset: offset, writable: writable}
	return volfs.NewFile(f), nil
}

// ── btrfsWriteFile (fileBackend for Create/OpenFile) ─────────────────────────

type btrfsWriteFile struct {
	v        *Volume
	objID    uint64
	in       inodeItem
	offset   int64
	writable bool
}

func (f *btrfsWriteFile) Read(p []byte) (int, error) {
	f.v.mu.Lock()
	defer f.v.mu.Unlock()
	return f.v.readFileRange(f.objID, &f.in, f.offset, p)
}

func (f *btrfsWriteFile) Write(p []byte) (int, error) {
	if !f.writable {
		return 0, fmt.Errorf("btrfs: not open for writing")
	}
	f.v.mu.Lock()
	defer f.v.mu.Unlock()

	// Read current content, splice in new data, write back.
	cur, _ := f.v.readFileData(f.objID, &f.in)
	end := f.offset + int64(len(p))
	if end > int64(len(cur)) {
		ext := make([]byte, end)
		copy(ext, cur)
		cur = ext
	}
	copy(cur[f.offset:], p)
	f.in.size = uint64(len(cur))
	f.in.mtime = nowSec()
	f.in.ctime = nowSec()
	_ = f.v.writeInodeItem(f.objID, &f.in)
	_ = f.v.writeExtentData(f.objID, &f.in, cur)
	f.offset += int64(len(p))
	return len(p), nil
}

func (f *btrfsWriteFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		f.offset = offset
	case 1:
		f.offset += offset
	case 2:
		f.offset = int64(f.in.size) + offset
	}
	if f.offset < 0 {
		f.offset = 0
	}
	return f.offset, nil
}

func (f *btrfsWriteFile) Close() error { return nil }

func (f *btrfsWriteFile) Stat() (fs.FileInfo, error) {
	return &btrfsFileInfo{name: fmt.Sprintf("inode_%d", f.objID), in: f.in}, nil
}

func (f *btrfsWriteFile) ReadDir(n int) ([]fs.DirEntry, error) {
	return nil, fmt.Errorf("btrfs: ReadDir on non-directory")
}

// ── Volume.Mkdir / MkdirAll ───────────────────────────────────────────────────

func (v *Volume) Mkdir(name string, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mkdir(name, perm)
}

func (v *Volume) mkdir(name string, perm fs.FileMode) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirObjID, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return fmt.Errorf("btrfs: mkdir %q: parent: %w", name, err)
	}

	now := nowSec()
	newObjID := v.allocObjID()
	in := inodeItem{
		generation: v.sb.generation + 1,
		transID:    v.sb.generation + 1,
		nlink:      2,
		mode:       uint32(ifdir) | uint32(perm&0x1FF),
		atime:      now,
		ctime:      now,
		mtime:      now,
		otime:      now,
	}
	if err := v.writeInodeItem(newObjID, &in); err != nil {
		return err
	}
	// Add "." and ".." entries inside the new directory.
	if err := v.addDirEntry(newObjID, ".", newObjID, dirDir); err != nil {
		return err
	}
	if err := v.addDirEntry(newObjID, "..", dirObjID, dirDir); err != nil {
		return err
	}
	// Link into parent.
	return v.addDirEntry(dirObjID, base, newObjID, dirDir)
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
	dirObjID, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return err
	}
	objID, err := v.lookupPath(name)
	if err != nil {
		return err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return err
	}
	if in.mode&ifmt == ifdir && !allowDir {
		return fmt.Errorf("btrfs: %q is a directory", name)
	}
	if err := v.removeDirEntry(dirObjID, base, objID); err != nil {
		return err
	}
	in.nlink--
	in.ctime = nowSec()
	if in.nlink == 0 {
		_ = v.deleteItem(btrfsKey{objectID: objID, itemType: typeInodeItem, offset: 0})
		// Remove extent data.
		items, _ := v.scanItems(v.fsTreeRoot, objID, typeExtentData)
		for _, it := range items {
			_ = v.deleteItem(btrfsKey{objectID: objID, itemType: typeExtentData, offset: it.offset})
		}
		return nil
	}
	return v.writeInodeItem(objID, &in)
}

func (v *Volume) RemoveAll(p string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.removeAll(p)
}

func (v *Volume) removeAll(p string) error {
	objID, err := v.lookupPath(p)
	if err != nil {
		return nil
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return err
	}
	if in.mode&ifmt == ifdir {
		entries, _ := v.readDirEntries(objID)
		for _, e := range entries {
			_ = v.removeAll(path.Join(p, e.name))
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

	oldDirObjID, err := v.lookupPathFollow(oldDir, 0)
	if err != nil {
		return err
	}
	newDirObjID, err := v.lookupPathFollow(newDir, 0)
	if err != nil {
		return err
	}
	srcObjID, err := v.lookupPath(oldpath)
	if err != nil {
		return err
	}
	in, err := v.readInodeItem(srcObjID)
	if err != nil {
		return err
	}

	dtype := dirReg
	if in.mode&ifmt == ifdir {
		dtype = dirDir
	} else if in.mode&ifmt == iflnk {
		dtype = dirLnk
	}

	// Remove destination if it exists.
	if _, err := v.lookupPath(newpath); err == nil {
		destObjID, _ := v.lookupPath(newpath)
		_ = v.removeDirEntry(newDirObjID, newBase, destObjID)
	}
	if err := v.addDirEntry(newDirObjID, newBase, srcObjID, uint8(dtype)); err != nil {
		return err
	}
	if err := v.removeDirEntry(oldDirObjID, oldBase, srcObjID); err != nil {
		return err
	}
	in.ctime = nowSec()
	return v.writeInodeItem(srcObjID, &in)
}

// ── Volume.Symlink ────────────────────────────────────────────────────────────

func (v *Volume) Symlink(oldname, newname string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	dir, base := path.Split(path.Clean("/" + newname))
	dirObjID, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return err
	}
	target := []byte(oldname)
	now := nowSec()
	newObjID := v.allocObjID()
	in := inodeItem{
		generation: v.sb.generation + 1,
		transID:    v.sb.generation + 1,
		size:       uint64(len(target)),
		nbytes:     uint64(len(target)),
		nlink:      1,
		mode:       uint32(iflnk) | 0777,
		atime:      now,
		ctime:      now,
		mtime:      now,
		otime:      now,
	}
	if err := v.writeInodeItem(newObjID, &in); err != nil {
		return err
	}
	if err := v.writeExtentData(newObjID, &in, target); err != nil {
		return err
	}
	return v.addDirEntry(dirObjID, base, newObjID, dirLnk)
}

// ── Volume.Link ───────────────────────────────────────────────────────────────

func (v *Volume) Link(oldname, newname string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	srcObjID, err := v.lookupPath(oldname)
	if err != nil {
		return err
	}
	in, err := v.readInodeItem(srcObjID)
	if err != nil {
		return err
	}
	if in.mode&ifmt == ifdir {
		return fmt.Errorf("btrfs: link: cannot hard-link directories")
	}
	newDir, newBase := path.Split(path.Clean("/" + newname))
	newDirObjID, err := v.lookupPathFollow(newDir, 0)
	if err != nil {
		return err
	}
	dtype := dirReg
	if in.mode&ifmt == iflnk {
		dtype = dirLnk
	}
	if err := v.addDirEntry(newDirObjID, newBase, srcObjID, uint8(dtype)); err != nil {
		return err
	}
	in.nlink++
	in.ctime = nowSec()
	return v.writeInodeItem(srcObjID, &in)
}