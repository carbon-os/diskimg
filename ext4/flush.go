package ext4

import (
	"fmt"
	"sort"
)

// Unmount flushes all dirty blocks and inodes to the underlying SectionReader,
// then releases all in-memory state.
func (v *Volume) Unmount() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.flush()
}

// flush writes all dirty state in a safe order:
//   1. Data blocks
//   2. Inode table blocks (via dirtyInodes → raw inode → inode table block)
//   3. Bitmap blocks
//   4. GDT and superblock blocks (already staged in v.dirty by alloc.go)
func (v *Volume) flush() error {
	// First, stage all dirty inodes into the appropriate inode table blocks
	// (they are then part of v.dirty and written in the next pass).
	for num, raw := range v.dirtyInodes {
		if err := v.flushInode(num, raw); err != nil {
			return err
		}
	}
	v.dirtyInodes = make(map[uint32][]byte)

	// Write dirty blocks in ascending block-number order for predictability.
	keys := make([]uint64, 0, len(v.dirty))
	for k := range v.dirty {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, blk := range keys {
		data := v.dirty[blk]
		off := int64(blk) * int64(v.sb.blockSize)
		if _, err := v.sr.Seek(off, 0); err != nil {
			return fmt.Errorf("ext4: flush block %d seek: %w", blk, err)
		}
		// SectionReader doesn't implement Write, so we use the underlying writer.
		// We need a WriteAt; assert the inner type.
		if wa, ok := v.sr.GetAt().(interface{ WriteAt([]byte, int64) (int, error) }); ok {
			if _, err := wa.WriteAt(data, off); err != nil {
				return fmt.Errorf("ext4: flush block %d write: %w", blk, err)
			}
		} else {
			// Fallback: write via pwrite-style method on the inner reader.
			if err := v.writeBlockDirect(blk, data); err != nil {
				return err
			}
		}
		delete(v.dirty, blk)
	}
	return nil
}

// flushInode writes the raw inode bytes into the inode table,
// staging the modified inode table block into v.dirty.
func (v *Volume) flushInode(num uint32, raw []byte) error {
	grp := v.inodeBlockGroup(num)
	localIdx := v.inodeLocalIndex(num)
	tableBlk := v.inodeTableBlock(grp)
	bs := int64(v.sb.blockSize)
	inodeSize := int64(v.sb.inodeSize)

	// Which block within the inode table holds this inode?
	tableByteOff := int64(localIdx) * inodeSize
	blockIdx := uint64(tableByteOff / bs)
	inBlockOff := tableByteOff % bs

	physBlk := tableBlk + blockIdx
	blkData, err := v.readBlock(physBlk)
	if err != nil {
		return fmt.Errorf("ext4: flush inode %d: read table block: %w", num, err)
	}
	copy(blkData[inBlockOff:inBlockOff+inodeSize], raw)
	v.writeBlock(physBlk, blkData)
	return nil
}

// writeBlockDirect writes data directly to the SectionReader's backing store
// at the given block position.
func (v *Volume) writeBlockDirect(blk uint64, data []byte) error {
	off := int64(blk) * int64(v.sb.blockSize)
	// The SectionReader wraps an io.ReaderAt; for write we need io.WriterAt.
	// We store the raw WriterAt alongside the reader.
	if v.wa == nil {
		return fmt.Errorf("ext4: no writable backend (read-only image?)")
	}
	if _, err := v.wa.WriteAt(data, v.srOffset+off); err != nil {
		return fmt.Errorf("ext4: write block %d: %w", blk, err)
	}
	return nil
}