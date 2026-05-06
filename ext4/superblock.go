// Package ext4 implements a pure-Go read-only ext4 filesystem driver.
// All multi-byte fields are little-endian per the ext4 specification.
package ext4

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Superblock offset within a partition is always 1024 bytes.
const SuperblockOffset = 1024

// ext4 magic number.
const Magic = 0xEF53

// Incompatible feature flags (must be understood to read the fs).
const (
	IncompatFileType = uint32(0x0002) // dir entries have file_type byte
	IncompatExtents  = uint32(0x0040) // extent trees used
	Incompat64Bit    = uint32(0x0080) // 64-bit block descriptor fields
	IncompatFlexBG   = uint32(0x0200) // flexible block groups
	IncompatInline   = uint32(0x8000) // inline data in inodes
)

// Read-only compatible feature flags.
const (
	ROCompatDirIndex = uint32(0x0010) // htree directories
	ROCompatHugeFile = uint32(0x0008) // large files
	ROCompatGDTCsum  = uint32(0x0010) // GDT checksum / uninit_bg
)

// Inode flag bits.
const (
	InodeFlagExtents = uint32(0x00080000) // inode uses extent tree
	InodeFlagIndex   = uint32(0x00001000) // directory uses htree
	InodeFlagInline  = uint32(0x10000000) // inline data
)

// Superblock holds the parsed ext4 superblock.
// All uint32/uint16 values are decoded from little-endian bytes.
type Superblock struct {
	InodesCount    uint32 // 0x00 total inodes
	BlocksCountLo  uint32 // 0x04 total blocks (lo 32 bits)
	FreeBlocksLo   uint32 // 0x0C free blocks
	FreeInodes     uint32 // 0x10 free inodes
	FirstDataBlock uint32 // 0x14 first data block (0 for 4K, 1 for 1K)
	LogBlockSize   uint32 // 0x18 block_size = 1024 << LogBlockSize
	BlocksPerGroup uint32 // 0x20 blocks per block group
	InodesPerGroup uint32 // 0x28 inodes per block group
	Magic          uint16 // 0x38 must be 0xEF53
	RevLevel       uint32 // 0x4C 0=old, 1=dynamic
	FirstIno       uint32 // 0x54 first non-reserved inode (usually 11)
	InodeSize      uint16 // 0x58 inode record size (128 or 256)
	FeatCompat     uint32 // 0x5C compatible features
	FeatIncompat   uint32 // 0x60 incompatible features
	FeatROCompat   uint32 // 0x64 read-only compatible features
	UUID           [16]byte // 0x68 filesystem UUID
	VolumeName     [16]byte // 0x78 volume name

	// Derived
	BlockSize int64  // 1024 << LogBlockSize
	GDTBlock  uint64 // block number of the group descriptor table
	GDTSize   int    // bytes per block group descriptor (32 or 64)
}

// ReadSuperblock reads and parses the superblock from r at SuperblockOffset.
func ReadSuperblock(r io.ReaderAt) (*Superblock, error) {
	buf := make([]byte, 1024)
	if _, err := r.ReadAt(buf, SuperblockOffset); err != nil {
		return nil, fmt.Errorf("ext4: read superblock: %w", err)
	}

	sb := &Superblock{}
	le := binary.LittleEndian

	sb.InodesCount    = le.Uint32(buf[0x00:])
	sb.BlocksCountLo  = le.Uint32(buf[0x04:])
	sb.FreeBlocksLo   = le.Uint32(buf[0x0C:])
	sb.FreeInodes     = le.Uint32(buf[0x10:])
	sb.FirstDataBlock = le.Uint32(buf[0x14:])
	sb.LogBlockSize   = le.Uint32(buf[0x18:])
	sb.BlocksPerGroup = le.Uint32(buf[0x20:])
	sb.InodesPerGroup = le.Uint32(buf[0x28:])
	sb.Magic          = le.Uint16(buf[0x38:])
	sb.RevLevel       = le.Uint32(buf[0x4C:])
	sb.FirstIno       = le.Uint32(buf[0x54:])
	sb.InodeSize      = le.Uint16(buf[0x58:])
	sb.FeatCompat     = le.Uint32(buf[0x5C:])
	sb.FeatIncompat   = le.Uint32(buf[0x60:])
	sb.FeatROCompat   = le.Uint32(buf[0x64:])
	copy(sb.UUID[:], buf[0x68:0x78])
	copy(sb.VolumeName[:], buf[0x78:0x88])

	if sb.Magic != Magic {
		return nil, fmt.Errorf("ext4: bad magic: 0x%04X (want 0x%04X)", sb.Magic, Magic)
	}

	// Old rev: fixed inode size of 128
	if sb.RevLevel == 0 {
		sb.InodeSize = 128
		sb.FirstIno  = 11
	}

	sb.BlockSize = int64(1024) << sb.LogBlockSize

	// GDT block = block immediately after the superblock's block.
	// sb is in block: 1024 / block_size (integer division)
	sbBlock := uint64(1024 / sb.BlockSize)
	sb.GDTBlock = sbBlock + 1

	// GDT entry size: 64 bytes if 64BIT feature, 32 bytes otherwise.
	if sb.FeatIncompat&Incompat64Bit != 0 {
		sb.GDTSize = 64
	} else {
		sb.GDTSize = 32
	}

	return sb, nil
}

// GroupCount returns the number of block groups in this filesystem.
func (sb *Superblock) GroupCount() uint32 {
	total := uint64(sb.BlocksCountLo)
	perGrp := uint64(sb.BlocksPerGroup)
	return uint32((total + perGrp - 1) / perGrp)
}

// HasIncompat reports whether all given incompatible feature flags are set.
func (sb *Superblock) HasIncompat(flags uint32) bool {
	return sb.FeatIncompat&flags == flags
}

// HasROCompat reports whether all given read-only compat feature flags are set.
func (sb *Superblock) HasROCompat(flags uint32) bool {
	return sb.FeatROCompat&flags == flags
}