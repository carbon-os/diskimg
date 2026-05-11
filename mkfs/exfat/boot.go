package exfat

import (
	"encoding/binary"
	"math/rand"
	"time"
)

// buildBootRegion builds the 12-sector primary boot region in memory.
// The caller writes it twice (primary then backup).
func buildBootRegion(ss int, layout *fsLayout, opts Options) []byte {
	bootRegion := make([]byte, 12*ss)
	volSerial := rand.New(rand.NewSource(time.Now().UnixNano())).Uint32()

	// Sector 0: Main Boot Sector (VBR).
	vbr := bootRegion[:ss]
	vbr[0], vbr[1], vbr[2] = 0xEB, 0x76, 0x90 // jump boot
	copy(vbr[3:11], "EXFAT   ")
	// bytes 11–63: MustBeZero (already zero from make)
	putU64(vbr[64:], 0)                                    // PartitionOffset: unknown
	putU64(vbr[72:], uint64(layout.totalSectors))          // VolumeLength
	putU32(vbr[80:], uint32(layout.fatOffset))             // FATOffset
	putU32(vbr[84:], uint32(layout.fatSectors))            // FATLength
	putU32(vbr[88:], uint32(layout.clusterHeapOffset))     // ClusterHeapOffset
	putU32(vbr[92:], uint32(layout.clusterCount))          // ClusterCount
	putU32(vbr[96:], uint32(layout.rootDirCluster))        // FirstClusterOfRootDirectory
	putU32(vbr[100:], volSerial)                           // VolumeSerialNumber
	putU16(vbr[104:], 0x0100)                              // FileSystemRevision 1.00
	putU16(vbr[106:], 0)                                   // VolumeFlags (excluded from checksum)
	vbr[108] = layout.bpsShift                             // BytesPerSectorShift
	vbr[109] = layout.spcShift                             // SectorsPerClusterShift
	vbr[110] = 1                                           // NumberOfFATs
	vbr[111] = 0x80                                        // DriveSelect
	vbr[112] = 0                                           // PercentInUse (excluded from checksum)
	vbr[ss-2] = 0x55
	vbr[ss-1] = 0xAA

	// Sectors 1–8: Extended Boot Sectors (each ends with 0xAA550000).
	for i := 1; i <= 8; i++ {
		sec := bootRegion[i*ss : (i+1)*ss]
		sec[ss-4] = 0x00
		sec[ss-3] = 0x00
		sec[ss-2] = 0x55
		sec[ss-1] = 0xAA
	}
	// Sector 9:  OEM Parameters — zeros.
	// Sector 10: Reserved      — zeros.

	// Sector 11: Boot Checksum — 32-bit checksum of sectors 0–10 tiled to fill.
	csum := bootChecksum(bootRegion[:11*ss], ss)
	csumSec := bootRegion[11*ss : 12*ss]
	for i := 0; i+3 < ss; i += 4 {
		binary.LittleEndian.PutUint32(csumSec[i:], csum)
	}

	return bootRegion
}

// bootChecksum computes the exFAT boot checksum over sectors 0–10,
// excluding VolumeFlags (bytes 106–107) and PercentInUse (byte 112).
func bootChecksum(data []byte, ss int) uint32 {
	var sum uint32
	for i := 0; i < 11*ss; i++ {
		if i == 106 || i == 107 || i == 112 {
			continue
		}
		sum = (sum>>1 | sum<<31) + uint32(data[i])
	}
	return sum
}