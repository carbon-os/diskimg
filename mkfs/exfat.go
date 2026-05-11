package mkfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"time"
	"unicode/utf16"
)

// ExFAT writes a complete exFAT filesystem onto rw.
// rw must be positioned at offset 0 (first byte of the partition).
//
// The root directory will contain an Allocation Bitmap entry, an Up-case
// Table entry, and — if opts.Label is non-empty — a Volume Label entry.
//
// Note: the Up-case Table written here maps only the 26 ASCII lowercase
// letters to uppercase. Full Unicode case mapping is outside the scope of
// this formatter; callers targeting multilingual workloads should replace
// the table post-format.
func ExFAT(rw io.ReadWriteSeeker, sizeBytes int64, opts Options) error {
	ss := opts.sectorSize()
	if !isPow2(ss) || ss < 512 || ss > 4096 {
		return fmt.Errorf("mkfs.ExFAT: unsupported sector size %d", ss)
	}

	totalSectors := sizeBytes / int64(ss)
	if totalSectors < 2048 {
		return fmt.Errorf("mkfs.ExFAT: volume too small (need ≥ 2 048 sectors)")
	}

	bpsShift := floorLog2(ss)
	spcShift := exfatSPCShift(sizeBytes, ss)
	spc := int64(1) << spcShift     // sectors per cluster
	clusterBytes := spc * int64(ss) // bytes per cluster

	// ── Layout ───────────────────────────────────────────────────────────
	//   [0, 24)               boot region  (12 primary + 12 backup sectors)
	//   [24, 24+fatSectors)   FAT
	//   [clusterHeapOffset, ) cluster heap
	const bootRegionSectors = 24
	fatOffset := int64(bootRegionSectors)

	// Upper-bound cluster count for a conservative FAT-size estimate.
	maxClusters := totalSectors / spc
	entriesPerSector := int64(ss) / 4
	fatSectors := roundUpI((maxClusters+2+entriesPerSector-1)/entriesPerSector, spc)

	clusterHeapOffset := roundUpI(fatOffset+fatSectors, spc)
	clusterCount := (totalSectors - clusterHeapOffset) / spc
	if clusterCount < 3 {
		return fmt.Errorf("mkfs.ExFAT: volume too small for metadata")
	}

	// Cluster allocation map:
	//   cluster 2              : allocation bitmap
	//   clusters 3 … 2+U       : up-case table   (U clusters)
	//   cluster 2+U+1          : root directory
	const upcaseTableBytes = 131072 // 64 K × 2-byte entries, uncompressed
	upcaseClusters := int64((upcaseTableBytes + clusterBytes - 1) / clusterBytes)

	bitmapBytes := (clusterCount + 7) / 8
	bitmapClusters := int64((bitmapBytes + clusterBytes - 1) / clusterBytes)
	if bitmapClusters < 1 {
		bitmapClusters = 1
	}

	upcaseFirstCluster := int64(2) + bitmapClusters
	rootDirCluster := upcaseFirstCluster + upcaseClusters

	if rootDirCluster+1 > clusterCount+2 {
		return fmt.Errorf("mkfs.ExFAT: not enough clusters for filesystem metadata")
	}

	volSerial := rand.New(rand.NewSource(time.Now().UnixNano())).Uint32()

	// ── Build the 12-sector primary boot region in memory ─────────────────
	bootRegion := make([]byte, bootRegionSectors/2*int(ss)) // 12 sectors

	// Sector 0: Main Boot Sector (VBR)
	vbr := bootRegion[:ss]
	vbr[0], vbr[1], vbr[2] = 0xEB, 0x76, 0x90             // jump boot
	copy(vbr[3:11], "EXFAT   ")                             // OEM name
	// bytes 11–63: MustBeZero
	putU64(vbr[64:], 0)                                     // PartitionOffset (0 = unknown)
	putU64(vbr[72:], uint64(totalSectors))                  // VolumeLength
	putU32(vbr[80:], uint32(fatOffset))                     // FATOffset
	putU32(vbr[84:], uint32(fatSectors))                    // FATLength
	putU32(vbr[88:], uint32(clusterHeapOffset))             // ClusterHeapOffset
	putU32(vbr[92:], uint32(clusterCount))                  // ClusterCount
	putU32(vbr[96:], uint32(rootDirCluster))                // FirstClusterOfRootDirectory
	putU32(vbr[100:], volSerial)                            // VolumeSerialNumber
	putU16(vbr[104:], 0x0100)                               // FileSystemRevision 1.00
	putU16(vbr[106:], 0)                                    // VolumeFlags (excluded from checksum)
	vbr[108] = bpsShift                                     // BytesPerSectorShift
	vbr[109] = spcShift                                     // SectorsPerClusterShift
	vbr[110] = 1                                            // NumberOfFATs
	vbr[111] = 0x80                                         // DriveSelect
	vbr[112] = 0                                            // PercentInUse (excluded from checksum)
	vbr[ss-2] = 0x55                                        // BootSignature
	vbr[ss-1] = 0xAA

	// Sectors 1–8: Extended Boot Sectors (each ends with 0xAA550000).
	for i := 1; i <= 8; i++ {
		sec := bootRegion[i*ss : (i+1)*ss]
		sec[ss-4] = 0x00
		sec[ss-3] = 0x00
		sec[ss-2] = 0x55
		sec[ss-1] = 0xAA
	}
	// Sector 9:  OEM Parameters — all zeros (already zero from make).
	// Sector 10: Reserved      — all zeros.

	// Sector 11: Boot Checksum — 32-bit checksum of sectors 0–10 tiled to fill.
	csum := exfatBootChecksum(bootRegion[:11*ss], ss)
	csumSec := bootRegion[11*ss : 12*ss]
	for i := 0; i+3 < ss; i += 4 {
		binary.LittleEndian.PutUint32(csumSec[i:], csum)
	}

	// Write primary boot region then backup (identical content).
	if _, err := rw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for range [2]struct{}{} {
		if _, err := rw.Write(bootRegion); err != nil {
			return fmt.Errorf("mkfs.ExFAT: write boot region: %w", err)
		}
	}

	// ── FAT (written sector-by-sector to avoid large allocations) ─────────
	// Entries 0, 1: reserved. Entries for bitmap, upcase, root-dir chains.
	// All other entries: 0x00000000 (free cluster).
	for s := int64(0); s < fatSectors; s++ {
		sec := make([]byte, ss)
		for e := int64(0); e < entriesPerSector; e++ {
			cluster := s*entriesPerSector + e
			var val uint32
			switch {
			case cluster == 0:
				val = 0xFFFFFFF8
			case cluster == 1:
				val = 0xFFFFFFFF
			// Bitmap chain (clusters 2 … 2+bitmapClusters-1)
			case cluster >= 2 && cluster < 2+bitmapClusters:
				if cluster == 2+bitmapClusters-1 {
					val = 0xFFFFFFFF
				} else {
					val = uint32(cluster + 1)
				}
			// Upcase chain (clusters upcaseFirstCluster … upcaseFirstCluster+upcaseClusters-1)
			case cluster >= upcaseFirstCluster && cluster < upcaseFirstCluster+upcaseClusters:
				if cluster == upcaseFirstCluster+upcaseClusters-1 {
					val = 0xFFFFFFFF
				} else {
					val = uint32(cluster + 1)
				}
			// Root directory (single cluster)
			case cluster == rootDirCluster:
				val = 0xFFFFFFFF
			}
			binary.LittleEndian.PutUint32(sec[e*4:], val)
		}
		if _, err := rw.Write(sec); err != nil {
			return fmt.Errorf("mkfs.ExFAT: write FAT sector %d: %w", s, err)
		}
	}

	// ── Alignment gap between FAT end and cluster heap start ──────────────
	fatEndSector := fatOffset + fatSectors
	if clusterHeapOffset > fatEndSector {
		gap := make([]byte, (clusterHeapOffset-fatEndSector)*int64(ss))
		if _, err := rw.Write(gap); err != nil {
			return fmt.Errorf("mkfs.ExFAT: write cluster heap alignment: %w", err)
		}
	}

	// ── Cluster 2: Allocation Bitmap ──────────────────────────────────────
	bitmapBuf := make([]byte, bitmapClusters*clusterBytes)
	// Mark clusters 2 through rootDirCluster as allocated (bit 0 = cluster 2).
	for c := int64(2); c <= rootDirCluster; c++ {
		bit := c - 2
		bitmapBuf[bit/8] |= 1 << (bit % 8)
	}
	if _, err := rw.Write(bitmapBuf); err != nil {
		return fmt.Errorf("mkfs.ExFAT: write allocation bitmap: %w", err)
	}

	// ── Clusters upcaseFirstCluster … : Up-case Table ─────────────────────
	upcaseBuf := buildUpcaseTable()
	upcaseChecksum := upcaseTableChecksum(upcaseBuf)
	// Pad to a whole number of clusters.
	upcasePadded := make([]byte, upcaseClusters*clusterBytes)
	copy(upcasePadded, upcaseBuf)
	if _, err := rw.Write(upcasePadded); err != nil {
		return fmt.Errorf("mkfs.ExFAT: write upcase table: %w", err)
	}

	// ── Root Directory cluster ────────────────────────────────────────────
	rootDir := make([]byte, clusterBytes)
	pos := 0
	if opts.Label != "" {
		writeExFATLabelEntry(rootDir[pos:], opts.Label)
		pos += 32
	}
	writeExFATBitmapEntry(rootDir[pos:], uint32(2), uint64(bitmapBytes))
	pos += 32
	writeExFATUpcaseEntry(rootDir[pos:], upcaseChecksum, uint32(upcaseFirstCluster), upcaseTableBytes)
	if _, err := rw.Write(rootDir); err != nil {
		return fmt.Errorf("mkfs.ExFAT: write root directory: %w", err)
	}

	return nil
}

