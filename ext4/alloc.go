package ext4

import (
	"encoding/binary"
	"fmt"
	"time"
)

// ── block allocator ───────────────────────────────────────────────────────────

// allocBlock finds and marks a free block in the filesystem, preferring the
// given block group.  Returns the physical block number.
func (v *Volume) allocBlock(preferGroup uint32) (uint64, error) {
	groups := uint32(len(v.groups))
	for i := uint32(0); i < groups; i++ {
		grp := (preferGroup + i) % groups
		blk, err := v.allocBlockInGroup(grp)
		if err == nil {
			return blk, nil
		}
	}
	return 0, fmt.Errorf("ext4: no free blocks")
}

func (v *Volume) allocBlockInGroup(grp uint32) (uint64, error) {
	gd := &v.groups[grp]
	free := uint32(gd.freeBlocksLo) | uint32(gd.freeBlocksHi)<<16
	if free == 0 {
		return 0, fmt.Errorf("group %d full", grp)
	}

	bitmapBlk := uint64(gd.blockBitmapLo) | uint64(gd.blockBitmapHi)<<32
	bitmap, err := v.readBlock(bitmapBlk)
	if err != nil {
		return 0, err
	}

	// Find the first clear bit.
	bit := findFreeBit(bitmap, int(v.sb.blocksPerGroup))
	if bit < 0 {
		return 0, fmt.Errorf("group %d bitmap has no free bit", grp)
	}
	setBit(bitmap, bit)
	v.writeBlock(bitmapBlk, bitmap)

	// Update group descriptor free count.
	newFree := free - 1
	gd.freeBlocksLo = uint16(newFree)
	gd.freeBlocksHi = uint16(newFree >> 16)
	v.writeGroupDesc(grp, gd)

	// Update superblock free blocks.
	v.sb.freeBlocksLo--
	v.writeSuperblock()

	// Calculate physical block number.
	startBlock := uint64(grp) * uint64(v.sb.blocksPerGroup)
	if v.sb.firstDataBlock == 1 {
		startBlock++ // 1K block size offset
	}
	return startBlock + uint64(bit), nil
}

// freeBlock marks physBlock as free in the bitmap and updates counts.
func (v *Volume) freeBlock(physBlock uint64) error {
	bs := uint64(v.sb.blocksPerGroup)
	start := uint64(0)
	if v.sb.firstDataBlock == 1 {
		start = 1
	}
	grp := uint32((physBlock - start) / bs)
	bit := int((physBlock - start) % bs)

	gd := &v.groups[grp]
	bitmapBlk := uint64(gd.blockBitmapLo) | uint64(gd.blockBitmapHi)<<32
	bitmap, err := v.readBlock(bitmapBlk)
	if err != nil {
		return err
	}
	clearBit(bitmap, bit)
	v.writeBlock(bitmapBlk, bitmap)

	free := uint32(gd.freeBlocksLo) | uint32(gd.freeBlocksHi)<<16
	free++
	gd.freeBlocksLo = uint16(free)
	gd.freeBlocksHi = uint16(free >> 16)
	v.writeGroupDesc(grp, gd)

	v.sb.freeBlocksLo++
	v.writeSuperblock()
	return nil
}

// ── inode allocator ───────────────────────────────────────────────────────────

// allocInode finds and marks a free inode, preferring preferGroup.
// For directories the allocator scans for the least-loaded group.
func (v *Volume) allocInode(preferGroup uint32, isDir bool) (uint32, error) {
	groups := uint32(len(v.groups))

	startGroup := preferGroup
	if isDir {
		// Pick the group with the most free inodes for directory locality.
		best := preferGroup
		bestFree := uint32(0)
		for g := uint32(0); g < groups; g++ {
			gd := &v.groups[g]
			f := uint32(gd.freeInodesLo)
			if f > bestFree {
				bestFree = f
				best = g
			}
		}
		startGroup = best
	}

	for i := uint32(0); i < groups; i++ {
		grp := (startGroup + i) % groups
		ino, err := v.allocInodeInGroup(grp, isDir)
		if err == nil {
			return ino, nil
		}
	}
	return 0, fmt.Errorf("ext4: no free inodes")
}

func (v *Volume) allocInodeInGroup(grp uint32, isDir bool) (uint32, error) {
	gd := &v.groups[grp]
	if gd.freeInodesLo == 0 {
		return 0, fmt.Errorf("group %d inode table full", grp)
	}

	bitmapBlk := uint64(gd.inodeBitmapLo) | uint64(gd.inodeBitmapHi)<<32
	bitmap, err := v.readBlock(bitmapBlk)
	if err != nil {
		return 0, err
	}

	bit := findFreeBit(bitmap, int(v.sb.inodesPerGroup))
	if bit < 0 {
		return 0, fmt.Errorf("group %d inode bitmap full", grp)
	}
	setBit(bitmap, bit)
	v.writeBlock(bitmapBlk, bitmap)

	gd.freeInodesLo--
	if isDir {
		gd.usedDirsLo++
	}
	v.writeGroupDesc(grp, gd)

	v.sb.freeInodes--
	v.writeSuperblock()

	// Inode numbers are 1-based.
	return grp*v.sb.inodesPerGroup + uint32(bit) + 1, nil
}

