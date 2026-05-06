package mkfs

import "fmt"

const (
	BlockSize      = 4096
	InodeSize      = 128
	inodesPerBlock = BlockSize / InodeSize // 32
	maxGroupBlocks = BlockSize * 8         // 32768 blocks = 128 MiB max per group
)

// Layout is the computed on-disk geometry of a single-block-group ext4 image.
type Layout struct {
	PartitionSize    int64
	TotalBlocks      uint32
	InodeCount       uint32
	BlockBitmapBlock uint32 // always 2
	InodeBitmapBlock uint32 // always 3
	InodeTableBlock  uint32 // always 4
	InodeTableLen    uint32 // in blocks
	FirstDataBlock   uint32 // 4 + InodeTableLen
}

// Calculate derives the Layout for the given partition size and node count.
// nodeCount is the total number of filesystem nodes (files + dirs + symlinks).
func Calculate(partSize int64, nodeCount int) (Layout, error) {
	totalBlocks := uint32(partSize / BlockSize)
	if totalBlocks > maxGroupBlocks {
		return Layout{}, fmt.Errorf(
			"mkfs: partition too large (%d blocks); single block group supports at most %d (%.0f MiB)",
			totalBlocks, maxGroupBlocks, float64(maxGroupBlocks*BlockSize)/(1<<20),
		)
	}

	// Reserve 10 system inodes + all actual nodes + ~12% slack, rounded up to a block boundary.
	inodeCount := uint32(10+nodeCount) + uint32(nodeCount)/8 + 1
	if rem := inodeCount % inodesPerBlock; rem != 0 {
		inodeCount += inodesPerBlock - rem
	}

	inodeTableLen := inodeCount / inodesPerBlock
	firstDataBlock := uint32(4) + inodeTableLen

	if firstDataBlock >= totalBlocks {
		return Layout{}, fmt.Errorf(
			"mkfs: partition too small: %d blocks available, need at least %d (inode table alone needs %d blocks)",
			totalBlocks, firstDataBlock+1, inodeTableLen,
		)
	}

	return Layout{
		PartitionSize:    partSize,
		TotalBlocks:      totalBlocks,
		InodeCount:       inodeCount,
		BlockBitmapBlock: 2,
		InodeBitmapBlock: 3,
		InodeTableBlock:  4,
		InodeTableLen:    inodeTableLen,
		FirstDataBlock:   firstDataBlock,
	}, nil
}