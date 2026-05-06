package ext4

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Extent tree structures per the Linux kernel specification.
//
// Every node in the extent tree starts with an ext4_extent_header (12 bytes):
//   [0–1]  eh_magic    uint16   must be 0xF30A
//   [2–3]  eh_entries  uint16   valid entries after header
//   [4–5]  eh_max      uint16   capacity of this node
//   [6–7]  eh_depth    uint16   0 = leaf, >0 = index node
//   [8–11] eh_generation uint32
//
// Leaf node entry — ext4_extent (12 bytes):
//   [0–3]  ee_block    uint32   first logical block covered
//   [4–5]  ee_len      uint16   block count (>32768 = uninitialized)
//   [6–7]  ee_start_hi uint16   physical block (upper 16 bits)
//   [8–11] ee_start_lo uint32   physical block (lower 32 bits)
//
// Index node entry — ext4_extent_idx (12 bytes):
//   [0–3]  ei_block    uint32   first logical block covered by this subtree
//   [4–7]  ei_leaf_lo  uint32   physical block of child node (lower 32 bits)
//   [8–9]  ei_leaf_hi  uint16   physical block of child node (upper 16 bits)
//   [10–11] unused

const (
	extentMagic    = uint16(0xF30A)
	extentHdrSize  = 12
	extentLeafSize = 12
	extentIdxSize  = 12
)

// ExtentRange maps a run of logical blocks to a physical block start.
type ExtentRange struct {
	LogicalStart  uint32 // first logical block
	Length        uint16 // number of blocks
	PhysicalStart uint64 // first physical block
}

// physBlock computes the physical block for a leaf extent entry.
func physBlock(hi uint16, lo uint32) uint64 {
	return uint64(hi)<<32 | uint64(lo)
}

// CollectExtents traverses the extent tree rooted at inode.IBlock and
// returns all leaf ExtentRanges in logical order.
func CollectExtents(r io.ReaderAt, sb *Superblock, inode *Inode) ([]ExtentRange, error) {
	// The extent tree root is in the first 12 bytes of IBlock.
	return parseExtentNode(r, sb, inode.IBlock[:])
}

// parseExtentNode recursively parses a 60-byte (inode) or block-sized extent node.
func parseExtentNode(r io.ReaderAt, sb *Superblock, data []byte) ([]ExtentRange, error) {
	if len(data) < extentHdrSize {
		return nil, fmt.Errorf("ext4: extent node too short (%d bytes)", len(data))
	}

	magic := binary.LittleEndian.Uint16(data[0:2])
	if magic != extentMagic {
		return nil, fmt.Errorf("ext4: extent bad magic 0x%04X", magic)
	}

	entries := int(binary.LittleEndian.Uint16(data[2:4]))
	depth := binary.LittleEndian.Uint16(data[6:8])

	if depth == 0 {
		// Leaf node: entries are ext4_extent structs.
		return parseLeafEntries(data[extentHdrSize:], entries)
	}

	// Index node: entries are ext4_extent_idx pointing to child blocks.
	var result []ExtentRange
	for i := 0; i < entries; i++ {
		off := extentHdrSize + i*extentIdxSize
		if off+extentIdxSize > len(data) {
			break
		}
		entry := data[off : off+extentIdxSize]
		leafLo := binary.LittleEndian.Uint32(entry[4:8])
		leafHi := binary.LittleEndian.Uint16(entry[8:10])
		childBlock := physBlock(leafHi, leafLo)

		childData, err := readRawBlock(r, sb, childBlock)
		if err != nil {
			return nil, fmt.Errorf("ext4: extent index child block %d: %w", childBlock, err)
		}
		childRanges, err := parseExtentNode(r, sb, childData)
		if err != nil {
			return nil, fmt.Errorf("ext4: extent child node: %w", err)
		}
		result = append(result, childRanges...)
	}
	return result, nil
}

// parseLeafEntries decodes leaf extent entries from raw bytes.
func parseLeafEntries(data []byte, count int) ([]ExtentRange, error) {
	ranges := make([]ExtentRange, 0, count)
	for i := 0; i < count; i++ {
		off := i * extentLeafSize
		if off+extentLeafSize > len(data) {
			break
		}
		e := data[off : off+extentLeafSize]
		length := binary.LittleEndian.Uint16(e[4:6])
		// If bit 15 set, extent is uninitialized (pre-allocated but unwritten).
		// Treat as regular for read purposes.
		if length > 0x8000 {
			length -= 0x8000
		}
		ranges = append(ranges, ExtentRange{
			LogicalStart:  binary.LittleEndian.Uint32(e[0:4]),
			Length:        length,
			PhysicalStart: physBlock(binary.LittleEndian.Uint16(e[6:8]), binary.LittleEndian.Uint32(e[8:12])),
		})
	}
	return ranges, nil
}

// readRawBlock reads one full block from the partition reader.
func readRawBlock(r io.ReaderAt, sb *Superblock, blockNum uint64) ([]byte, error) {
	buf := make([]byte, sb.BlockSize)
	off := int64(blockNum) * sb.BlockSize
	if _, err := r.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("read block %d: %w", blockNum, err)
	}
	return buf, nil
}