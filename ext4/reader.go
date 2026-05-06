package ext4

import (
	"fmt"
	"io"
)

// InodeReader implements io.Reader over an inode's data blocks.
// It transparently handles both extent-tree and indirect-block-map inodes.
// Sparse holes (block number 0) are read as zero bytes.
type InodeReader struct {
	r          io.ReaderAt
	sb         *Superblock
	blocks     []uint64 // physical block numbers, one per logical block
	size       int64    // total file size in bytes
	pos        int64    // current read position
	blockCache []byte   // cached current block
	cachedBlk  int      // logical block number currently in cache
}

// NewInodeReader creates an InodeReader for the given inode.
// It resolves all logical→physical block mappings upfront.
func NewInodeReader(r io.ReaderAt, sb *Superblock, inode *Inode) (*InodeReader, error) {
	var blocks []uint64
	var err error

	if inode.UsesExtents() {
		extents, e := CollectExtents(r, sb, inode)
		if e != nil {
			return nil, fmt.Errorf("extent tree: %w", e)
		}
		blocks = extentsToBlocks(extents, inode.Size(), sb.BlockSize)
	} else {
		blocks, err = CollectIndirectBlocks(r, sb, inode)
		if err != nil {
			return nil, fmt.Errorf("indirect blocks: %w", err)
		}
	}

	return &InodeReader{
		r:         r,
		sb:        sb,
		blocks:    blocks,
		size:      inode.Size(),
		cachedBlk: -1,
	}, nil
}

// Read implements io.Reader.
func (ir *InodeReader) Read(p []byte) (int, error) {
	if ir.pos >= ir.size {
		return 0, io.EOF
	}

	// Clamp to file size
	remaining := ir.size - ir.pos
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}

	total := 0
	for len(p) > 0 {
		lbn := int(ir.pos / ir.sb.BlockSize)
		offInBlock := int(ir.pos % ir.sb.BlockSize)

		if err := ir.ensureBlock(lbn); err != nil {
			return total, err
		}

		n := copy(p, ir.blockCache[offInBlock:])
		p = p[n:]
		total += n
		ir.pos += int64(n)
	}
	return total, nil
}

// ensureBlock loads logical block lbn into the cache if not already there.
func (ir *InodeReader) ensureBlock(lbn int) error {
	if lbn == ir.cachedBlk {
		return nil
	}
	ir.blockCache = make([]byte, ir.sb.BlockSize)
	ir.cachedBlk = lbn

	if lbn >= len(ir.blocks) || ir.blocks[lbn] == 0 {
		// Sparse hole or past end — leave zeroes
		return nil
	}

	off := int64(ir.blocks[lbn]) * ir.sb.BlockSize
	if _, err := ir.r.ReadAt(ir.blockCache, off); err != nil {
		return fmt.Errorf("read data block %d (lbn %d): %w", ir.blocks[lbn], lbn, err)
	}
	return nil
}

// extentsToBlocks converts a slice of ExtentRange into a flat per-logical-block
// array of physical block numbers, filling holes with 0.
func extentsToBlocks(extents []ExtentRange, size, blockSize int64) []uint64 {
	numBlocks := int((size + blockSize - 1) / blockSize)
	blocks := make([]uint64, numBlocks)
	for _, ex := range extents {
		for i := uint32(0); i < uint32(ex.Length); i++ {
			lbn := int(ex.LogicalStart) + int(i)
			if lbn >= numBlocks {
				break
			}
			blocks[lbn] = ex.PhysicalStart + uint64(i)
		}
	}
	return blocks
}