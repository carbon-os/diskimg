// Package fat32 formats FAT32 filesystems onto raw partition streams.
package fat32

import (
	"fmt"
	"io"
)

const (
	reservedSec = uint32(32)
	numFATs     = uint32(2)
)

// Options configures FAT32 formatting.
type Options struct {
	// Label is the volume label; up to 11 ASCII characters, uppercased and
	// space-padded. Defaults to "NO NAME" when empty.
	Label string

	// SectorSize is the logical sector size in bytes. Defaults to 512.
	// Must be a power of two in [512, 4096].
	SectorSize int
}

func (o Options) sectorSize() int {
	if o.SectorSize == 0 {
		return 512
	}
	return o.SectorSize
}

// Format writes a complete FAT32 filesystem onto rw.
// rw must be positioned at offset 0 (first byte of the partition).
// sizeBytes must be large enough to hold at least 65 536 sectors.
func Format(rw io.ReadWriteSeeker, sizeBytes int64, opts Options) error {
	ss := opts.sectorSize()
	if !isPow2(ss) || ss < 512 || ss > 4096 {
		return fmt.Errorf("fat32: unsupported sector size %d", ss)
	}

	totalSectors := uint32(sizeBytes / int64(ss))
	if totalSectors < 65536 {
		return fmt.Errorf("fat32: partition too small (%d sectors; need ≥ 65 536)", totalSectors)
	}

	spc := sectorsPerCluster(sizeBytes, ss)
	size := fatSize(totalSectors, spc)
	vbr := buildVBR(ss, totalSectors, size, spc, opts)
	fsinfo := buildFSInfo(ss)
	empty := make([]byte, ss)

	if _, err := rw.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// Reserved region: sectors 0–31.
	// 0, 6  = VBR + backup VBR
	// 1, 7  = FSInfo + backup FSInfo
	// 2–31  = empty (2–5 before backup, 8–31 after)
	for i := uint32(0); i < reservedSec; i++ {
		sec := empty
		switch i {
		case 0, 6:
			sec = vbr
		case 1, 7:
			sec = fsinfo
		}
		if _, err := rw.Write(sec); err != nil {
			return fmt.Errorf("fat32: write reserved sector %d: %w", i, err)
		}
	}

	if err := writeFATs(rw, ss, size); err != nil {
		return err
	}
	return writeRootDir(rw, ss, spc, opts.Label)
}