// Package gpt parses GUID Partition Tables.
package gpt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"

	"github.com/carbon-os/diskimg/partition"
)

const (
	sectorSize        = 512
	gptSignature      = "EFI PART"
	gptHeaderLBA      = 1                  // primary header at LBA 1
	gptHeaderSize     = 92                 // header is 92 bytes
	gptPartEntrySize  = 128
	gptPartEntryStart = 2                  // partition entries begin at LBA 2
	gptMaxPartitions  = 128
)

// header is the binary layout of the GPT header (92 bytes, LE).
type header struct {
	Signature      [8]byte
	Revision       uint32
	HeaderSize     uint32
	HeaderCRC32    uint32
	Reserved       uint32
	MyLBA          uint64
	AlternateLBA   uint64
	FirstUsableLBA uint64
	LastUsableLBA  uint64
	DiskGUID       [16]byte
	PartEntryLBA   uint64
	NumPartEntries uint32
	PartEntrySize  uint32
	PartArrayCRC32 uint32
}

// partEntry is the 128-byte GPT partition entry layout.
type partEntry struct {
	TypeGUID   [16]byte
	UniqueGUID [16]byte
	StartLBA   uint64
	EndLBA     uint64
	Attributes uint64
	Name       [72]byte // UTF-16LE, up to 36 chars
}

// Parse reads a GPT from r.  Returns nil without error if no GPT is found.
func Parse(r io.ReaderAt) ([]*partition.Partition, error) {
	// Read LBA 0 to check for protective MBR (type 0xEE means GPT present).
	mbr := make([]byte, sectorSize)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		return nil, fmt.Errorf("gpt: read mbr: %w", err)
	}
	hasProtective := false
	for i := 0; i < 4; i++ {
		off := 0x1BE + i*16
		if mbr[off+4] == 0xEE {
			hasProtective = true
			break
		}
	}
	if !hasProtective {
		return nil, nil // not GPT
	}

	// Read and validate primary GPT header at LBA 1.
	hdrBuf := make([]byte, sectorSize)
	if _, err := r.ReadAt(hdrBuf, gptHeaderLBA*sectorSize); err != nil {
		return nil, fmt.Errorf("gpt: read header: %w", err)
	}
	if string(hdrBuf[:8]) != gptSignature {
		return nil, fmt.Errorf("gpt: invalid signature")
	}

	var hdr header
	br := bytes.NewReader(hdrBuf)
	if err := binary.Read(br, binary.LittleEndian, &hdr); err != nil {
		return nil, fmt.Errorf("gpt: parse header: %w", err)
	}

	// Read partition entries.
	entryAreaSize := int(hdr.NumPartEntries) * int(hdr.PartEntrySize)
	entryBuf := make([]byte, entryAreaSize)
	if _, err := r.ReadAt(entryBuf, int64(hdr.PartEntryLBA)*sectorSize); err != nil {
		return nil, fmt.Errorf("gpt: read partition entries: %w", err)
	}

	var parts []*partition.Partition
	for i := 0; i < int(hdr.NumPartEntries); i++ {
		off := i * int(hdr.PartEntrySize)
		var e partEntry
		er := bytes.NewReader(entryBuf[off : off+int(hdr.PartEntrySize)])
		if err := binary.Read(er, binary.LittleEndian, &e); err != nil {
			continue
		}
		// Skip empty entries (all zeros in type GUID).
		if isZero(e.TypeGUID[:]) {
			continue
		}
		name := decodeUTF16LE(e.Name[:])
		parts = append(parts, &partition.Partition{
			Index:     i + 1,
			StartByte: int64(e.StartLBA) * sectorSize,
			SizeBytes: int64(e.EndLBA-e.StartLBA+1) * sectorSize,
			TypeGUID:  formatGUID(e.TypeGUID),
			Name:      name,
		})
	}
	return parts, nil
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// formatGUID encodes a 16-byte mixed-endian GUID to its string form.
func formatGUID(b [16]byte) string {
	// GPT GUIDs: first 3 components little-endian, last 2 big-endian.
	p1 := binary.LittleEndian.Uint32(b[0:4])
	p2 := binary.LittleEndian.Uint16(b[4:6])
	p3 := binary.LittleEndian.Uint16(b[6:8])
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		p1, p2, p3,
		b[8], b[9],
		b[10], b[11], b[12], b[13], b[14], b[15])
}

// decodeUTF16LE converts the UTF-16LE name field to a Go string.
func decodeUTF16LE(b []byte) string {
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	// Trim null terminator.
	for i, v := range u16 {
		if v == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}