// freeInode marks ino as free in the inode bitmap.
func (v *Volume) freeInode(ino uint32) {
	grp := v.inodeBlockGroup(ino)
	bit := int(v.inodeLocalIndex(ino))
	gd := &v.groups[grp]

	bitmapBlk := uint64(gd.inodeBitmapLo) | uint64(gd.inodeBitmapHi)<<32
	bitmap, err := v.readBlock(bitmapBlk)
	if err != nil {
		return
	}
	clearBit(bitmap, bit)
	v.writeBlock(bitmapBlk, bitmap)

	gd.freeInodesLo++
	v.writeGroupDesc(grp, gd)
	v.sb.freeInodes++
	v.writeSuperblock()
}

// ── bitmap helpers ────────────────────────────────────────────────────────────

func findFreeBit(bitmap []byte, limit int) int {
	for i := 0; i < limit; i++ {
		if bitmap[i/8]>>(uint(i%8))&1 == 0 {
			return i
		}
	}
	return -1
}

func setBit(bitmap []byte, bit int) {
	bitmap[bit/8] |= 1 << uint(bit%8)
}

func clearBit(bitmap []byte, bit int) {
	bitmap[bit/8] &^= 1 << uint(bit%8)
}

// ── superblock / GDT write-back ───────────────────────────────────────────────

// writeSuperblock stages the in-memory superblock back to block 0 (primary).
func (v *Volume) writeSuperblock() {
	buf := make([]byte, 1024)
	// Read existing bytes first so we only modify the fields we track.
	existing := make([]byte, 1024)
	v.sr.ReadAt(existing, sbOffset)
	copy(buf, existing)

	le := binary.LittleEndian
	le.PutUint32(buf[0:4], v.sb.inodesCount)
	le.PutUint32(buf[4:8], v.sb.blocksCountLo)
	le.PutUint32(buf[12:16], v.sb.freeBlocksLo)
	le.PutUint32(buf[16:20], v.sb.freeInodes)
	le.PutUint32(buf[48:52], uint32(time.Now().Unix())) // s_wtime

	// Stage into the dirty block covering the superblock offset.
	// Superblock lives at byte 1024; for blockSize ≥ 2048, it's in block 0.
	// For 1K block size, it's in block 1.
	var sbBlock uint64 = 0
	if v.sb.blockSize == 1024 {
		sbBlock = 1
	}
	blkBuf, _ := v.readBlock(sbBlock)
	inBlockOff := int64(sbOffset) % int64(v.sb.blockSize)
	copy(blkBuf[inBlockOff:inBlockOff+1024], buf)
	v.writeBlock(sbBlock, blkBuf)
}

// writeGroupDesc stages the updated group descriptor to the GDT dirty cache.
func (v *Volume) writeGroupDesc(grp uint32, gd *groupDesc) {
	v.groups[grp] = *gd

	// Stage GDT block(s) into dirty cache.
	gdtBlock := uint64(v.sb.firstDataBlock) + 1
	ds := int(v.sb.descSize)
	gdtByteOff := int64(grp) * int64(ds)
	gdtBlkNum := gdtBlock + uint64(gdtByteOff)/uint64(v.sb.blockSize)
	gdtBuf, _ := v.readBlock(gdtBlkNum)

	off := int(gdtByteOff % int64(v.sb.blockSize))
	le := binary.LittleEndian
	le.PutUint32(gdtBuf[off+0:off+4], gd.blockBitmapLo)
	le.PutUint32(gdtBuf[off+4:off+8], gd.inodeBitmapLo)
	le.PutUint32(gdtBuf[off+8:off+12], gd.inodeTableLo)
	le.PutUint16(gdtBuf[off+12:off+14], gd.freeBlocksLo)
	le.PutUint16(gdtBuf[off+14:off+16], gd.freeInodesLo)
	le.PutUint16(gdtBuf[off+16:off+18], gd.usedDirsLo)
	le.PutUint16(gdtBuf[off+18:off+20], gd.flags)
	if ds >= 64 {
		le.PutUint32(gdtBuf[off+32:off+36], gd.blockBitmapHi)
		le.PutUint32(gdtBuf[off+36:off+40], gd.inodeBitmapHi)
		le.PutUint32(gdtBuf[off+40:off+44], gd.inodeTableHi)
		le.PutUint16(gdtBuf[off+44:off+46], gd.freeBlocksHi)
		le.PutUint16(gdtBuf[off+46:off+48], gd.freeInodesHi)
		le.PutUint16(gdtBuf[off+48:off+50], gd.usedDirsHi)
	}
	v.writeBlock(gdtBlkNum, gdtBuf)
}