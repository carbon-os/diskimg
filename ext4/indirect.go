package ext4

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Indirect block map — the old ext2/ext3 scheme still valid in ext4.
// Used when InodeFlagExtents (0x80000) is NOT set in inode.Flags.
//
// IBlock layout (15 × uint32 = 60 bytes):
//   [0..11]  12 direct block numbers
//   [12]     singly-indirect block
//   [13]     doubly-indirect block
//   [14]     triply-indirect block
//
// A zero block number means a sparse hole (read as zeroes).
//
// For 4K blocks: blocksPerIndirect = 4096/4 = 1024 pointers per block.

// CollectIndirectBlocks reads the indirect block map from inode.IBlock and
// returns the physical block number for each logical block 0..count-1.
// count is derived from the file size.
func CollectIndirectBlocks(r io.ReaderAt, sb *Superblock, inode *Inode) ([]uint64, error) {
	size := inode.Size()
	if size == 0 {
		return nil, nil
	}

	// Number of logical blocks needed.
	numBlocks := int((size + sb.BlockSize - 1) / sb.BlockSize)
	ptrs := readU32s(inode.IBlock[:])          // 15 uint32s
	bpi := int(sb.BlockSize / 4)               // block pointers per indirect block

	blocks := make([]uint64, 0, numBlocks)

	for lbn := 0; lbn < numBlocks; lbn++ {
		phys, err := resolveBlock(r, sb, ptrs, bpi, lbn)
		if err != nil {
			return nil, fmt.Errorf("ext4: indirect lbn=%d: %w", lbn, err)
		}
		blocks = append(blocks, phys)
	}
	return blocks, nil
}

// resolveBlock resolves logical block number lbn through the indirect map.
func resolveBlock(r io.ReaderAt, sb *Superblock, ptrs []uint32, bpi, lbn int) (uint64, error) {
	// Direct blocks [0..11]
	if lbn < 12 {
		return uint64(ptrs[lbn]), nil
	}
	lbn -= 12

	// Singly indirect [12]
	if lbn < bpi {
		return readIndirectPtr(r, sb, ptrs[12], lbn)
	}
	lbn -= bpi

	// Doubly indirect [13]
	bpi2 := bpi * bpi
	if lbn < bpi2 {
		midPhys, err := readIndirectPtr(r, sb, ptrs[13], lbn/bpi)
		if err != nil {
			return 0, err
		}
		return readIndirectPtr(r, sb, uint32(midPhys), lbn%bpi)
	}
	lbn -= bpi2

	// Triply indirect [14]
	midPhys1, err := readIndirectPtr(r, sb, ptrs[14], lbn/(bpi*bpi))
	if err != nil {
		return 0, err
	}
	midPhys2, err := readIndirectPtr(r, sb, uint32(midPhys1), (lbn/bpi)%bpi)
	if err != nil {
		return 0, err
	}
	return readIndirectPtr(r, sb, uint32(midPhys2), lbn%bpi)
}

// readIndirectPtr reads the ptrIndex-th uint32 from the block at blockNum.
// Returns 0 (sparse hole) if blockNum is 0.
func readIndirectPtr(r io.ReaderAt, sb *Superblock, blockNum uint32, ptrIndex int) (uint64, error) {
	if blockNum == 0 {
		return 0, nil // sparse hole
	}
	var buf [4]byte
	off := int64(blockNum)*sb.BlockSize + int64(ptrIndex)*4
	if _, err := r.ReadAt(buf[:], off); err != nil {
		return 0, fmt.Errorf("read indirect ptr at block %d idx %d: %w", blockNum, ptrIndex, err)
	}
	return uint64(binary.LittleEndian.Uint32(buf[:])), nil
}

// readU32s decodes a byte slice as a sequence of little-endian uint32 values.
func readU32s(b []byte) []uint32 {
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out
}