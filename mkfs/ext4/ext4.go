// Package ext4 formats ext4 filesystems onto raw partition streams.
//
// Feature set written (matches mkfs.ext4 defaults):
//   - 4 KiB blocks, 256-byte inodes
//   - sparse_super  (SB backups in groups 0,1 and powers of 3,5,7)
//   - extent-tree mapped inodes (INCOMPAT_EXTENTS)
//   - 64-bit block group descriptors (INCOMPAT_64BIT), desc_size=64
//   - flex_bg with flex-group size 16 (INCOMPAT_FLEX_BG)
//   - dir_filetype (INCOMPAT_FILETYPE)
//   - has_journal  (COMPAT_HAS_JOURNAL) — empty JBD2 journal in inode 8
//   - ext_attr, resize_inode (COMPAT)
//   - sparse_super, large_file, huge_file, dir_nlink, extra_isize (RO_COMPAT)
//   - metadata_csum (RO_COMPAT) — crc32c on SB, GDT, bitmaps, inodes
//
// The formatter makes a single sequential write pass with no intermediate
// allocations proportional to volume size.
package ext4

import (
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// Options configures ext4 formatting.
type Options struct {
	// Label is the volume label; up to 16 bytes. Truncated silently if longer.
	Label string

	// UUID is the filesystem UUID.  A random v4 UUID is generated when zero.
	UUID [16]byte

	// InodeRatio: one inode is created per this many bytes.
	// Defaults to 16384 (mkfs.ext4 default).
	InodeRatio int64

	// ReservedPct is the percentage of blocks reserved for root.
	// Defaults to 5.
	ReservedPct int

	// internal, set by computeLayout
	reservedGDT uint32
}

// Format writes a complete ext4 filesystem onto rw.
// rw must be positioned at offset 0 (first byte of the partition).
// sizeBytes must be ≥ 16 MiB.
func Format(rw io.ReadWriteSeeker, sizeBytes int64, opts Options) error {
	if sizeBytes < 16<<20 {
		return fmt.Errorf("ext4: partition too small (%d bytes; need ≥ 16 MiB)", sizeBytes)
	}
	if opts.InodeRatio == 0 {
		opts.InodeRatio = 16384
	}
	if opts.ReservedPct == 0 {
		opts.ReservedPct = 5
	}
	zeroUUID := [16]byte{}
	if opts.UUID == zeroUUID {
		u, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("ext4: uuid: %w", err)
		}
		copy(opts.UUID[:], u[:])
	}

	fs, err := computeLayout(sizeBytes, &opts)
	if err != nil {
		return fmt.Errorf("ext4: layout: %w", err)
	}

	if _, err := rw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return fs.write(rw)
}

func timeNow() int64 { return time.Now().Unix() }