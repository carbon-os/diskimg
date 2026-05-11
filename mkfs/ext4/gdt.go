package ext4

import (
	"encoding/binary"
	"hash/crc32"
)

// buildGDT serialises all group descriptors into a byte slice.
// It always produces gdtBlocks * blockSize bytes.
func (fs *fsLayout) buildGDT() []byte {
	size := fs.gdtBlocks * blockSize
	buf := make([]byte, size)
	for g := uint32(0); g < fs.numGroups; g++ {
		off := uint64(g) * gdtEntrySize
		fs.encodeGDE(buf[off:off+gdtEntrySize], g)
	}
	return buf
}

// encodeGDE fills exactly 64 bytes at dst with the group descriptor for group g.
func (fs *fsLayout) encodeGDE(dst []byte, g uint32) {
	gl := &fs.groups[g]
	le := binary.LittleEndian

	le.PutUint32(dst[0:], uint32(gl.blockBitmap))     // bg_block_bitmap_lo
	le.PutUint32(dst[4:], uint32(gl.inodeBitmap))     // bg_inode_bitmap_lo
	le.PutUint32(dst[8:], uint32(gl.inodeTable))      // bg_inode_table_lo
	le.PutUint16(dst[12:], uint16(gl.freeBlocks))     // bg_free_blocks_count_lo
	le.PutUint16(dst[14:], uint16(gl.freeInodes))     // bg_free_inodes_count_lo
	le.PutUint16(dst[16:], gl.usedDirs)               // bg_used_dirs_count_lo
	// bg_flags: EXT4_BG_INODE_ZEROED (0x4) — inode table was zeroed at mkfs
	le.PutUint16(dst[18:], 0x0004)
	// bg_exclude_bitmap_lo = 0 (offset 20)
	// bg_block_bitmap_csum_lo / hi — compute later (offset 24, 56)
	le.PutUint16(dst[28:], uint16(gl.freeInodes))     // bg_itable_unused_lo

	// 64-bit high halves
	le.PutUint32(dst[32:], uint32(gl.blockBitmap>>32))  // bg_block_bitmap_hi
	le.PutUint32(dst[36:], uint32(gl.inodeBitmap>>32))  // bg_inode_bitmap_hi
	le.PutUint32(dst[40:], uint32(gl.inodeTable>>32))   // bg_inode_table_hi
	le.PutUint16(dst[44:], uint16(gl.freeBlocks>>16))   // bg_free_blocks_count_hi
	le.PutUint16(dst[46:], uint16(gl.freeInodes>>16))   // bg_free_inodes_count_hi
	le.PutUint16(dst[48:], uint16(gl.usedDirs>>8))      // bg_used_dirs_count_hi (always 0)
	le.PutUint16(dst[50:], uint16(gl.freeInodes>>16))   // bg_itable_unused_hi

	// Compute block / inode bitmap checksums (crc32c over uuid+groupnum+bitmap).
	bbCsum := fs.bitmapCsum(g, true)
	ibCsum := fs.bitmapCsum(g, false)
	le.PutUint16(dst[24:], uint16(bbCsum))   // bg_block_bitmap_csum_lo
	le.PutUint16(dst[56:], uint16(bbCsum>>16)) // bg_block_bitmap_csum_hi
	le.PutUint16(dst[26:], uint16(ibCsum))   // bg_inode_bitmap_csum_lo
	le.PutUint16(dst[58:], uint16(ibCsum>>16)) // bg_inode_bitmap_csum_hi

	// Group descriptor checksum (bg_checksum at offset 30):
	// crc32c(seed + group_num_le32 + desc_with_csum_zeroed) & 0xFFFF
	le.PutUint16(dst[30:], 0) // zero csum field before computing
	var gnum [4]byte
	binary.LittleEndian.PutUint32(gnum[:], g)
	h := crc32cUpdate(fs.csumSeed, gnum[:])
	h = crc32cUpdate(h, dst[:64])
	le.PutUint16(dst[30:], uint16(h))
}

// bitmapCsum computes the crc32c checksum of a block or inode bitmap for group g.
// blockBitmap = true → block bitmap; false → inode bitmap.
func (fs *fsLayout) bitmapCsum(g uint32, blockBitmap bool) uint32 {
	var bitmap []byte
	if blockBitmap {
		bitmap = fs.buildBlockBitmap(g)
	} else {
		bitmap = fs.buildInodeBitmap(g)
	}
	var gnum [4]byte
	binary.LittleEndian.PutUint32(gnum[:], g)
	h := crc32cUpdate(fs.csumSeed, gnum[:])
	return crc32cUpdate(h, bitmap)
}