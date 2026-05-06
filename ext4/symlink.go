package ext4

import (
	"fmt"
	"io"
	"strings"
)

// ReadSymlink returns the symlink target for a symlink inode.
// Targets ≤60 bytes are stored directly in IBlock (fast symlinks).
// Longer targets are stored in data blocks via the normal inode reader.
func ReadSymlink(r io.ReaderAt, sb *Superblock, inode *Inode) (string, error) {
	size := inode.Size()
	if size == 0 {
		return "", nil
	}

	// Fast symlink: target fits entirely in the 60-byte IBlock field.
	// This applies when the inode has NO data blocks allocated.
	// The check: if UsesExtents is false and all block pointers are 0,
	// OR if size <= 60 and IBlock looks like text, use inline.
	//
	// Reliable heuristic: size <= 60 AND not using extents AND not using
	// indirect blocks (first block pointer is 0).
	if size <= 60 && !inode.UsesExtents() {
		raw := inode.IBlock[:size]
		return strings.TrimRight(string(raw), "\x00"), nil
	}

	// Long symlink: read from data blocks.
	ir, err := NewInodeReader(r, sb, inode)
	if err != nil {
		return "", fmt.Errorf("ext4: symlink reader: %w", err)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(ir, buf); err != nil {
		return "", fmt.Errorf("ext4: read symlink data: %w", err)
	}
	return strings.TrimRight(string(buf), "\x00"), nil
}