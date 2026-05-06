package mkfs

import "encoding/binary"

// writeGDT writes the single group descriptor table entry at block 1.
// We do not set the 64BIT incompat flag, so each descriptor is 32 bytes.
//
// 32-byte group descriptor layout:
//   [0–3]   bg_block_bitmap_lo
//   [4–7]   bg_inode_bitmap_lo
//   [8–11]  bg_inode_table_lo
//   [12–13] bg_free_blocks_count_lo
//   [14–15] bg_free_inodes_count_lo
//   [16–17] bg_used_dirs_count_lo
//   [18–31] flags, checksums — all zero for our use case
func writeGDT(img []byte, l *Layout, bm *bitmap, usedDirs uint32) {
	gdt := img[BlockSize : 2*BlockSize] // block 1
	le := binary.LittleEndian

	freeBlocks := l.TotalBlocks - bm.usedBlocks()
	freeInodes := l.InodeCount - bm.usedInodes()

	le.PutUint32(gdt[0:], l.BlockBitmapBlock)   // bg_block_bitmap_lo
	le.PutUint32(gdt[4:], l.InodeBitmapBlock)   // bg_inode_bitmap_lo
	le.PutUint32(gdt[8:], l.InodeTableBlock)    // bg_inode_table_lo
	le.PutUint16(gdt[12:], uint16(freeBlocks))  // bg_free_blocks_count_lo
	le.PutUint16(gdt[14:], uint16(freeInodes))  // bg_free_inodes_count_lo
	le.PutUint16(gdt[16:], uint16(usedDirs))    // bg_used_dirs_count_lo
	// bg_flags, checksums — remain zero
}