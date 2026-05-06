package ext4

import (
	"fmt"
	"io"
)

// DirEntry is a parsed directory entry.
type DirEntry struct {
	InodeNum uint32
	Name     string
	FileType uint8 // 0=unknown,1=reg,2=dir,3=chr,4=blk,5=fifo,6=sock,7=symlink
}

// FileType constants matching the ext4 dir_entry file_type field.
const (
	FTUnknown  = 0
	FTRegFile  = 1
	FTDir      = 2
	FTChrDev   = 3
	FTBlkDev   = 4
	FTFIFO     = 5
	FTSock     = 6
	FTSymlink  = 7
)

// ReadDir reads all directory entries for a directory inode.
// It reads every data block of the inode and parses entries linearly,
// skipping entries with inode=0 (unused/htree index nodes).
//
// This single approach handles both linear directories and htree directories:
//   - Linear: all blocks are leaf blocks with real entries.
//   - HTree: block 0 has . and .., rest of block 0 is index data covered by
//     the fake dotdot rec_len; index node blocks have inode=0 and are skipped;
//     leaf blocks parse normally.
func ReadDir(r io.ReaderAt, sb *Superblock, inode *Inode) ([]*DirEntry, error) {
	ir, err := NewInodeReader(r, sb, inode)
	if err != nil {
		return nil, fmt.Errorf("ext4: readdir: %w", err)
	}

	hasFileType := sb.FeatIncompat&IncompatFileType != 0
	var entries []*DirEntry
	blockSize := int(sb.BlockSize)

	// Process one block at a time.
	blockBuf := make([]byte, blockSize)
	for {
		n, readErr := io.ReadFull(ir, blockBuf)
		if n == 0 {
			break
		}
		// Even a partial block may have entries; pad remainder with zeroes.
		for i := n; i < blockSize; i++ {
			blockBuf[i] = 0
		}

		if err := parseDirBlock(blockBuf[:blockSize], hasFileType, &entries); err != nil {
			return nil, err
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("ext4: readdir block: %w", readErr)
		}
	}
	return entries, nil
}

// parseDirBlock parses one block worth of directory entries.
func parseDirBlock(block []byte, hasFileType bool, out *[]*DirEntry) error {
	pos := 0
	for pos < len(block) {
		if pos+8 > len(block) {
			break
		}

		inodeNum := leU32(block[pos:])
		recLen := int(leU16(block[pos+4:]))
		nameLen := int(block[pos+6])

		if recLen == 0 {
			break // safety: prevent infinite loop
		}

		if inodeNum != 0 && nameLen > 0 {
			if pos+8+nameLen > len(block) {
				break // corrupted entry, stop block
			}
			name := string(block[pos+8 : pos+8+nameLen])

			// Skip . and .. — callers should handle these if needed.
			if name != "." && name != ".." {
				ft := uint8(FTUnknown)
				if hasFileType {
					ft = block[pos+7]
				}
				*out = append(*out, &DirEntry{
					InodeNum: inodeNum,
					Name:     name,
					FileType: ft,
				})
			}
		}

		pos += recLen
	}
	return nil
}

// leU32 decodes a little-endian uint32 from b.
func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// leU16 decodes a little-endian uint16 from b.
func leU16(b []byte) uint16 {
	return uint16(b[0]) | uint16(b[1])<<8
}