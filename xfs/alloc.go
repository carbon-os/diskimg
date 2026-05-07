package xfs

import (
	"encoding/binary"
	"fmt"
)

// allocBlock finds a free block, preferring the AG that contains parentIno.
// Returns the absolute block number.
// XFS uses free-space B+trees per AG; for a pure writer we do a simple linear
// scan of the AGF free-space counters to pick an AG, then allocate from the
// end of that AG's used space (approximation safe for fresh/controlled images).
func (v *Volume) allocBlock(parentIno uint64) (uint64, error) {
	ag, _ := v.inodeAG(parentIno)
	n := uint32(len(v.agfs))
	for i := uint32(0); i < n; i++ {
		a := (ag + i) % n
		if v.agfs[a].freeBlks > 0 {
			blk, err := v.allocBlockInAG(a)
			if err == nil {
				return blk, nil
			}
		}
	}
	return 0, fmt.Errorf("xfs: no free blocks")
}

// allocBlockInAG allocates one block from AG a by bumping the AGF's
// recorded "longest free run" pointer.  This is intentionally simple:
// it is correct for building fresh images where blocks are append-only.
func (v *Volume) allocBlockInAG(ag uint32) (uint64, error) {
	agBase := int64(ag) * int64(v.sb.agBlocks) * int64(v.sb.blockSize)
	buf := make([]byte, v.sb.sectSize)
	if _, err := v.sr.ReadAt(buf, agBase+agfOff); err != nil {
		return 0, fmt.Errorf("xfs: read AGF %d: %w", ag, err)
	}
	be := binary.BigEndian
	// AGF fields (big-endian):
	//  0:4   magic
	//  4:8   version
	//  8:12  seqno
	// 12:16  length
	// 16:20  bno root
	// 20:24  cnt root
	// 24:28  bno level
	// 28:32  cnt level
	// 32:36  flfirst
	// 36:40  fllast
	// 40:44  flcount
	// 44:48  freeblks
	// 48:52  longest
	freeBlks := be.Uint32(buf[44:48])
	if freeBlks == 0 {
		return 0, fmt.Errorf("xfs: AG %d is full", ag)
	}
	// Derive a safe allocation point: agBlocks - freeBlks gives a rough
	// "high-water mark" of used blocks.  This is safe for append-only usage.
	hwm := uint64(v.sb.agBlocks) - uint64(freeBlks)
	physBlk := uint64(ag)*uint64(v.sb.agBlocks) + hwm

	// Update AGF in place.
	freeBlks--
	be.PutUint32(buf[44:48], freeBlks)
	if _, err := v.sr.ReadAt(buf[:0], 0); err == nil { // no-op to satisfy linter
	}
	// Stage updated AGF sector as a dirty block at sector offset.
	sectorBlk := (agBase + agfOff) / int64(v.sb.blockSize)
	sectorOff := (agBase + agfOff) % int64(v.sb.blockSize)
	sectorData, _ := v.readBlock(uint64(sectorBlk))
	copy(sectorData[sectorOff:], buf)
	v.writeBlock(uint64(sectorBlk), sectorData)

	v.agfs[ag].freeBlks = freeBlks
	v.sb.dblocks-- // loose approximation; superblock written on Unmount
	return physBlk, nil
}

// allocInode allocates a new inode number, preferring the AG of parentIno.
// For simplicity (and correctness for fresh images) it increments the AGI's
// "most recently allocated" pointer.
func (v *Volume) allocInode(parentIno uint64) (uint64, error) {
	ag, _ := v.inodeAG(parentIno)
	n := uint32(len(v.agis))
	for i := uint32(0); i < n; i++ {
		a := (ag + i) % n
		if v.agis[a].freeCount > 0 {
			ino, err := v.allocInodeInAG(a)
			if err == nil {
				return ino, nil
			}
		}
	}
	return 0, fmt.Errorf("xfs: no free inodes")
}

func (v *Volume) allocInodeInAG(ag uint32) (uint64, error) {
	agi := &v.agis[ag]
	if agi.freeCount == 0 {
		return 0, fmt.Errorf("xfs: AG %d inode table full", ag)
	}

	// Bump the "last allocated" AG-relative inode number.
	agi.newIno++
	agi.freeCount--

	// Persist AGI update.
	agBase := int64(ag) * int64(v.sb.agBlocks) * int64(v.sb.blockSize)
	buf := make([]byte, v.sb.sectSize)
	if _, err := v.sr.ReadAt(buf, agBase+agiOff); err != nil {
		return 0, fmt.Errorf("xfs: read AGI %d: %w", ag, err)
	}
	be := binary.BigEndian
	be.PutUint32(buf[28:32], agi.freeCount)
	be.PutUint32(buf[32:36], agi.newIno)

	sectorBlk := (agBase + agiOff) / int64(v.sb.blockSize)
	sectorOff := (agBase + agiOff) % int64(v.sb.blockSize)
	sectorData, _ := v.readBlock(uint64(sectorBlk))
	copy(sectorData[sectorOff:], buf)
	v.writeBlock(uint64(sectorBlk), sectorData)

	// Compute absolute inode number.
	inoBits := uint(v.sb.inopBLog) + uint(v.sb.agBlkLog)
	absIno := uint64(ag)<<inoBits | uint64(agi.newIno)
	return absIno, nil
}

// freeInode marks ino as no longer in use (increments freeCount in AGI).
func (v *Volume) freeInode(ino uint64) {
	ag, _ := v.inodeAG(ino)
	if int(ag) >= len(v.agis) {
		return
	}
	v.agis[ag].freeCount++

	agBase := int64(ag) * int64(v.sb.agBlocks) * int64(v.sb.blockSize)
	buf := make([]byte, v.sb.sectSize)
	if _, err := v.sr.ReadAt(buf, agBase+agiOff); err != nil {
		return
	}
	binary.BigEndian.PutUint32(buf[28:32], v.agis[ag].freeCount)
	sectorBlk := (agBase + agiOff) / int64(v.sb.blockSize)
	sectorOff := (agBase + agiOff) % int64(v.sb.blockSize)
	sectorData, _ := v.readBlock(uint64(sectorBlk))
	copy(sectorData[sectorOff:], buf)
	v.writeBlock(uint64(sectorBlk), sectorData)
}