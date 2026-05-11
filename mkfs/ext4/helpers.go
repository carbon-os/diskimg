package ext4

import (
	"hash/crc32"
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// crc32cUpdate continues a running CRC-32C checksum.
func crc32cUpdate(crc uint32, data []byte) uint32 {
	return crc32.Update(crc, crc32cTable, data)
}