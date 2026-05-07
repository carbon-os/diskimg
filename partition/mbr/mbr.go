package mbr

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/carbon-os/diskimg/partition"
)

const (
	sectorSize  = 512
	tableOffset = 0x1BE
	signature   = uint16(0xAA55)
	maxEntries  = 4
)

type entry struct {
	Status     uint8
	CHSFirst   [3]uint8
	PartType   uint8
	CHSLast    [3]uint8
	LBAStart   uint32
	LBASectors uint32
}

// Parse reads the MBR from the first 512 bytes of r and returns up to four
// partitions.  Returns (nil, nil) for unpartitioned, GPT-protected, or
// whole-disk-filesystem images (where MBR bytes are bootloader code).
// diskSize is used to reject partition entries whose start lies beyond the disk.
func Parse(r io.ReaderAt, diskSize int64) ([]*partition.Partition, error) {
	buf := make([]byte, sectorSize)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("mbr: read sector 0: %w", err)
	}

	sig := binary.LittleEndian.Uint16(buf[510:512])
	if sig != signature {
		return nil, nil // not MBR-partitioned
	}

	var parts []*partition.Partition
	for i := 0; i < maxEntries; i++ {
		off := tableOffset + i*16
		var e entry
		e.Status = buf[off]
		e.PartType = buf[off+4]
		e.LBAStart = binary.LittleEndian.Uint32(buf[off+8 : off+12])
		e.LBASectors = binary.LittleEndian.Uint32(buf[off+12 : off+16])

		if e.PartType == 0x00 || e.LBASectors == 0 {
			continue
		}
		if e.PartType == 0xEE {
			// Protective MBR → disk uses GPT.
			return nil, nil
		}

		startByte := int64(e.LBAStart) * sectorSize
		sizeBytes := int64(e.LBASectors) * sectorSize

		// Reject entries whose start lies outside the disk — these are
		// bootloader bytes (e.g. extlinux) masquerading as partition data.
		if diskSize > 0 && startByte >= diskSize {
			continue
		}

		parts = append(parts, &partition.Partition{
			Index:     i + 1,
			StartByte: startByte,
			SizeBytes: sizeBytes,
		})
	}
	return parts, nil
}