package fat32

import (
	"math/rand"
	"time"
)

// buildVBR constructs the 512-byte (or larger) Volume Boot Record.
func buildVBR(ss int, totalSectors, fatSize uint32, spc uint8, opts Options) []byte {
	vbr := make([]byte, ss)
	volumeID := rand.New(rand.NewSource(time.Now().UnixNano())).Uint32()

	vbr[0], vbr[1], vbr[2] = 0xEB, 0x58, 0x90 // jump boot
	copy(vbr[3:11], "MSDOS5.0")
	putU16(vbr[11:], uint16(ss))  // bytes per sector
	vbr[13] = spc                 // sectors per cluster
	putU16(vbr[14:], reservedSec) // reserved sectors
	vbr[16] = numFATs
	vbr[21] = 0xF8            // media: fixed disk
	putU16(vbr[24:], 63)      // sectors per track
	putU16(vbr[26:], 255)     // number of heads
	putU32(vbr[32:], totalSectors)
	putU32(vbr[36:], fatSize)
	putU32(vbr[44:], 2)          // root cluster
	putU16(vbr[48:], 1)          // FSInfo sector
	putU16(vbr[50:], 6)          // backup boot sector
	vbr[64] = 0x80               // drive number
	vbr[66] = 0x29               // extended boot signature
	putU32(vbr[67:], volumeID)
	copy(vbr[71:82], PadLabel(opts.Label))
	copy(vbr[82:90], "FAT32   ")
	vbr[ss-2] = 0x55
	vbr[ss-1] = 0xAA
	return vbr
}

// buildFSInfo constructs the FSInfo sector.
func buildFSInfo(ss int) []byte {
	b := make([]byte, ss)
	putU32(b[0:], 0x41615252)
	putU32(b[484:], 0x61417272)
	putU32(b[488:], 0xFFFFFFFF) // free cluster count: unknown
	putU32(b[492:], 0xFFFFFFFF) // next free cluster: unknown
	putU32(b[508:], 0xAA550000) // trail signature
	return b
}