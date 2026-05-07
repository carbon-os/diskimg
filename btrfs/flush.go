package btrfs

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Unmount flushes all dirty state to the backing store and releases memory.
func (v *Volume) Unmount() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.flush()
}

// flush writes dirty nodes and the updated superblock to disk in a safe order.
func (v *Volume) flush() error {
	if v.wa == nil {
		return fmt.Errorf("btrfs: no writable backend (read-only image?)")
	}

	// 1. Update ROOT_ITEM in the root tree if the FS tree root changed.
	if err := v.updateRootItem(); err != nil {
		return err
	}

	// 2. Write dirty node blocks in ascending physical-offset order.
	keys := make([]int64, 0, len(v.dirty))
	for k := range v.dirty {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, phys := range keys {
		data := v.dirty[phys]
		if _, err := v.wa.WriteAt(data, v.srOff+phys); err != nil {
			return fmt.Errorf("btrfs: write block @%#x: %w", phys, err)
		}
		delete(v.dirty, phys)
	}

	// 3. Write the updated superblock.
	return v.writeSuperblock()
}

// updateRootItem re-reads the root tree node that contains ROOT_ITEM for the
// FS tree and patches bytenr to reflect any root change from splits.
func (v *Volume) updateRootItem() error {
	key := btrfsKey{objectID: objFSTree, itemType: typeRootItem, offset: 0}
	data, ok, err := v.searchTree(v.rootTreeRoot, key)
	if err != nil || !ok || len(data) < rootItemBytNrOff+8 {
		return err // nothing to update or tree unreadable
	}
	current := binary.LittleEndian.Uint64(data[rootItemBytNrOff:])
	if current == v.fsTreeRoot {
		return nil // no change
	}

	// Patch bytenr in the ROOT_ITEM payload and re-insert.
	binary.LittleEndian.PutUint64(data[rootItemBytNrOff:], v.fsTreeRoot)
	return v.btreeInsert(&v.rootTreeRoot, key, data, v.sb.generation+1)
}

// writeSuperblock encodes and writes the primary superblock with incremented
// generation and recomputed checksum.
func (v *Volume) writeSuperblock() error {
	buf := make([]byte, sbSize)
	// Read the existing superblock so we preserve fields we don't track.
	if _, err := v.sr.ReadAt(buf, sbOffset); err != nil {
		return fmt.Errorf("btrfs: read sb for update: %w", err)
	}
	le := binary.LittleEndian

	// Bump generation.
	newGen := v.sb.generation + 1
	le.PutUint64(buf[0x48:], newGen)

	// Update root tree root pointer (may have changed due to splits).
	le.PutUint64(buf[0x50:], v.rootTreeRoot)

	// Update bytes_used: old value + number of new dirty bytes.
	// (Simplified: add nodeSize * number of newly allocated nodes.)
	// For correctness we just leave bytes_used as-is; the kernel recalculates.

	// Recompute checksum.
	v.checksumSuperblock(buf)

	if _, err := v.wa.WriteAt(buf, v.srOff+sbOffset); err != nil {
		return fmt.Errorf("btrfs: write superblock: %w", err)
	}
	return nil
}