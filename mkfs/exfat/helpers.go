package exfat

import (
	"encoding/binary"
	"math/bits"
)

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

func isPow2(n int) bool { return n > 0 && n&(n-1) == 0 }

func roundUpI(n, m int64) int64 {
	if m == 0 {
		return n
	}
	return ((n + m - 1) / m) * m
}

// floorLog2 returns floor(log₂(x)) for x ≥ 1.
func floorLog2(x int) uint8 { return uint8(bits.Len(uint(x)) - 1) }

// spcShift returns the SectorsPerClusterShift targeting a cluster size
// appropriate for the given volume.
func spcShift(sizeBytes int64, ss int) uint8 {
	mb := sizeBytes / (1024 * 1024)
	var clusterBytes int64
	switch {
	case mb <= 256:
		clusterBytes = 4096
	case mb <= 32*1024:
		clusterBytes = 32768
	default:
		clusterBytes = 131072
	}
	spc := clusterBytes / int64(ss)
	if spc < 1 {
		spc = 1
	}
	return floorLog2(int(spc))
}