package xfs

import (
	"fmt"
	"sort"
)

// Unmount flushes all dirty blocks and inodes to the backing store,
// then releases all in-memory state.
func (v *Volume) Unmount() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.flush()
}

// flush writes all dirty state in a safe order:
//  1. Dirty inodes → staged into their inode-table blocks → join v.dirty
//  2. All dirty blocks in ascending order
func (v *Volume) flush() error {
	// Stage dirty inodes into their containing inode-table blocks.
	for ino, raw := range v.dirtyInodes {
		if err := v.flushInode(ino, raw); err != nil {
			return err
		}
	}
	v.dirtyInodes = make(map[uint64][]byte)

	// Write dirty blocks in ascending order.
	keys := make([]uint64, 0, len(v.dirty))
	for k := range v.dirty {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, blk := range keys {
		data := v.dirty[blk]
		if err := v.writeBlockDirect(blk, data); err != nil {
			return err
		}
		delete(v.dirty, blk)
	}
	return nil
}

// flushInode copies raw inode bytes into the inode-table block and stages it.
func (v *Volume) flushInode(ino uint64, raw []byte) error {
	blk, offInBlk := v.inodePhysBlock(ino)
	blkData, err := v.readBlock(blk)
	if err != nil {
		return fmt.Errorf("xfs: flush inode %d: %w", ino, err)
	}
	copy(blkData[offInBlk:offInBlk+int64(v.sb.inodeSize)], raw)
	v.writeBlock(blk, blkData)
	return nil
}

// writeBlockDirect writes a block directly to the backing WriterAt.
func (v *Volume) writeBlockDirect(blk uint64, data []byte) error {
	if v.wa == nil {
		return fmt.Errorf("xfs: no writable backend (read-only image?)")
	}
	off := int64(blk)*int64(v.sb.blockSize) + v.srOffset
	if _, err := v.wa.WriteAt(data, off); err != nil {
		return fmt.Errorf("xfs: write block %d: %w", blk, err)
	}
	return nil
}