// Package mbr parses MBR (Master Boot Record) partition tables.
//
// MBR layout (512 bytes at disk offset 0):
//   [0   – 445]  boot code + disk signature — copy verbatim
//   [446 – 461]  partition entry 1 (16 bytes)
//   [462 – 477]  partition entry 2
//   [478 – 493]  partition entry 3
//   [494 – 509]  partition entry 4
//   [510 – 511]  boot signature: 0x55 0xAA
//
// Partition entry (16 bytes, all little-endian):
//   [0]      status (0x80 = bootable)
//   [1–3]    CHS start  (ignored; use LBA)
//   [4]      type byte
//   [5–7]    CHS end    (ignored)
//   [8–11]   LBA start  (uint32)
//   [12–15]  sector count (uint32)
package mbr

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/carbon-os/diskimg/partition"
)

const (
	mbrSize         = 512
	partTableOffset = 446
	partEntrySize   = 16
	partEntryCount  = 4
	sigOffset       = 510
	sig0            = 0x55
	sig1            = 0xAA
)

// Parse reads the MBR at offset 0 of r and returns the active partitions.
// Returns an error if the boot signature is missing.
func Parse(r io.ReaderAt, sectorSize int64) ([]*partition.Partition, error) {
	buf := make([]byte, mbrSize)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("mbr: read: %w", err)
	}

	if buf[sigOffset] != sig0 || buf[sigOffset+1] != sig1 {
		return nil, fmt.Errorf("mbr: bad signature: %02X %02X", buf[sigOffset], buf[sigOffset+1])
	}

	var parts []*partition.Partition
	for i := 0; i < partEntryCount; i++ {
		e := buf[partTableOffset+i*partEntrySize : partTableOffset+(i+1)*partEntrySize]
		typeByte := e[4]
		if typeByte == 0x00 {
			continue // unused entry
		}
		lbaStart := binary.LittleEndian.Uint32(e[8:12])
		sectors := binary.LittleEndian.Uint32(e[12:16])
		if lbaStart == 0 || sectors == 0 {
			continue
		}
		startBytes := int64(lbaStart) * sectorSize
		sizeBytes := int64(sectors) * sectorSize

		parts = append(parts, &partition.Partition{
			Index:      i + 1,
			StartLBA:   uint64(lbaStart),
			EndLBA:     uint64(lbaStart) + uint64(sectors) - 1,
			StartBytes: startBytes,
			SizeBytes:  sizeBytes,
			TypeByte:   typeByte,
			Bootable:   e[0] == 0x80,
		})
	}

	if len(parts) == 0 {
		return nil, fmt.Errorf("mbr: no active partitions")
	}
	return parts, nil
}