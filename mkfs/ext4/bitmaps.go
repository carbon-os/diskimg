package ext4

// buildBlockBitmap returns a blockSize-byte block bitmap for group g.
// All metadata blocks (SB, GDT, reserved GDT, bitmaps, inode table) and
// any pre-allocated data blocks (journal, root dir, lost+found) are marked used.
func (fs *fsLayout) buildBlockBitmap(g uint32) []byte {
	bm := make([]byte, blockSize)
	gl := &fs.groups[g]

	// Mark blocks [firstBlock .. dataStart) as used (metadata overhead).
	// After dataStart the blocks are free — except in group 0 where journal,
	// root dir and lost+found have already advanced dataStart past them.
	used := gl.dataStart - gl.firstBlock
	setBits(bm, 0, used)

	// If the group is the last one and doesn't fill a full blocksPerGroup,
	// bits beyond the last real block must be marked used (unusable padding).
	groupBlocks := gl.lastBlock - gl.firstBlock + 1
	if groupBlocks < blocksPerGroup {
		setBits(bm, groupBlocks, uint64(blocksPerGroup)-groupBlocks)
	}

	return bm
}

// buildInodeBitmap returns a blockSize-byte inode bitmap for group g.
// For group 0, inodes 1–inoLostFound (1..11) are marked used.
// All other groups have all inodes free.
func (fs *fsLayout) buildInodeBitmap(g uint32) []byte {
	bm := make([]byte, blockSize)
	if g == 0 {
		setBits(bm, 0, uint64(inoLostFound)) // inodes 1..11 (0-based bits 0..10)
	}
	// Mark bits beyond inodesPerGrp as used (unusable padding).
	if fs.inodesPerGrp < uint32(blocksPerGroup) {
		setBits(bm, uint64(fs.inodesPerGrp), uint64(blocksPerGroup)-uint64(fs.inodesPerGrp))
	}
	return bm
}

// setBits sets count bits starting at bitOffset in bm.
func setBits(bm []byte, bitOffset, count uint64) {
	for i := uint64(0); i < count; i++ {
		b := bitOffset + i
		bm[b/8] |= 1 << (b % 8)
	}
}