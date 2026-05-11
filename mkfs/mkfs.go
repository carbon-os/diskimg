// Package mkfs provides low-level filesystem formatters.
//
// Each formatter writes a complete, mountable filesystem onto an
// io.ReadWriteSeeker — whether that is a raw partition stream from
// Builder.OpenRaw, a plain *os.File, or an in-memory buffer.
// The caller is responsible for ensuring rw is seeked to position 0
// (the first byte of the target partition) before calling.
package mkfs

import (
	"encoding/binary"
	"math/bits"
)

// Options configures filesystem formatting.
type Options struct {
	// Label is the volume label.
	//   FAT32 : up to 11 ASCII characters, uppercased and space-padded.
	//   exFAT : up to 11 UTF-16LE code units.
	Label string

	// SectorSize is the logical sector size in bytes. Defaults to 512.
	// Must be a power of two in [512, 4096].
	SectorSize int
}

func (o Options) sectorSize() int {
	if o.SectorSize == 0 {
		return 512
	}
	return o.SectorSize
}

// ── shared binary helpers ─────────────────────────────────────────────────────

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

// roundUpI rounds n up to the nearest multiple of m.
func roundUpI(n, m int64) int64 {
	if m == 0 {
		return n
	}
	return ((n + m - 1) / m) * m
}

// floorLog2 returns floor(log₂(x)) for x ≥ 1.
func floorLog2(x int) uint8 {
	return uint8(bits.Len(uint(x)) - 1)
}

func isPow2(n int) bool { return n > 0 && n&(n-1) == 0 }