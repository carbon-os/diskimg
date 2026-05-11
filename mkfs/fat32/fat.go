package fat32

import (
	"fmt"
	"io"
)

// fatSize computes the FAT32 table size in sectors using the Microsoft spec formula.
func fatSize(totalSectors uint32, spc uint8) uint32 {
	tmpVal1 := totalSectors - reservedSec
	tmpVal2 := (256*uint32(spc) + numFATs) / 2
	return (tmpVal1 + tmpVal2 - 1) / tmpVal2
}

// writeFATs writes both FAT copies sequentially to w.
func writeFATs(w io.Writer, ss int, size uint32) error {
	sec := make([]byte, ss)
	for fat := 0; fat < numFATs; fat++ {
		putU32(sec[0:], 0x0FFFFFF8) // media descriptor word
		putU32(sec[4:], 0x0FFFFFFF) // end-of-chain
		putU32(sec[8:], 0x0FFFFFFF) // root dir cluster, end-of-chain
		for s := uint32(0); s < size; s++ {
			if _, err := w.Write(sec); err != nil {
				return fmt.Errorf("fat32: write FAT%d sector %d: %w", fat+1, s, err)
			}
			if s == 0 {
				putU32(sec[0:], 0)
				putU32(sec[4:], 0)
				putU32(sec[8:], 0)
			}
		}
	}
	return nil
}