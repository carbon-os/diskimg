package fat

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FS holds parsed FAT32 BPB fields for basic traversal.
//
// BPB (BIOS Parameter Block) layout at partition start:
//   [0–2]   jump instruction
//   [3–10]  OEM name
//   [11–12] bytes per sector (uint16 LE)
//   [13]    sectors per cluster (uint8)
//   [14–15] reserved sectors (uint16 LE)    ← FAT tables start here
//   [16]    number of FATs (uint8)
//   [28–31] hidden sectors (uint32 LE)
//   [32–35] total sectors 32 (uint32 LE)
//   [36–39] sectors per FAT (uint32 LE)     ← FAT32 only
//   [44–47] root cluster (uint32 LE)        ← FAT32 root dir first cluster
//   [82–89] "FAT32   " signature
type FS struct {
	r              io.ReaderAt
	bytesPerSector uint16
	sectPerCluster uint8
	reservedSects  uint16
	numFATs        uint8
	sectPerFAT     uint32
	rootCluster    uint32
	clusterSize    int64
	fatOffset      int64 // byte offset of FAT table 1
	dataOffset     int64 // byte offset of cluster 2
}

// Open parses the FAT32 BPB from a partition-relative SectionReader.
func Open(r io.ReaderAt) (*FS, error) {
	var buf [512]byte
	if _, err := r.ReadAt(buf[:], 0); err != nil {
		return nil, fmt.Errorf("fat32: read BPB: %w", err)
	}

	le := binary.LittleEndian
	fs := &FS{
		r:              r,
		bytesPerSector: le.Uint16(buf[11:13]),
		sectPerCluster: buf[13],
		reservedSects:  le.Uint16(buf[14:16]),
		numFATs:        buf[16],
		sectPerFAT:     le.Uint32(buf[36:40]),
		rootCluster:    le.Uint32(buf[44:48]),
	}

	if fs.bytesPerSector == 0 || fs.sectPerCluster == 0 {
		return nil, fmt.Errorf("fat32: invalid BPB")
	}

	fs.clusterSize = int64(fs.bytesPerSector) * int64(fs.sectPerCluster)
	fs.fatOffset = int64(fs.reservedSects) * int64(fs.bytesPerSector)
	fs.dataOffset = fs.fatOffset + int64(fs.numFATs)*int64(fs.sectPerFAT)*int64(fs.bytesPerSector)

	return fs, nil
}

// clusterOffset returns the byte offset of a cluster (cluster numbers start at 2).
func (fs *FS) clusterOffset(cluster uint32) int64 {
	return fs.dataOffset + int64(cluster-2)*fs.clusterSize
}

// nextCluster reads the FAT entry for the given cluster.
// Returns 0x0FFFFFFF or higher for end-of-chain.
func (fs *FS) nextCluster(cluster uint32) (uint32, error) {
	off := fs.fatOffset + int64(cluster)*4
	var buf [4]byte
	if _, err := fs.r.ReadAt(buf[:], off); err != nil {
		return 0, fmt.Errorf("fat32: read FAT entry %d: %w", cluster, err)
	}
	return binary.LittleEndian.Uint32(buf[:]) & 0x0FFFFFFF, nil
}

// readClusterChain collects all cluster numbers in the chain starting at first.
func (fs *FS) readClusterChain(first uint32) ([]uint32, error) {
	var chain []uint32
	cur := first
	for cur >= 2 && cur < 0x0FFFFFF8 {
		chain = append(chain, cur)
		next, err := fs.nextCluster(cur)
		if err != nil {
			return nil, err
		}
		if next == cur {
			break // safety: avoid infinite loop
		}
		cur = next
	}
	return chain, nil
}