package mkfs

import "fmt"

// bitmap tracks allocated blocks and inodes for a single block group.
type bitmap struct {
	blockBmap   []byte
	inodeBmap   []byte
	nextBlock   uint32
	nextInode   uint32
	totalBlocks uint32
	usedBlkCnt  uint32
	usedInoCnt  uint32
}

func newBitmap(l *Layout) *bitmap {
	bm := &bitmap{
		blockBmap:   make([]byte, BlockSize),
		inodeBmap:   make([]byte, BlockSize),
		nextBlock:   l.FirstDataBlock,
		nextInode:   11, // inodes 1–10 are reserved
		totalBlocks: l.TotalBlocks,
	}
	// Mark system blocks 0..FirstDataBlock-1 as used.
	for i := uint32(0); i < l.FirstDataBlock; i++ {
		bm.blockBmap[i/8] |= 1 << (i % 8)
	}
	bm.usedBlkCnt = l.FirstDataBlock

	// Mark reserved inodes 1–10 as used (0-indexed in bitmap).
	for i := uint32(0); i < 10; i++ {
		bm.inodeBmap[i/8] |= 1 << (i % 8)
	}
	bm.usedInoCnt = 10
	return bm
}

// allocBlock reserves the next free data block and returns its number.
func (bm *bitmap) allocBlock() uint32 {
	n := bm.nextBlock
	if n >= bm.totalBlocks {
		panic(fmt.Sprintf("mkfs: out of disk space — no free blocks (at block %d)", n))
	}
	bm.nextBlock++
	bm.blockBmap[n/8] |= 1 << (n % 8)
	bm.usedBlkCnt++
	return n
}

// allocInode reserves the next free inode number (1-based) and returns it.
func (bm *bitmap) allocInode() uint32 {
	n := bm.nextInode
	bm.nextInode++
	idx := n - 1 // convert to 0-based bitmap index
	bm.inodeBmap[idx/8] |= 1 << (idx % 8)
	bm.usedInoCnt++
	return n
}

func (bm *bitmap) usedBlocks() uint32 { return bm.usedBlkCnt }
func (bm *bitmap) usedInodes() uint32 { return bm.usedInoCnt }