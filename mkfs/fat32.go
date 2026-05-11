package mkfs

import (
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"
)

// FAT32 writes a complete FAT32 filesystem onto rw.
// rw must be positioned at offset 0 (the first byte of the partition).
// sizeBytes is the total partition size; it must be large enough to hold
// at least 65 536 sectors (≈ 32 MB with 512-byte sectors).
func FAT32(rw io.ReadWriteSeeker, sizeBytes int64, opts Options) error {
	ss := opts.sectorSize()
	if !isPow2(ss) || ss < 512 || ss > 4096 {
		return fmt.Errorf("mkfs.FAT32: unsupported sector size %d", ss)
	}

	totalSectors := uint32(sizeBytes / int64(ss))
	if totalSectors < 65536 {
		return fmt.Errorf("mkfs.FAT32: partition too small (%d sectors; need ≥ 65 536)", totalSectors)
	}

	const (
		reservedSectors = 32
		numFATs         = 2
	)
	spc := fat32SPC(sizeBytes, ss)

	// FAT size: Microsoft spec formula for FAT32.
	//   TmpVal1 = TotSec − RsvdSecCnt
	//   TmpVal2 = (256 × SecPerClus + NumFATs) / 2
	//   FATSz   = ⌈TmpVal1 / TmpVal2⌉
	tmpVal1 := uint32(totalSectors - reservedSectors)
	tmpVal2 := uint32(256*uint32(spc)+numFATs) / 2
	fatSize := (tmpVal1 + tmpVal2 - 1) / tmpVal2

	volumeID := rand.New(rand.NewSource(time.Now().UnixNano())).Uint32()
	label := fat32PadLabel(opts.Label)

	// ── Sector 0: Volume Boot Record ─────────────────────────────────────
	vbr := make([]byte, ss)
	vbr[0], vbr[1], vbr[2] = 0xEB, 0x58, 0x90 // jump boot
	copy(vbr[3:11], "MSDOS5.0")                  // OEM name
	putU16(vbr[11:], uint16(ss))                  // bytes per sector
	vbr[13] = spc                                 // sectors per cluster
	putU16(vbr[14:], reservedSectors)             // reserved sectors
	vbr[16] = numFATs                             // number of FATs
	// vbr[17:19] = 0  RootEntCnt (0 = FAT32)
	// vbr[19:21] = 0  TotSec16   (0 = FAT32)
	vbr[21] = 0xF8                                // media type: fixed disk
	// vbr[22:24] = 0  FATSz16    (0 = FAT32)
	putU16(vbr[24:], 63)                          // sectors per track
	putU16(vbr[26:], 255)                         // number of heads
	// vbr[28:32] = 0  hidden sectors
	putU32(vbr[32:], totalSectors)                // TotSec32
	putU32(vbr[36:], fatSize)                     // FATSz32
	// vbr[40:42] = 0  ExtFlags (mirror both FATs)
	// vbr[42:44] = 0  FSVer 0.0
	putU32(vbr[44:], 2)                           // root cluster = 2
	putU16(vbr[48:], 1)                           // FSInfo sector
	putU16(vbr[50:], 6)                           // backup boot sector
	vbr[64] = 0x80                                // drive number
	vbr[66] = 0x29                                // extended boot signature
	putU32(vbr[67:], volumeID)                    // volume serial number
	copy(vbr[71:82], label)                       // volume label (11 bytes)
	copy(vbr[82:90], "FAT32   ")                  // FS type string
	vbr[ss-2] = 0x55                              // boot sector signature
	vbr[ss-1] = 0xAA

	// ── Sector 1: FSInfo ─────────────────────────────────────────────────
	fsinfo := make([]byte, ss)
	putU32(fsinfo[0:], 0x41615252)   // lead signature
	putU32(fsinfo[484:], 0x61417272) // structure signature
	putU32(fsinfo[488:], 0xFFFFFFFF) // free cluster count: unknown
	putU32(fsinfo[492:], 0xFFFFFFFF) // next free cluster: unknown
	putU32(fsinfo[508:], 0xAA550000) // trail signature

	empty := make([]byte, ss)

	// ── Reserved region (sectors 0–31) ────────────────────────────────────
	// Sector 0 = VBR, 1 = FSInfo, 2–5 = empty, 6 = backup VBR,
	// 7 = backup FSInfo, 8–31 = empty.
	if _, err := rw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for i := 0; i < reservedSectors; i++ {
		sec := empty
		switch i {
		case 0, 6:
			sec = vbr
		case 1, 7:
			sec = fsinfo
		}
		if _, err := rw.Write(sec); err != nil {
			return fmt.Errorf("mkfs.FAT32: write reserved sector %d: %w", i, err)
		}
	}

	// ── FAT1 then FAT2 ────────────────────────────────────────────────────
	// FAT[0] = 0x0FFFFFF8  (media descriptor word)
	// FAT[1] = 0x0FFFFFFF  (end-of-chain)
	// FAT[2] = 0x0FFFFFFF  (root directory, single cluster, end-of-chain)
	fatSec := make([]byte, ss)
	for fat := 0; fat < numFATs; fat++ {
		putU32(fatSec[0:], 0x0FFFFFF8)
		putU32(fatSec[4:], 0x0FFFFFFF)
		putU32(fatSec[8:], 0x0FFFFFFF)
		for s := uint32(0); s < fatSize; s++ {
			if _, err := rw.Write(fatSec); err != nil {
				return fmt.Errorf("mkfs.FAT32: write FAT%d sector %d: %w", fat+1, s, err)
			}
			if s == 0 {
				// Clear the three init entries; remainder of FAT is all zeros.
				putU32(fatSec[0:], 0)
				putU32(fatSec[4:], 0)
				putU32(fatSec[8:], 0)
			}
		}
	}

	// ── Root directory cluster (cluster 2, one cluster) ───────────────────
	clusterBytes := int(spc) * ss
	rootDir := make([]byte, clusterBytes)
	if opts.Label != "" {
		writeFAT32VolumeLabel(rootDir, opts.Label)
	}
	if _, err := rw.Write(rootDir); err != nil {
		return fmt.Errorf("mkfs.FAT32: write root directory: %w", err)
	}

	return nil
}

// fat32SPC selects the sectors-per-cluster value following Microsoft's
// recommendations for a given volume size and sector size.
func fat32SPC(sizeBytes int64, ss int) uint8 {
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

// fat32PadLabel returns s uppercased and padded / truncated to exactly 11 bytes.
func fat32PadLabel(s string) []byte {
	if s == "" {
		s = "NO NAME"
	}
	s = strings.ToUpper(s)
	b := make([]byte, 11)
	for i := range b {
		b[i] = ' '
	}
	copy(b, []byte(s))
	return b
}

// writeFAT32VolumeLabel writes a volume-label (ATTR_VOLUME_ID) directory entry
// into the first 32 bytes of buf, which must be the root directory cluster.
func writeFAT32VolumeLabel(buf []byte, label string) {
	if len(buf) < 32 {
		return
	}
	// Bytes 0-10: name field holds the full 11-byte label.
	l := fat32PadLabel(label)
	copy(buf[:11], l)
	buf[11] = 0x08 // ATTR_VOLUME_ID
	// All other fields (time, date, cluster, size) remain zero.
}