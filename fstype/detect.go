// Package fstype identifies filesystem types by probing magic bytes.
package fstype

import "io"

// Type names the detected filesystem.
type Type string

const (
	Ext4    Type = "ext4"
	FAT32   Type = "fat32"
	FAT16   Type = "fat16"
	FAT12   Type = "fat12"
	Unknown Type = "unknown"
)

func (t Type) String() string { return string(t) }

// Detect probes the partition at (offset, size) inside r.
// Returns Unknown (not an error) if the format is unrecognised.
func Detect(r io.ReaderAt, offset, _ int64) (Type, error) {
	if isExt4(r, offset) {
		return Ext4, nil
	}
	if t, ok := fatType(r, offset); ok {
		return t, nil
	}
	return Unknown, nil
}

// isExt4 checks magic 0xEF53 at superblock offset 0x38.
// Superblock lives at partition+1024; magic at partition+1024+56 = partition+1080.
func isExt4(r io.ReaderAt, partOff int64) bool {
	var buf [2]byte
	if _, err := r.ReadAt(buf[:], partOff+1080); err != nil {
		return false
	}
	return buf[0] == 0x53 && buf[1] == 0xEF // LE 0xEF53
}

// fatType detects FAT12/16/32 from the BIOS Parameter Block.
func fatType(r io.ReaderAt, partOff int64) (Type, bool) {
	var buf [90]byte
	if _, err := r.ReadAt(buf[:], partOff); err != nil {
		return Unknown, false
	}
	switch string(buf[82:90]) {
	case "FAT32   ":
		return FAT32, true
	}
	switch string(buf[54:62]) {
	case "FAT16   ":
		return FAT16, true
	case "FAT12   ":
		return FAT12, true
	}
	return Unknown, false
}