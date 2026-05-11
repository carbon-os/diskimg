package fat32

import (
	"encoding/binary"
	"math/bits"
)

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

func isPow2(n int) bool { return n > 0 && n&(n-1) == 0 }

// sectorsPerCluster follows Microsoft's size-based recommendations for FAT32.
func sectorsPerCluster(sizeBytes int64, ss int) uint8 {
	mb := sizeBytes / (1024 * 1024)
	var clusterBytes int
	switch {
	case mb <= 64:
		clusterBytes = 512
	case mb <= 128:
		clusterBytes = 1024
	case mb <= 256:
		clusterBytes = 2048
	case mb <= 8*1024:
		clusterBytes = 4096
	case mb <= 16*1024:
		clusterBytes = 8192
	case mb <= 32*1024:
		clusterBytes = 16384
	default:
		clusterBytes = 32768
	}
	spc := clusterBytes / ss
	if spc < 1 {
		spc = 1
	}
	if spc > 128 {
		spc = 128
	}
	return uint8(spc)
}

// floorLog2 returns floor(log₂(x)) for x ≥ 1.
func floorLog2(x int) uint8 { return uint8(bits.Len(uint(x)) - 1) }