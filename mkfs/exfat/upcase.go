package exfat

import "encoding/binary"

// UpcaseTableBytes is the fixed size of the uncompressed exFAT up-case table.
const UpcaseTableBytes = 131072 // 64 K entries × 2 bytes

// buildUpcaseTable returns a 128 KB exFAT up-case table mapping the 26 ASCII
// lowercase letters to uppercase; all other code points are identity-mapped.
//
// Note: full Unicode case mapping is outside scope; callers targeting
// multilingual workloads should replace the table post-format.
func buildUpcaseTable() []byte {
	table := make([]byte, UpcaseTableBytes)
	for i := 0; i < 65536; i++ {
		upper := uint16(i)
		if i >= 0x61 && i <= 0x7A {
			upper = uint16(i - 0x20)
		}
		binary.LittleEndian.PutUint16(table[i*2:], upper)
	}
	return table
}

// upcaseChecksum computes the rotate-right-add checksum stored in the
// Up-case Table directory entry.
func upcaseChecksum(table []byte) uint32 {
	var sum uint32
	for _, b := range table {
		sum = (sum>>1 | sum<<31) + uint32(b)
	}
	return sum
}