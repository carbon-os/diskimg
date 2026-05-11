package ntfs

// buildBootSector returns the 512-byte (or ss-byte) NTFS Volume Boot Record.
//
// Layout (all fields little-endian unless noted):
//
//	0x00-0x02  jump instruction EB 52 90
//	0x03-0x0A  OEM ID "NTFS    "
//	0x0B-0x53  BPB + extended BPB
//	0x54-0x1FD bootstrap code (zeros for non-bootable images)
//	0x1FE-0x1FF boot signature 55 AA
//
// Extended BPB fields used by the NTFS boot loader:
//
//	0x28  8  sectors per volume   (totalSectors - 1; last sector = backup)
//	0x30  8  first cluster of $MFT
//	0x38  8  first cluster of $MFTMirr
//	0x40  1  clusters per MFT record  (negative → 2^|n| bytes; 0xF6 = 1 KiB)
//	0x44  1  clusters per index entry (negative → 2^|n| bytes; 0x01 = 1 cluster)
//	0x48  8  volume serial number
func buildBootSector(l *fsLayout, serial uint64) []byte {
	ss := l.ss
	boot := make([]byte, ss)

	// Jump + OEM ID
	boot[0], boot[1], boot[2] = 0xEB, 0x52, 0x90
	copy(boot[3:11], "NTFS    ")

	// ── BPB ────────────────────────────────────────────────────────────────
	putU16(boot[0x0B:], uint16(ss))  // bytes per sector
	boot[0x0D] = uint8(l.spc)        // sectors per cluster
	// 0x0E-0x0F reserved sectors — 0 in NTFS
	// 0x10-0x12 FAT count / root entries etc. — 0
	boot[0x15] = 0xF8 // media type: fixed disk
	putU16(boot[0x18:], 63)           // sectors per track (nominal)
	putU16(boot[0x1A:], 255)          // number of heads (nominal)
	// 0x1C hidden sectors — 0 (partition offset unknown to formatter)

	// ── Extended BPB ───────────────────────────────────────────────────────
	boot[0x24] = 0x80 // BIOS drive number (first hard disk)
	boot[0x26] = 0x80 // extended boot signature
	// Sectors per volume: total sectors minus the backup boot sector.
	putU64(boot[0x28:], uint64(l.totalSectors-1))
	putU64(boot[0x30:], uint64(l.mftLCN))     // first cluster of $MFT
	putU64(boot[0x38:], uint64(l.mftMirrLCN)) // first cluster of $MFTMirr
	// MFT record size: 0xF6 = int8(-10) → 2^10 = 1024 bytes.
	boot[0x40] = 0xF6
	// Index entry allocation size.
	// 0x01 = 1 cluster when clusterSize ≥ 4 KiB; otherwise encode as 2^12 = 4 KiB.
	if l.clusterSize >= 4096 {
		boot[0x44] = 0x01
	} else {
		boot[0x44] = 0xF4 // int8(-12) → 2^12 = 4096 bytes
	}
	putU64(boot[0x48:], serial) // volume serial number

	// Boot signature
	boot[ss-2], boot[ss-1] = 0x55, 0xAA
	return boot
}