package ntfs

import (
	"encoding/binary"
	"math/bits"
	"time"
	"unicode/utf16"
)

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

func isPow2(n int) bool { return n > 0 && n&(n-1) == 0 }

func floorLog2(x int) int {
	if x <= 0 {
		return 0
	}
	return bits.Len(uint(x)) - 1
}

func roundUpI(n, m int64) int64 {
	if m == 0 {
		return n
	}
	return ((n + m - 1) / m) * m
}

// windowsFiletime converts t to a Windows FILETIME: 100-nanosecond intervals
// since 1 January 1601.
func windowsFiletime(t time.Time) uint64 {
	const epochOffset uint64 = 116444736000000000 // 100 ns intervals
	return uint64(t.UnixNano())/100 + epochOffset
}

// toUTF16LE encodes s as UTF-16LE bytes.
func toUTF16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[i*2:], u)
	}
	return buf
}

// sectorsPerCluster returns the recommended sectors-per-cluster for NTFS
// following Microsoft's size-based table (assumes 512-byte sectors).
func sectorsPerCluster(sizeBytes int64, ss int) int {
	gb := sizeBytes / (1 << 30)
	var clusterBytes int
	switch {
	case gb < 1:
		clusterBytes = 512
	case gb < 2:
		clusterBytes = 1024
	case gb < 4:
		clusterBytes = 2048
	case gb < 16:
		clusterBytes = 4096
	case gb < 64:
		clusterBytes = 8192
	case gb < 256:
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
	return spc
}

// encodeRunList encodes a single contiguous run (startLCN, numClusters) as an
// NTFS run list, terminated by 0x00. NTFS run list encoding:
//   - 1 header byte: high nibble = bytes in offset field, low nibble = bytes in length field
//   - length bytes (unsigned LE)
//   - offset bytes (signed LE delta from previous run; 0 for first run)
func encodeRunList(startLCN, numClusters int64) []byte {
	lenB := varLenUnsigned(uint64(numClusters))
	offB := varLenSigned(startLCN)
	buf := make([]byte, 1+len(lenB)+len(offB)+1) // hdr + len + off + terminator
	buf[0] = byte(len(offB)<<4 | len(lenB))
	copy(buf[1:], lenB)
	copy(buf[1+len(lenB):], offB)
	// buf[last] = 0x00 terminator (zero from make)
	return buf
}

// varLenUnsigned returns the minimum-byte little-endian encoding of v.
func varLenUnsigned(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	n := 8
	for n > 1 && b[n-1] == 0 {
		n--
	}
	return b[:n]
}

// varLenSigned returns the minimum-byte little-endian signed encoding of v,
// preserving two's-complement sign extension.
func varLenSigned(v int64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	n := 8
	for n > 1 {
		if v >= 0 && b[n-1] == 0 && b[n-2] < 0x80 {
			n--
		} else if v < 0 && b[n-1] == 0xFF && b[n-2] >= 0x80 {
			n--
		} else {
			break
		}
	}
	return b[:n]
}

// buildUpcaseTable returns a 128 KB NTFS/exFAT up-case table mapping the 26
// ASCII lower-case letters to upper-case; all other code points are identity.
func buildUpcaseTable() []byte {
	const tableBytes = 131072 // 65 536 entries × 2 bytes
	tbl := make([]byte, tableBytes)
	for i := 0; i < 65536; i++ {
		upper := uint16(i)
		if i >= 0x61 && i <= 0x7A {
			upper = uint16(i - 0x20)
		}
		binary.LittleEndian.PutUint16(tbl[i*2:], upper)
	}
	return tbl
}