package exfat

import (
	"fmt"
	"io"
)

// writeFAT writes the exFAT FAT, setting chains for the bitmap, up-case table,
// and root directory clusters; all other entries are left free (zero).
func writeFAT(w io.Writer, ss int, l *fsLayout) error {
	entriesPerSector := int64(ss) / 4
	for s := int64(0); s < l.fatSectors; s++ {
		sec := make([]byte, ss)
		for e := int64(0); e < entriesPerSector; e++ {
			cluster := s*entriesPerSector + e
			val := clusterFATValue(cluster, l)
			putU32(sec[e*4:], val)
		}
		if _, err := w.Write(sec); err != nil {
			return fmt.Errorf("exfat: write FAT sector %d: %w", s, err)
		}
	}
	return nil
}

// clusterFATValue returns the FAT entry value for the given cluster number.
func clusterFATValue(cluster int64, l *fsLayout) uint32 {
	switch {
	case cluster == 0:
		return 0xFFFFFFF8
	case cluster == 1:
		return 0xFFFFFFFF

	// Allocation bitmap chain (clusters 2 … 2+bitmapClusters-1).
	case cluster >= 2 && cluster < 2+l.bitmapClusters:
		if cluster == 2+l.bitmapClusters-1 {
			return 0xFFFFFFFF
		}
		return uint32(cluster + 1)

	// Up-case table chain.
	case cluster >= l.upcaseFirstCluster && cluster < l.upcaseFirstCluster+l.upcaseClusters:
		if cluster == l.upcaseFirstCluster+l.upcaseClusters-1 {
			return 0xFFFFFFFF
		}
		return uint32(cluster + 1)

	// Root directory (single cluster).
	case cluster == l.rootDirCluster:
		return 0xFFFFFFFF
	}
	return 0
}