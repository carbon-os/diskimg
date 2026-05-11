package ext4

import (
	"encoding/binary"
)

// Inode mode bits
const (
	S_IFREG  = 0x8000
	S_IFDIR  = 0x4000
	S_IFLNK  = 0xA000
	modeDIR  = S_IFDIR | 0755
	modeREG  = S_IFREG | 0600
)

// Inode flags
const (
	EXT4_EXTENTS_FL = 0x00080000
	EXT4_EA_INODE_FL = 0x00200000
)

// buildInodeTable returns the complete inode table for group g.
// For g == 0 we pre-populate the reserved inodes (1–11).
func (fs *fsLayout) buildInodeTable(g uint32) []byte {
	sz := uint64(fs.inodesPerGrp) * inodeSize
	buf := make([]byte, sz)
	if g != 0 {
		return buf
	}

	// Helper: write inode at 0-based index idx (within group 0).
	put := func(idx uint32, mode uint16, size uint64, flags uint32, blkData []byte) {
		off := uint64(idx) * inodeSize
		inode := buf[off : off+inodeSize]
		le := binary.LittleEndian
		now32 := uint32(fs.now)
		le.PutUint16(inode[0:], mode)         // i_mode
		le.PutUint16(inode[2:], 0)            // i_uid lo
		le.PutUint32(inode[4:], uint32(size)) // i_size_lo
		le.PutUint32(inode[8:], now32)         // i_atime
		le.PutUint32(inode[12:], now32)        // i_ctime
		le.PutUint32(inode[16:], now32)        // i_mtime
		le.PutUint32(inode[20:], 0)            // i_dtime
		le.PutUint16(inode[24:], 0)            // i_gid lo
		le.PutUint16(inode[26:], 2)            // i_links_count (≥1)
		le.PutUint32(inode[28:], 0)            // i_blocks_lo (in 512-byte units; we use extents)
		le.PutUint32(inode[32:], flags)        // i_flags
		copy(inode[40:100], blkData)           // i_block[15] = 60 bytes
		le.PutUint16(inode[128:], 28)          // i_extra_isize
		le.PutUint32(inode[132:], now32)        // i_ctime_extra
		le.PutUint32(inode[136:], now32)        // i_mtime_extra
		le.PutUint32(inode[140:], now32)        // i_atime_extra
		le.PutUint32(inode[144:], now32)        // i_crtime
		le.PutUint32(inode[148:], now32)        // i_crtime_extra
		le.PutUint32(inode[152:], uint32(size>>32)) // i_size_high

		// Inode checksum: crc32c(seed + inode_num_le32 + inode_gen_le32 + inode)
		inodeNum := uint32(g*fs.inodesPerGrp) + idx + 1 // 1-based
		var numBuf [4]byte
		binary.LittleEndian.PutUint32(numBuf[:], inodeNum)
		h := crc32cUpdate(fs.csumSeed, numBuf[:])
		var genBuf [4]byte // generation = 0
		h = crc32cUpdate(h, genBuf[:])
		// checksum covers first 128 bytes + extra_isize (28 extra bytes = 156 total).
		// Place lo16 at offset 0x74 (116) and hi16 at offset 0x82 (130).
		h = crc32cUpdate(h, inode[:128])
		h = crc32cUpdate(h, inode[128:128+2]) // i_extra_isize
		h = crc32cUpdate(h, inode[130:156])   // 26 bytes of extra fields
		le.PutUint16(inode[116:], uint16(h))
		le.PutUint16(inode[130:], uint16(h>>16))
	}

	// ── Inode 1: bad blocks (empty, regular file) ──────────────────────────
	put(0, modeREG, 0, EXT4_EXTENTS_FL, buildEmptyExtentTree())

	// ── Inode 2: root directory ────────────────────────────────────────────
	rootExt := buildExtentLeaf(0, 1, fs.rootDirBlock)
	put(1, modeDIR, blockSize, EXT4_EXTENTS_FL, rootExt[:])

	// ── Inodes 3–7: reserved (empty regular files) ────────────────────────
	for i := uint32(2); i <= 6; i++ {
		put(i, modeREG, 0, EXT4_EXTENTS_FL, buildEmptyExtentTree())
	}

	// ── Inode 8: journal ───────────────────────────────────────────────────
	jExt := buildExtentLeaf(0, fs.journalSize, fs.journalBlock)
	jSize := uint64(fs.journalSize) * blockSize
	// i_blocks_lo for journal = journalSize * (blockSize/512)
	inode8 := buf[7*inodeSize : 8*inodeSize]
	binary.LittleEndian.PutUint32(inode8[28:], uint32(fs.journalSize*(blockSize/512)))
	// We'll call put() but need to patch i_blocks_lo after:
	put(7, modeREG, jSize, EXT4_EXTENTS_FL, jExt[:])
	binary.LittleEndian.PutUint32(inode8[28:], fs.journalSize*(blockSize/512))

	// ── Inodes 9–10: exclude, replica (empty) ─────────────────────────────
	put(8, modeREG, 0, EXT4_EXTENTS_FL, buildEmptyExtentTree())
	put(9, modeREG, 0, EXT4_EXTENTS_FL, buildEmptyExtentTree())

	// ── Inode 11: lost+found ───────────────────────────────────────────────
	lfExt := buildExtentLeaf(0, 1, fs.lfDirBlock)
	put(10, modeDIR, blockSize, EXT4_EXTENTS_FL, lfExt[:])

	return buf
}

// buildEmptyExtentTree returns 60 bytes encoding a valid but empty extent tree.
func buildEmptyExtentTree() []byte {
	b := make([]byte, 60)
	binary.LittleEndian.PutUint16(b[0:], 0xF30A) // eh_magic
	binary.LittleEndian.PutUint16(b[2:], 0)      // eh_entries = 0
	binary.LittleEndian.PutUint16(b[4:], 4)      // eh_max = 4
	binary.LittleEndian.PutUint16(b[6:], 0)      // eh_depth = 0 (leaf)
	return b
}

// buildExtentLeaf returns a 60-byte i_block[] encoding a single-leaf extent
// tree with one extent covering logicalStart..logicalStart+numBlocks-1 mapped
// to physBlock.
func buildExtentLeaf(logicalStart uint32, numBlocks uint32, physBlock uint64) [60]byte {
	var b [60]byte
	le := binary.LittleEndian
	// extent header
	le.PutUint16(b[0:], 0xF30A)  // eh_magic
	le.PutUint16(b[2:], 1)       // eh_entries = 1
	le.PutUint16(b[4:], 4)       // eh_max = 4
	le.PutUint16(b[6:], 0)       // eh_depth = 0 (leaf)
	le.PutUint32(b[8:], 0)       // eh_generation
	// ext4_extent at offset 12
	le.PutUint32(b[12:], logicalStart)           // ee_block
	le.PutUint16(b[16:], uint16(numBlocks))      // ee_len (≤32768 = initialized)
	le.PutUint16(b[18:], uint16(physBlock>>32))  // ee_start_hi
	le.PutUint32(b[20:], uint32(physBlock))      // ee_start_lo
	return b
}