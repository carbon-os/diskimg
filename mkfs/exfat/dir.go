package exfat

import "unicode/utf16"

// writeVolumeLabel writes a Volume Label directory entry (type 0x83) into buf.
func writeVolumeLabel(buf []byte, label string) {
	if len(buf) < 32 {
		return
	}
	runes := []rune(label)
	if len(runes) > 11 {
		runes = runes[:11]
	}
	buf[0] = 0x83
	buf[1] = byte(len(runes))
	for i, r := range runes {
		if u := utf16.Encode([]rune{r}); len(u) > 0 {
			putU16(buf[2+i*2:], u[0])
		}
	}
}

// writeAllocBitmapEntry writes an Allocation Bitmap directory entry (type 0x81).
func writeAllocBitmapEntry(buf []byte, firstCluster uint32, dataLength uint64) {
	if len(buf) < 32 {
		return
	}
	buf[0] = 0x81
	// buf[1] = 0: first bitmap (not TexFAT second bitmap)
	putU32(buf[20:], firstCluster)
	putU64(buf[24:], dataLength)
}

// writeUpcaseEntry writes an Up-case Table directory entry (type 0x82).
func writeUpcaseEntry(buf []byte, checksum, firstCluster uint32, dataLength uint64) {
	if len(buf) < 32 {
		return
	}
	buf[0] = 0x82
	putU32(buf[4:], checksum)
	putU32(buf[20:], firstCluster)
	putU64(buf[24:], dataLength)
}