// ── exFAT helpers ─────────────────────────────────────────────────────────────

// exfatSPCShift returns the SectorsPerClusterShift (log₂ of sectors-per-cluster)
// that targets a cluster size appropriate for the given volume.
func exfatSPCShift(sizeBytes int64, ss int) uint8 {
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

// exfatBootChecksum computes the boot checksum over the first 11 sectors,
// excluding the VolumeFlags (bytes 106–107) and PercentInUse (byte 112).
func exfatBootChecksum(data []byte, ss int) uint32 {
	var sum uint32
	limit := 11 * ss
	for i := 0; i < limit; i++ {
		if i == 106 || i == 107 || i == 112 {
			continue
		}
		sum = (sum>>1 | sum<<31) + uint32(data[i])
	}
	return sum
}

// buildUpcaseTable returns an uncompressed 128 KB exFAT up-case table that
// maps the 26 ASCII lowercase letters to uppercase and leaves all other
// code points as identity mappings.
func buildUpcaseTable() []byte {
	table := make([]byte, 131072)
	for i := 0; i < 65536; i++ {
		var upper uint16
		if i >= 0x61 && i <= 0x7A {
			upper = uint16(i - 0x20)
		} else {
			upper = uint16(i)
		}
		binary.LittleEndian.PutUint16(table[i*2:], upper)
	}
	return table
}

// upcaseTableChecksum computes the simple rotate-right-and-add checksum
// stored in the Up-case Table directory entry.
func upcaseTableChecksum(table []byte) uint32 {
	var sum uint32
	for _, b := range table {
		sum = (sum>>1 | sum<<31) + uint32(b)
	}
	return sum
}

// ── root directory entry writers ─────────────────────────────────────────────

// writeExFATLabelEntry writes a Volume Label directory entry (type 0x83).
func writeExFATLabelEntry(buf []byte, label string) {
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
		u := utf16.Encode([]rune{r})
		if len(u) > 0 {
			binary.LittleEndian.PutUint16(buf[2+i*2:], u[0])
		}
	}
}

// writeExFATBitmapEntry writes an Allocation Bitmap directory entry (type 0x81).
func writeExFATBitmapEntry(buf []byte, firstCluster uint32, dataLength uint64) {
	if len(buf) < 32 {
		return
	}
	buf[0] = 0x81
	// buf[1] = 0: first bitmap (not TexFAT second bitmap)
	putU32(buf[20:], firstCluster)
	putU64(buf[24:], dataLength)
}

// writeExFATUpcaseEntry writes an Up-case Table directory entry (type 0x82).
func writeExFATUpcaseEntry(buf []byte, checksum uint32, firstCluster uint32, dataLength uint64) {
	if len(buf) < 32 {
		return
	}
	buf[0] = 0x82
	putU32(buf[4:], checksum)
	putU32(buf[20:], firstCluster)
	putU64(buf[24:], dataLength)
}