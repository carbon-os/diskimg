package diskimg

import (
	"fmt"
	"io"
	"os"

	"github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/partition"
	"github.com/carbon-os/diskimg/partition/gpt"
	"github.com/carbon-os/diskimg/partition/mbr"
)

// Image represents an attached disk image file.  All reads and writes go
// through the underlying file handle; nothing is copied into memory.
type Image struct {
	f          *os.File
	size       int64
	isGPT      bool
	partitions []*partition.Partition
	regions    []*Region
	mounts     map[int]fs.Volume // key = 1-based partition index
}

// Attach opens the named disk image and parses its partition table.
// The file is opened read-write; call Detach to close it.
func Attach(path string) (*Image, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("attach %s: %w", path, err)
	}

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("attach %s: seek: %w", path, err)
	}

	img := &Image{
		f:      f,
		size:   size,
		mounts: make(map[int]fs.Volume),
	}

	// Try GPT first (it has a protective MBR).
	parts, err := gpt.Parse(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("attach %s: gpt parse: %w", path, err)
	}
	if parts != nil {
		img.isGPT = true
		img.partitions = parts
	} else {
		// Fall back to MBR.
		parts, err = mbr.Parse(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("attach %s: mbr parse: %w", path, err)
		}
		img.partitions = parts
	}

	img.regions = buildRegions(img.partitions, size, img.isGPT)
	return img, nil
}

// Partitions returns the parsed partition list.
func (img *Image) Partitions() []*partition.Partition {
	return img.partitions
}

// Regions returns the ordered region map.
func (img *Image) Regions() []*Region {
	return img.regions
}

// findPartitionRegion returns the region for the given 1-based partition index.
func (img *Image) findPartitionRegion(index int) (*Region, error) {
	for _, r := range img.regions {
		if r.Kind == RegionPartition && r.PartitionIndex == index {
			return r, nil
		}
	}
	return nil, fmt.Errorf("partition %d not found", index)
}