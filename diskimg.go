// Package diskimg provides pure-Go reading, extraction, and rebuilding
// of raw disk images (.img) without external tools or root access.
//
// Supports GPT and MBR partition tables, ext4 filesystems with full
// extent tree, htree directory, and indirect block map support.
//
// The core model is surgical slicing: raw bytes outside the target partition
// (bootloader gap, GRUB core.img, GPT backup header) are preserved verbatim.
// Only filesystem contents are extracted and rebuilt.
//
// Usage:
//
//	img, err := diskimg.Open("debian.img")
//	defer img.Close()
//
//	err = img.ExtractPartition(1, tarWriter, false)
//	err = img.Rebuild("new.img", 1, tarReader)
package diskimg

import (
	"archive/tar"
	"fmt"
	"io"
	"os"

	"github.com/carbon-os/diskimg/ext4"
	"github.com/carbon-os/diskimg/fat"
	"github.com/carbon-os/diskimg/fstype"
	"github.com/carbon-os/diskimg/partition"
	"github.com/carbon-os/diskimg/partition/gpt"
	"github.com/carbon-os/diskimg/partition/mbr"
)

// Image represents an open disk image file.
type Image struct {
	f          *os.File
	size       int64
	sectorSize int64
	tableType  partition.TableType
	partitions []*partition.Partition
	slices     []*Slice
}

// Open opens a disk image, parses its partition table, and builds the slice map.
func Open(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("diskimg: open %q: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("diskimg: stat: %w", err)
	}
	img := &Image{f: f, size: fi.Size(), sectorSize: 512}
	img.parsePartitionTable()
	img.buildSlices()
	return img, nil
}

// Close closes the underlying image file.
func (img *Image) Close() error { return img.f.Close() }

// Size returns the total image size in bytes.
func (img *Image) Size() int64 { return img.size }

// SectorSize returns the logical sector size (typically 512).
func (img *Image) SectorSize() int64 { return img.sectorSize }

// TableType returns "gpt", "mbr", or "raw".
func (img *Image) TableType() partition.TableType { return img.tableType }

// Partitions returns the detected partitions in index order.
func (img *Image) Partitions() []*partition.Partition { return img.partitions }

// Slices returns the ordered byte-range slices.
func (img *Image) Slices() []*Slice { return img.slices }

func (img *Image) parsePartitionTable() {
	if parts, err := gpt.Parse(img.f, img.size, img.sectorSize); err == nil {
		img.tableType = partition.TableTypeGPT
		img.partitions = parts
		return
	}
	if parts, err := mbr.Parse(img.f, img.sectorSize); err == nil {
		img.tableType = partition.TableTypeMBR
		img.partitions = parts
		return
	}
	img.tableType = partition.TableTypeRaw
	img.partitions = []*partition.Partition{
		{Index: 1, StartBytes: 0, SizeBytes: img.size},
	}
}

// DetectFilesystem returns the filesystem type for partition partNum (1-based).
func (img *Image) DetectFilesystem(partNum int) (fstype.Type, error) {
	p, err := img.partition(partNum)
	if err != nil {
		return fstype.Unknown, err
	}
	return fstype.Detect(img.f, p.StartBytes, p.SizeBytes)
}

// ExtractPartition streams the filesystem of partition partNum to tw.
func (img *Image) ExtractPartition(partNum int, tw *tar.Writer, verbose bool) error {
	p, err := img.partition(partNum)
	if err != nil {
		return err
	}

	ft, err := fstype.Detect(img.f, p.StartBytes, p.SizeBytes)
	if err != nil {
		return fmt.Errorf("diskimg: detect fs: %w", err)
	}

	sr := io.NewSectionReader(img.f, p.StartBytes, p.SizeBytes)

	switch ft {
	case fstype.Ext4:
		sb, err := ext4.ReadSuperblock(sr)
		if err != nil {
			return fmt.Errorf("diskimg: ext4 superblock: %w", err)
		}
		fmt.Printf("  ext4 volume: %q  block size: %d\n",
			strings.TrimRight(string(sb.VolumeName[:]), "\x00"),
			sb.BlockSize)
		return ext4.Extract(sr, sb, tw, verbose)

	case fstype.FAT32:
		fs, err := fat.Open(sr)
		if err != nil {
			return fmt.Errorf("diskimg: fat32 open: %w", err)
		}
		return fat.Extract(fs, tw, verbose)

	default:
		return fmt.Errorf("diskimg: unsupported filesystem %q in partition %d", ft, partNum)
	}
}

func (img *Image) partition(n int) (*partition.Partition, error) {
	if n < 1 || n > len(img.partitions) {
		return nil, fmt.Errorf("diskimg: partition %d out of range (1–%d)", n, len(img.partitions))
	}
	return img.partitions[n-1], nil
}