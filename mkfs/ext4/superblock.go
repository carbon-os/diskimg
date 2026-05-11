package ext4

import (
	"encoding/binary"
	"hash/crc32"
)

// Feature flag constants
const (
	// COMPAT
	compatHasJournal  = 0x0004
	compatExtAttr     = 0x0008
	compatResizeInode = 0x0010
	compatDirIndex    = 0x0020

	// INCOMPAT
	incompatFiletype  = 0x0002
	incompatExtents   = 0x0040
	incompat64bit     = 0x0080
	incompatFlexBG    = 0x0200

	// RO_COMPAT
	roCompatSparseSB  = 0x0001
	roCompatLargeFile = 0x0002
	roCompatHugeFile  = 0x0008
	roCompatDirNlink  = 0x0020
	roCompatExtraIsize = 0x0040
	roCompatMetadataCsum = 0x0400

	sbMagic = 0xEF53
)

// buildSuperblock constructs a 1024-byte superblock for block group g.
// For g == 0 this is the primary; for g > 0 it is a backup copy with
// s_block_group_nr set accordingly.
func (fs *fsLayout) buildSuperblock(g uint32) []byte {
	sb := make([]byte, 1024)
	le := binary.LittleEndian

	now32 := uint32(fs.now)
	reservedBlocks := uint64(fs.totalBlocks) * uint64(fs.opts.ReservedPct) / 100

	// Count free blocks across all groups
	var freeBlocks uint64
	for i := range fs.groups {
		freeBlocks += uint64(fs.groups[i].freeBlocks)
	}
	var freeInodes uint32
	for i := range fs.groups {
		freeInodes += fs.groups[i].freeInodes
	}

	le.PutUint32(sb[0:], fs.totalInodes)           // s_inodes_count
	le.PutUint32(sb[4:], uint32(fs.totalBlocks))   // s_blocks_count_lo
	le.PutUint32(sb[8:], uint32(reservedBlocks))   // s_r_blocks_count_lo
	le.PutUint32(sb[12:], uint32(freeBlocks))       // s_free_blocks_count_lo
	le.PutUint32(sb[16:], freeInodes)              // s_free_inodes_count
	le.PutUint32(sb[20:], 0)                       // s_first_data_block (0 for 4K blocks)
	le.PutUint32(sb[24:], 2)                       // s_log_block_size (2 → 4096)
	le.PutUint32(sb[28:], 2)                       // s_log_cluster_size (= s_log_block_size)
	le.PutUint32(sb[32:], blocksPerGroup)          // s_blocks_per_group
	le.PutUint32(sb[36:], blocksPerGroup)          // s_clusters_per_group
	le.PutUint32(sb[40:], fs.inodesPerGrp)        // s_inodes_per_group
	le.PutUint32(sb[44:], now32)                   // s_mtime
	le.PutUint32(sb[48:], now32)                   // s_wtime
	le.PutUint16(sb[52:], 0)                       // s_mnt_count
	le.PutUint16(sb[54:], 0xFFFF)                  // s_max_mnt_count (-1 = disabled)
	le.PutUint16(sb[56:], sbMagic)                 // s_magic
	le.PutUint16(sb[58:], 0x0001)                  // s_state = cleanly unmounted
	le.PutUint16(sb[60:], 1)                       // s_errors = continue
	le.PutUint16(sb[62:], 0)                       // s_minor_rev_level
	le.PutUint32(sb[64:], now32)                   // s_lastcheck
	le.PutUint32(sb[68:], 0)                       // s_checkinterval
	le.PutUint32(sb[72:], 0)                       // s_creator_os = Linux
	le.PutUint32(sb[76:], 1)                       // s_rev_level = EXT4_DYNAMIC_REV
	le.PutUint16(sb[80:], 0)                       // s_def_resuid
	le.PutUint16(sb[82:], 0)                       // s_def_resgid
	le.PutUint32(sb[84:], inoFirstNonReserved)     // s_first_ino = 11
	le.PutUint16(sb[88:], inodeSize)               // s_inode_size
	le.PutUint16(sb[90:], uint16(g))               // s_block_group_nr
	le.PutUint32(sb[92:], compatHasJournal|compatExtAttr|compatResizeInode|compatDirIndex)
	le.PutUint32(sb[96:], incompatFiletype|incompatExtents|incompat64bit|incompatFlexBG)
	le.PutUint32(sb[100:], roCompatSparseSB|roCompatLargeFile|roCompatHugeFile|
		roCompatDirNlink|roCompatExtraIsize|roCompatMetadataCsum)
	copy(sb[104:120], fs.opts.UUID[:])             // s_uuid
	padLabel(sb[120:136], fs.opts.Label)           // s_volume_name
	// s_last_mounted[64] at 136 — leave zero
	// s_algorithm_usage_bitmap at 200 — zero
	le.PutUint16(sb[222:], uint16(fs.opts.reservedGDT)) // s_reserved_gdt_blocks
	// s_journal_uuid at 208..224 — leave zero (internal journal)
	le.PutUint32(sb[224:], inoJournal)             // s_journal_inum = 8
	le.PutUint32(sb[232:], 0)                      // s_last_orphan
	// s_hash_seed at 236..252 — zero OK
	sb[252] = 1                                    // s_def_hash_version = half_md4
	sb[253] = 1                                    // s_jnl_backup_type
	le.PutUint16(sb[254:], gdtEntrySize)           // s_desc_size = 64
	le.PutUint32(sb[256:], 0x0C)                   // s_default_mount_opts: user_xattr|acl
	le.PutUint32(sb[260:], 0)                      // s_first_meta_bg
	le.PutUint32(sb[264:], now32)                  // s_mkfs_time
	// s_jnl_blocks[17] at 268..336 — set journal inode i_block/i_size
	fs.writeJournalInodeBackup(sb[268:])
	// 64-bit high fields
	le.PutUint32(sb[336:], uint32(fs.totalBlocks>>32))  // s_blocks_count_hi
	le.PutUint32(sb[340:], uint32(reservedBlocks>>32))  // s_r_blocks_count_hi
	le.PutUint32(sb[344:], uint32(freeBlocks>>32))      // s_free_blocks_count_hi
	le.PutUint16(sb[348:], 28)                          // s_min_extra_isize
	le.PutUint16(sb[350:], 28)                          // s_want_extra_isize
	sb[372] = flexGroupLog                               // s_log_groups_per_flex
	sb[373] = 1                                         // s_checksum_type = crc32c
	le.PutUint32(sb[624:], fs.csumSeed)                 // s_checksum_seed
	le.PutUint32(sb[396:], uint32(fs.numGroups))        // s_lpf_ino — not quite but harmless

	// Superblock checksum (last 4 bytes of the 1024-byte block)
	// The spec says checksum covers the whole SB with the csum field set to 0.
	csum := crc32cUpdate(fs.csumSeed, sb[:1020])
	le.PutUint32(sb[1020:], csum)

	return sb
}

// writeJournalInodeBackup encodes the journal inode's first 15 i_block[]
// entries + i_size into the s_jnl_blocks[17] SB field (68 bytes).
func (fs *fsLayout) writeJournalInodeBackup(dst []byte) {
	le := binary.LittleEndian
	// Build a fake extent header + one extent pointing at journalBlock.
	// i_block[0..3] = extent header + one leaf
	// i_block[4..14] = zero
	// i_size_high = i_size[16], i_size = i_size[17]
	// (indices in the backup are 0-based: entry 0 = i_block[0])
	ext := buildExtentLeaf(0, uint32(fs.journalSize), fs.journalBlock)
	copy(dst[0:], ext[:])
	jSize := uint64(fs.journalSize) * blockSize
	le.PutUint32(dst[60:], uint32(jSize>>32)) // i_size_high (entry 15)
	le.PutUint32(dst[64:], uint32(jSize))     // i_size lo  (entry 16)
}

func padLabel(dst []byte, s string) {
	for i := range dst {
		dst[i] = 0
	}
	if len(s) > len(dst) {
		s = s[:len(dst)]
	}
	copy(dst, s)
}

const inoFirstNonReserved = 11