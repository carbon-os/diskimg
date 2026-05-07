package btrfs

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Unmount flushes all dirty state to the backing store and releases memory.
//
// When called on a subvolume (one created via OpenSubvol), the flush is
// delegated to the parent Volume so the root tree and superblock are written
// exactly once by the owner of the backing writer.
func (v *Volume) Unmount() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.parent != nil {
		// Subvolume: hand off to the parent which owns the writer.
		v.parent.mu.Lock()
		defer v.parent.mu.Unlock()
		return v.parent.flush()
	}
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
	key, data, err := v.findRootItem(v.rootTreeRoot, objFSTree)
	if err != nil {
		return err
	}
	if len(data) < rootItemBytNrOff+8 {
		return fmt.Errorf("btrfs: ROOT_ITEM too small")
	}
	current := binary.LittleEndian.Uint64(data[rootItemBytNrOff:])
	if current == v.fsTreeRoot {
		return nil
	}
	binary.LittleEndian.PutUint64(data[rootItemBytNrOff:], v.fsTreeRoot)
	return v.btreeInsert(&v.rootTreeRoot, key, data, v.sb.generation+1)
}

// writeSuperblock encodes and writes the primary superblock with incremented
// generation and recomputed checksum.
func (v *Volume) writeSuperblock() error {
	buf := make([]byte, sbSize)
	if _, err := v.sr.ReadAt(buf, sbOffset); err != nil {
		return fmt.Errorf("btrfs: read sb for update: %w", err)
	}
	le := binary.LittleEndian

	newGen := v.sb.generation + 1
	le.PutUint64(buf[0x48:], newGen)
	le.PutUint64(buf[0x50:], v.rootTreeRoot)

	v.checksumSuperblock(buf)

	if _, err := v.wa.WriteAt(buf, v.srOff+sbOffset); err != nil {
		return fmt.Errorf("btrfs: write superblock: %w", err)
	}
	return nil
}