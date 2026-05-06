// Package gpt parses GUID Partition Tables (GPT).
//
// GPT disk layout (sector = 512 bytes default):
//   LBA 0       protective MBR
//   LBA 1       primary GPT header (92 bytes, padded to sector)
//   LBA 2–33    partition entry array (128 entries × 128 bytes)
//   LBA 34+     first usable LBA (partitions live here)
//   LBA -33..-2 backup partition entry array
//   LBA -1      backup GPT header
//
// GPT header fields (little-endian unless noted):
//   [0–7]   signature "EFI PART"
//   [8–11]  revision
//   [12–15] header size (92)
//   [16–19] header CRC32 (zeroed during calculation)
//   [24–31] current LBA (uint64)
//   [32–39] backup LBA  (uint64)
//   [40–47] first usable LBA
//   [48–55] last usable LBA
//   [56–71] disk GUID
//   [72–79] partition entries start LBA
//   [80–83] number of partition entries
//   [84–87] size of each entry (128)
//   [88–91] CRC32 of partition array
package gpt

import (
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"

	"github.com/carbon-os/diskimg/partition"
)

var gptSignature = [8]byte{'E', 'F', 'I', ' ', 'P', 'A', 'R', 'T'}

const (
	gptHeaderOffset  = 512 // LBA 1 at 512-byte sectors
	gptHeaderMinSize = 92
	gptEntrySize     = 128
	gptNameLen       = 72 // bytes = 36 UTF-16LE chars
)

// Parse reads the primary GPT header and partition entries from r.
func Parse(r io.ReaderAt, diskSize, sectorSize int64) ([]*partition.Partition, error) {
	hdr := make([]byte, sectorSize)
	if _, err := r.ReadAt(hdr, sectorSize); err != nil { // LBA 1
		return nil, fmt.Errorf("gpt: read header: %w", err)
	}

	if [8]byte(hdr[0:8]) != gptSignature {
		return nil, fmt.Errorf("gpt: bad signature")
	}

	hdrSize := binary.LittleEndian.Uint32(hdr[12:16])
	if hdrSize < gptHeaderMinSize {
		return nil, fmt.Errorf("gpt: header size %d too small", hdrSize)
	}

	entriesLBA := binary.LittleEndian.Uint64(hdr[72:80])
	numEntries := binary.LittleEndian.Uint32(hdr[80:84])
	entrySize := binary.LittleEndian.Uint32(hdr[84:88])
	if entrySize < gptEntrySize {
		return nil, fmt.Errorf("gpt: entry size %d too small", entrySize)
	}

	entriesOff := int64(entriesLBA) * sectorSize
	var parts []*partition.Partition

	for i := uint32(0); i < numEntries; i++ {
		entryOff := entriesOff + int64(i)*int64(entrySize)
		entry := make([]byte, entrySize)
		if _, err := r.ReadAt(entry, entryOff); err != nil {
			return nil, fmt.Errorf("gpt: read entry %d: %w", i, err)
		}

		typeGUID := GUID(entry[0:16])
		if typeGUID.IsZero() {
			continue // unused
		}

		uniqueGUID := GUID(entry[16:32])
		startLBA := binary.LittleEndian.Uint64(entry[32:40])
		endLBA := binary.LittleEndian.Uint64(entry[40:48])

		// Partition name: UTF-16LE, up to 36 chars
		nameBytes := entry[56 : 56+gptNameLen]
		name := decodeUTF16LE(nameBytes)

		startBytes := int64(startLBA) * sectorSize
		sizeBytes := (int64(endLBA)-int64(startLBA)+1) * sectorSize

		parts = append(parts, &partition.Partition{
			Index:      len(parts) + 1,
			StartLBA:   startLBA,
			EndLBA:     endLBA,
			StartBytes: startBytes,
			SizeBytes:  sizeBytes,
			TypeGUID:   typeGUID.String(),
			UniqueGUID: uniqueGUID.String(),
			Name:       name,
		})
	}

	if len(parts) == 0 {
		return nil, fmt.Errorf("gpt: no partitions found")
	}
	return parts, nil
}

// decodeUTF16LE converts a UTF-16LE byte slice to a Go string.
// Stops at the first null character.
func decodeUTF16LE(b []byte) string {
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	// trim at null
	for i, c := range u16 {
		if c == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}