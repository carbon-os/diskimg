// Package fat provides basic FAT32 filesystem reading for disk image extraction.
package fat

import "io"

// IsFAT32 probes the BPB at the start of the partition reader.
func IsFAT32(r io.ReaderAt, partOffset int64) bool {
	var buf [90]byte
	if _, err := r.ReadAt(buf[:], partOffset); err != nil {
		return false
	}
	return string(buf[82:90]) == "FAT32   "
}