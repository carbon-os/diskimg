package mkfs

import (
	"encoding/binary"
	"time"
)

// writeSuperblock writes the ext4 superblock at byte offset 1024.
//
// Feature flags chosen for a minimal mountable filesystem:
//   FeatIncompat  = FileType (0x0002) | Extents (0x0040)
//   FeatROCompat  = SparseSuper (0x0001)
//   FeatCompat    = 0  (no journal)
func writeSuperblock(img []byte, l *Layout, uuid [16]byte, bm *bitmap, usedDirs uint32) {
	// The superblock structure starts at byte 1024 inside the partition.
	sb := img[1024:]
	le := binary.LittleEndian
	now := uint32(time.Now().Unix())

	freeBlocks := l.TotalBlocks - bm.usedBlocks()
	freeInodes := l.InodeCount - bm.usedInodes()

	le.PutUint32(sb[0x00:], l.InodeCount)    // s_inodes_count
	le.PutUint32(sb[0x04:], l.TotalBlocks)   // s_blocks_count_lo
	le.PutUint32(sb[0x08:], 0)               // s_r_blocks_count_lo (reserved = 0)
	le.PutUint32(sb[0x0C:], freeBlocks)      // s_free_blocks_count_lo
	le.PutUint32(sb[0x10:], freeInodes)      // s_free_inodes_count
	le.PutUint32(sb[0x14:], 0)              // s_first_data_block (0 for 4 K blocks)
	le.PutUint32(sb[0x18:], 2)              // s_log_block_size → 1024 << 2 = 4096
	le.PutUint32(sb[0x1C:], 2)              // s_log_cluster_size
	le.PutUint32(sb[0x20:], l.TotalBlocks)  // s_blocks_per_group  (single group)
	le.PutUint32(sb[0x24:], l.TotalBlocks)  // s_clusters_per_group
	le.PutUint32(sb[0x28:], l.InodeCount)   // s_inodes_per_group
	le.PutUint32(sb[0x2C:], now)            // s_mtime
	le.PutUint32(sb[0x30:], now)            // s_wtime
	le.PutUint16(sb[0x34:], 0)             // s_mnt_count
	le.PutUint16(sb[0x36:], 0xFFFF)        // s_max_mnt_count (unlimited)
	le.PutUint16(sb[0x38:], 0xEF53)        // s_magic
	le.PutUint16(sb[0x3A:], 1)             // s_state (1 = cleanly unmounted)
	le.PutUint16(sb[0x3C:], 1)             // s_errors (1 = continue)
	le.PutUint16(sb[0x3E:], 0)             // s_minor_rev_level
	le.PutUint32(sb[0x40:], now)           // s_lastcheck
	le.PutUint32(sb[0x44:], 0)             // s_checkinterval
	le.PutUint32(sb[0x48:], 0)             // s_creator_os (0 = Linux)
	le.PutUint32(sb[0x4C:], 1)             // s_rev_level (1 = dynamic)
	le.PutUint16(sb[0x50:], 0)             // s_def_resuid
	le.PutUint16(sb[0x52:], 0)             // s_def_resgid
	// Dynamic-rev fields (valid when s_rev_level >= 1):
	le.PutUint32(sb[0x54:], 11)             // s_first_ino
	le.PutUint16(sb[0x58:], uint16(InodeSize)) // s_inode_size
	le.PutUint16(sb[0x5A:], 0)              // s_block_group_nr
	le.PutUint32(sb[0x5C:], 0)              // s_feature_compat (no journal)
	le.PutUint32(sb[0x60:], 0x0002|0x0040) // s_feature_incompat: FileType|Extents
	le.PutUint32(sb[0x64:], 0x0001)         // s_feature_ro_compat: SparseSuper
	copy(sb[0x68:], uuid[:])                // s_uuid (16 bytes)
	// s_volume_name [0x78:0x88] left as zero bytes (unnamed)
}