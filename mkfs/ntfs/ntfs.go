package ntfs

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const (
	mftRecordSize    = 1024
	numSysRecords    = 24
	attrDefCount     = 256
	attrDefEntrySize = 160
)

// Options exposes formatting configuration.
type Options struct {
	Label      string
	SectorSize int
}

// fsLayout holds the calculated LCNs and sizes for the NTFS structures.
type fsLayout struct {
	ss              int
	spc             int
	clusterSize     int64
	totalSectors    int64
	totalClusters   int64

	bootClusters    int64

	mftLCN          int64
	mftClusters     int64
	mftMirrLCN      int64
	mftMirrClusters int64

	logFileLCN      int64
	logFileClusters int64
	logFileBytes    int64

	attrDefLCN      int64
	attrDefClusters int64

	upcaseLCN       int64
	upcaseClusters  int64

	bitmapLCN       int64
	bitmapClusters  int64
	bitmapBytes     int64
}

// Format writes an NTFS filesystem to the given io.ReadWriteSeeker.
func Format(rw io.ReadWriteSeeker, sizeBytes int64, opts Options) error {
	ss := opts.SectorSize
	if ss == 0 {
		ss = 512
	}
	if !isPow2(ss) || ss < 512 || ss > 4096 {
		return fmt.Errorf("ntfs: invalid sector size %d", ss)
	}

	spc := sectorsPerCluster(sizeBytes, ss)
	l := computeLayout(sizeBytes, ss, spc)

	// 1. Write Boot Sector
	var serialBuf [8]byte
	rand.Read(serialBuf[:])
	serial := binary.LittleEndian.Uint64(serialBuf[:])

	boot := buildBootSector(&l, serial)
	if _, err := rw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(boot); err != nil {
		return err
	}

	// 2. Write Backup Boot Sector (last sector)
	if _, err := rw.Seek((l.totalSectors-1)*int64(ss), io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(boot); err != nil {
		return err
	}

	// 3. Write MFT records
	mftBytes := buildMFT(&l, time.Now(), opts.Label)
	if _, err := rw.Seek(l.mftLCN*l.clusterSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(mftBytes); err != nil {
		return err
	}

	// 4. Write MFTMirr (mirror of first 4 records)
	if _, err := rw.Seek(l.mftMirrLCN*l.clusterSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(mftBytes[:4*mftRecordSize]); err != nil {
		return err
	}

	// 5. Write Upcase Table
	upcase := buildUpcaseTable()
	if _, err := rw.Seek(l.upcaseLCN*l.clusterSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(upcase); err != nil {
		return err
	}

	// 6. Write $Bitmap — marks all system clusters as in-use so the driver
	//    never allocates over the boot sector, MFT, or other metadata.
	bitmap := buildBitmap(&l)
	if _, err := rw.Seek(l.bitmapLCN*l.clusterSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(bitmap); err != nil {
		return err
	}

	return nil
}

// buildBitmap returns the on-disk $Bitmap data with all system-reserved
// clusters marked as in-use (bit = 1). The returned slice is padded to a
// whole number of clusters so it can be written directly at bitmapLCN.
func buildBitmap(l *fsLayout) []byte {
	buf := make([]byte, l.bitmapClusters*l.clusterSize)

	markRange := func(start, count int64) {
		for i := start; i < start+count; i++ {
			buf[i/8] |= 1 << (uint(i) % 8)
		}
	}

	markRange(0,             l.bootClusters)    // LCN 0:          boot sector
	markRange(l.mftLCN,     l.mftClusters)      // $MFT
	markRange(l.mftMirrLCN, l.mftMirrClusters)  // $MFTMirr
	markRange(l.logFileLCN, l.logFileClusters)   // $LogFile
	markRange(l.attrDefLCN, l.attrDefClusters)   // $AttrDef
	markRange(l.upcaseLCN,  l.upcaseClusters)    // $UpCase
	markRange(l.bitmapLCN,  l.bitmapClusters)    // $Bitmap itself

	return buf
}

// computeLayout calculates the placements (LCNs) and sizes of all system files.
func computeLayout(sizeBytes int64, ss int, spc int) fsLayout {
	clusterSize := int64(ss * spc)
	totalSectors := sizeBytes / int64(ss)
	totalClusters := totalSectors / int64(spc)

	l := fsLayout{
		ss:            ss,
		spc:           spc,
		clusterSize:   clusterSize,
		totalSectors:  totalSectors,
		totalClusters: totalClusters,
	}

	l.bootClusters = roundUpI(8192, clusterSize) / clusterSize

	cursor := l.bootClusters

	// $MFT
	l.mftLCN = cursor
	l.mftClusters = roundUpI(int64(numSysRecords*mftRecordSize), clusterSize) / clusterSize
	cursor += l.mftClusters

	// $MFTMirr — placed at the middle of the disk
	l.mftMirrLCN = totalClusters / 2
	l.mftMirrClusters = roundUpI(4*mftRecordSize, clusterSize) / clusterSize

	// $LogFile
	l.logFileLCN = cursor
	l.logFileBytes = 2 * 1024 * 1024
	l.logFileClusters = roundUpI(l.logFileBytes, clusterSize) / clusterSize
	cursor += l.logFileClusters

	// $AttrDef
	l.attrDefLCN = cursor
	l.attrDefClusters = roundUpI(int64(attrDefCount*attrDefEntrySize), clusterSize) / clusterSize
	cursor += l.attrDefClusters

	// $UpCase
	l.upcaseLCN = cursor
	l.upcaseClusters = roundUpI(131072, clusterSize) / clusterSize
	cursor += l.upcaseClusters

	// $Bitmap
	l.bitmapLCN = cursor
	l.bitmapBytes = roundUpI(totalClusters, 8) / 8
	l.bitmapClusters = roundUpI(l.bitmapBytes, clusterSize) / clusterSize
	// Note: $Bitmap is not added to cursor since it is the last system
	// structure; the driver allocates user data after bitmapLCN+bitmapClusters.

	return l
}