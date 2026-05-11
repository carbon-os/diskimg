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
	attrDefCount     = 256 // Typical standard for basic NTFS implementations
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

	// 2. Write Backup Boot Sector (Last Sector)
	if _, err := rw.Seek((l.totalSectors-1)*int64(ss), io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(boot); err != nil {
		return err
	}

	// 3. Write MFT Records
	mftBytes := buildMFT(&l, time.Now(), opts.Label)
	if _, err := rw.Seek(l.mftLCN*l.clusterSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := rw.Write(mftBytes); err != nil {
		return err
	}

	// 4. Write MFTMirr (Mirror of first 4 records)
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

	// Note: LogFile, AttrDef, Bitmap, and other structures require zeroing out 
	// or specific population depending on how deep your mkfs implementation goes.

	return nil
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

	// LCN allocations (simplistic sequential allocation for mkfs purposes)
	l.bootClusters = roundUpI(8192, clusterSize) / clusterSize // Usually 16 sectors at 512b
	
	// Start allocating LCNs sequentially after the boot clusters
	cursor := l.bootClusters

	// $MFT
	l.mftLCN = cursor
	l.mftClusters = roundUpI(int64(numSysRecords*mftRecordSize), clusterSize) / clusterSize
	cursor += l.mftClusters

	// $MFTMirr (Ideally placed in the middle of the disk, but placing sequentially for simplicity)
	l.mftMirrLCN = totalClusters / 2
	l.mftMirrClusters = roundUpI(4*mftRecordSize, clusterSize) / clusterSize

	// $LogFile (Standard is ~2MB for small drives, ~64MB for large)
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
	l.bitmapBytes = roundUpI(totalClusters, 8) / 8 // 1 bit per cluster
	l.bitmapClusters = roundUpI(l.bitmapBytes, clusterSize) / clusterSize
	
	return l
}