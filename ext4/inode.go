package ext4

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Inode file type masks (upper nibble of i_mode).
const (
	ModeTypeMask = 0xF000
	ModeIFIFO    = 0x1000
	ModeIFCHR    = 0x2000
	ModeIFDIR    = 0x4000
	ModeIFBLK    = 0x6000
	ModeIFREG    = 0x8000
	ModeIFLNK    = 0xA000
	ModeIFSOCK   = 0xC000
)

// Inode holds parsed inode metadata and the raw i_block field.
//
// Key inode layout (256-byte modern form):
//   0x00  i_mode       uint16   file type + permissions
//   0x02  i_uid_lo     uint16
//   0x04  i_size_lo    uint32
//   0x08  i_atime      uint32
//   0x0C  i_ctime      uint32
//   0x10  i_mtime      uint32
//   0x14  i_dtime      uint32
//   0x18  i_gid_lo     uint16
//   0x1C  i_blocks_lo  uint32
//   0x20  i_flags      uint32
//   0x28  i_block[15]  60 bytes (extent tree root or indirect blocks or symlink)
//   0x6C  i_size_high  uint32
//   0x78  i_uid_hi     uint16  (in i_osd2)
//   0x7A  i_gid_hi     uint16  (in i_osd2)
type Inode struct {
	Mode    uint16
	UIDLo   uint16
	SizeLo  uint32
	ATime   uint32
	CTime   uint32
	MTime   uint32
	DTime   uint32
	GIDLo   uint16
	Flags   uint32
	IBlock  [60]byte // raw: extent tree header, indirect ptrs, or symlink bytes
	SizeHi  uint32
	UIDHi   uint16
	GIDHi   uint16
}

// IsDir reports whether the inode is a directory.
func (in *Inode) IsDir() bool { return in.Mode&ModeTypeMask == ModeIFDIR }

// IsRegular reports whether the inode is a regular file.
func (in *Inode) IsRegular() bool { return in.Mode&ModeTypeMask == ModeIFREG }

// IsSymlink reports whether the inode is a symbolic link.
func (in *Inode) IsSymlink() bool { return in.Mode&ModeTypeMask == ModeIFLNK }

// UsesExtents reports whether the inode uses an extent tree (vs indirect blocks).
func (in *Inode) UsesExtents() bool { return in.Flags&InodeFlagExtents != 0 }

// UsesHTree reports whether the directory inode uses an htree index.
func (in *Inode) UsesHTree() bool { return in.Flags&InodeFlagIndex != 0 }

// Size returns the full 64-bit file size.
func (in *Inode) Size() int64 {
	return int64(in.SizeHi)<<32 | int64(in.SizeLo)
}

// UID returns the full 32-bit user ID.
func (in *Inode) UID() uint32 { return uint32(in.UIDHi)<<16 | uint32(in.UIDLo) }

// GID returns the full 32-bit group ID.
func (in *Inode) GID() uint32 { return uint32(in.GIDHi)<<16 | uint32(in.GIDLo) }

// ModTime returns the last modification time.
func (in *Inode) ModTime() time.Time { return time.Unix(int64(in.MTime), 0) }

// Perm returns the file permission bits.
func (in *Inode) Perm() uint16 { return in.Mode & 0x0FFF }

// ReadInode reads and parses inode number inodeNum (1-based) from r.
//
// Inode location:
//   group = (inodeNum - 1) / sb.InodesPerGroup
//   index = (inodeNum - 1) % sb.InodesPerGroup
//   inode_table_block = bgd[group].InodeTableBlock()
//   byte_offset = inode_table_block * block_size + index * inode_size
func ReadInode(r io.ReaderAt, sb *Superblock, inodeNum uint32) (*Inode, error) {
	if inodeNum == 0 {
		return nil, fmt.Errorf("ext4: inode 0 is invalid")
	}
	idx := inodeNum - 1
	group := idx / sb.InodesPerGroup
	indexInGroup := idx % sb.InodesPerGroup

	bgd, err := ReadBlockGroupDesc(r, sb, group)
	if err != nil {
		return nil, fmt.Errorf("ext4: inode %d: %w", inodeNum, err)
	}

	tableBlock := bgd.InodeTableBlock()
	byteOffset := int64(tableBlock)*sb.BlockSize + int64(indexInGroup)*int64(sb.InodeSize)

	buf := make([]byte, sb.InodeSize)
	if _, err := r.ReadAt(buf, byteOffset); err != nil {
		return nil, fmt.Errorf("ext4: read inode %d at 0x%X: %w", inodeNum, byteOffset, err)
	}

	le := binary.LittleEndian
	in := &Inode{
		Mode:   le.Uint16(buf[0x00:]),
		UIDLo:  le.Uint16(buf[0x02:]),
		SizeLo: le.Uint32(buf[0x04:]),
		ATime:  le.Uint32(buf[0x08:]),
		CTime:  le.Uint32(buf[0x0C:]),
		MTime:  le.Uint32(buf[0x10:]),
		DTime:  le.Uint32(buf[0x14:]),
		GIDLo:  le.Uint16(buf[0x18:]),
		Flags:  le.Uint32(buf[0x20:]),
		SizeHi: le.Uint32(buf[0x6C:]),
	}
	copy(in.IBlock[:], buf[0x28:0x64])

	// i_osd2 contains uid_hi and gid_hi for Linux (creator_os == 0)
	if len(buf) >= 0x7C {
		in.UIDHi = le.Uint16(buf[0x78:])
		in.GIDHi = le.Uint16(buf[0x7A:])
	}

	return in, nil
}