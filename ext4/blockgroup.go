package ext4

import (
	"encoding/binary"
	"fmt"
	"io"
)

// BlockGroupDesc holds the parsed block group descriptor.
// Standard descriptors are 32 bytes; 64-byte form used when Incompat64Bit set.
//
// Layout (32-byte form):
//   [0–3]   bg_block_bitmap_lo
//   [4–7]   bg_inode_bitmap_lo
//   [8–11]  bg_inode_table_lo   ← block number of inode table
//   [12–13] bg_free_blocks_count_lo
//   [14–15] bg_free_inodes_count_lo
//   [16–17] bg_used_dirs_count_lo
//   [18–19] bg_flags
//   [20–23] bg_exclude_bitmap_lo
//   [24–25] bg_block_bitmap_csum_lo
//   [26–27] bg_inode_bitmap_csum_lo
//   [28–29] bg_itable_unused_lo
//   [30–31] bg_checksum
// 64-byte extension adds _hi counterparts at offsets 32–63.
type BlockGroupDesc struct {
	InodeTableLo uint32 // block number of inode table (lo 32 bits)
	InodeTableHi uint32 // block number of inode table (hi 32 bits, 64-bit only)
}

// InodeTableBlock returns the full 64-bit block number of this group's inode table.
func (bgd *BlockGroupDesc) InodeTableBlock() uint64 {
	return uint64(bgd.InodeTableHi)<<32 | uint64(bgd.InodeTableLo)
}

// ReadBlockGroupDesc reads the block group descriptor for group groupIdx.
func ReadBlockGroupDesc(r io.ReaderAt, sb *Superblock, groupIdx uint32) (*BlockGroupDesc, error) {
	gdtByteOffset := int64(sb.GDTBlock) * sb.BlockSize
	entryOffset := gdtByteOffset + int64(groupIdx)*int64(sb.GDTSize)

	buf := make([]byte, sb.GDTSize)
	if _, err := r.ReadAt(buf, entryOffset); err != nil {
		return nil, fmt.Errorf("ext4: read bgd[%d]: %w", groupIdx, err)
	}

	bgd := &BlockGroupDesc{
		InodeTableLo: binary.LittleEndian.Uint32(buf[8:12]),
	}
	if sb.GDTSize >= 64 {
		bgd.InodeTableHi = binary.LittleEndian.Uint32(buf[40:44])
	}
	return bgd, nil
}