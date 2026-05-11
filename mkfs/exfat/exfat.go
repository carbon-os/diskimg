// Package exfat formats exFAT filesystems onto raw partition streams.
package exfat

import (
	"fmt"
	"io"
)

// Options configures exFAT formatting.
type Options struct {
	// Label is the volume label; up to 11 UTF-16LE code units.
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

// fsLayout holds the computed partition layout passed between the formatting steps.
type fsLayout struct {
	totalSectors      int64
	fatOffset         int64
	fatSectors        int64
	clusterHeapOffset int64
	clusterCount      int64
	clusterBytes      int64
	bitmapClusters    int64
	bitmapBytes       int64
	upcaseFirstCluster int64
	upcaseClusters    int64
	rootDirCluster    int64
	bpsShift          uint8
	spcShift          uint8
}

// Format writes a complete exFAT filesystem onto rw.
// rw must be positioned at offset 0 (first byte of the partition).
func Format(rw io.ReadWriteSeeker, sizeBytes int64, opts Options) error {
	ss := opts.sectorSize()
	if !isPow2(ss) || ss < 512 || ss > 4096 {
		return fmt.Errorf("exfat: unsupported sector size %d", ss)
	}

	l, err := computeLayout(sizeBytes, ss)
	if err != nil {
		return err
	}

	if _, err := rw.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// Primary and backup boot regions (identical content, written twice).
	bootRegion := buildBootRegion(ss, l, opts)
	for range [2]struct{}{} {
		if _, err := rw.Write(bootRegion); err != nil {
			return fmt.Errorf("exfat: write boot region: %w", err)
		}
	}

	if err := writeFAT(rw, ss, l); err != nil {
		return err
	}

	// Alignment gap between FAT end and cluster heap start.
	fatEnd := l.fatOffset + l.fatSectors
	if l.clusterHeapOffset > fatEnd {
		gap := make([]byte, (l.clusterHeapOffset-fatEnd)*int64(ss))
		if _, err := rw.Write(gap); err != nil {
			return fmt.Errorf("exfat: write alignment gap: %w", err)
		}
	}

	// Cluster 2: Allocation Bitmap.
	bitmapBuf := make([]byte, l.bitmapClusters*l.clusterBytes)
	for c := int64(2); c <= l.rootDirCluster; c++ {
		bit := c - 2
		bitmapBuf[bit/8] |= 1 << (bit % 8)
	}
	if _, err := rw.Write(bitmapBuf); err != nil {
		return fmt.Errorf("exfat: write allocation bitmap: %w", err)
	}

	// Up-case table.
	upcaseBuf := buildUpcaseTable()
	csum := upcaseChecksum(upcaseBuf)
	upcasePadded := make([]byte, l.upcaseClusters*l.clusterBytes)
	copy(upcasePadded, upcaseBuf)
	if _, err := rw.Write(upcasePadded); err != nil {
		return fmt.Errorf("exfat: write upcase table: %w", err)
	}

	// Root directory cluster.
	rootDir := make([]byte, l.clusterBytes)
	pos := 0
	if opts.Label != "" {
		writeVolumeLabel(rootDir[pos:], opts.Label)
		pos += 32
	}
	writeAllocBitmapEntry(rootDir[pos:], 2, uint64(l.bitmapBytes))
	pos += 32
	writeUpcaseEntry(rootDir[pos:], csum, uint32(l.upcaseFirstCluster), UpcaseTableBytes)
	if _, err := rw.Write(rootDir); err != nil {
		return fmt.Errorf("exfat: write root directory: %w", err)
	}

	return nil
}

// computeLayout derives all cluster/sector offsets for the given volume.
func computeLayout(sizeBytes int64, ss int) (*fsLayout, error) {
	totalSectors := sizeBytes / int64(ss)
	if totalSectors < 2048 {
		return nil, fmt.Errorf("exfat: volume too small (need ≥ 2 048 sectors)")
	}

	shift := spcShift(sizeBytes, ss)
	spc := int64(1) << shift
	clusterBytes := spc * int64(ss)

	const bootRegionSectors = 24
	fatOffset := int64(bootRegionSectors)

	maxClusters := totalSectors / spc
	entriesPerSector := int64(ss) / 4
	fatSectors := roundUpI((maxClusters+2+entriesPerSector-1)/entriesPerSector, spc)

	clusterHeapOffset := roundUpI(fatOffset+fatSectors, spc)
	clusterCount := (totalSectors - clusterHeapOffset) / spc
	if clusterCount < 3 {
		return nil, fmt.Errorf("exfat: volume too small for metadata")
	}

	bitmapBytes := (clusterCount + 7) / 8
	bitmapClusters := (bitmapBytes + clusterBytes - 1) / clusterBytes
	if bitmapClusters < 1 {
		bitmapClusters = 1
	}

	upcaseClusters := int64((UpcaseTableBytes + clusterBytes - 1) / clusterBytes)
	upcaseFirstCluster := int64(2) + bitmapClusters
	rootDirCluster := upcaseFirstCluster + upcaseClusters

	if rootDirCluster+1 > clusterCount+2 {
		return nil, fmt.Errorf("exfat: not enough clusters for filesystem metadata")
	}

	return &fsLayout{
		totalSectors:       totalSectors,
		fatOffset:          fatOffset,
		fatSectors:         fatSectors,
		clusterHeapOffset:  clusterHeapOffset,
		clusterCount:       clusterCount,
		clusterBytes:       clusterBytes,
		bitmapClusters:     bitmapClusters,
		bitmapBytes:        bitmapBytes,
		upcaseFirstCluster: upcaseFirstCluster,
		upcaseClusters:     upcaseClusters,
		rootDirCluster:     rootDirCluster,
		bpsShift:           floorLog2(ss),
		spcShift:           shift,
	}, nil
}