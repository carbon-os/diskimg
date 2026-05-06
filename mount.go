package diskimg

import (
	"fmt"
	"io"

	"github.com/carbon-os/diskimg/ext4"
	"github.com/carbon-os/diskimg/fat32"
	"github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/fs/fstype"
)

// Mount mounts partition number index (1-based) and returns a Volume.
// The returned Volume must be Unmount()ed before Detach() is called.
func (img *Image) Mount(index int) (fs.Volume, error) {
	if v, ok := img.mounts[index]; ok {
		return v, nil
	}

	region, err := img.findPartitionRegion(index)
	if err != nil {
		return nil, fmt.Errorf("mount partition %d: %w", index, err)
	}

	// Temporary SectionReader for filesystem detection only.
	sr := io.NewSectionReader(img.f, region.Start, region.Size())
	ft := fstype.Detect(func(off int64, buf []byte) error {
		_, err := sr.ReadAt(buf, off)
		return err
	})

	var vol fs.Volume
	switch ft {
	case fstype.Ext4:
		vol, err = ext4.Open(img.f, region.Start, region.Size())
	case fstype.FAT32, fstype.FAT16, fstype.FAT12:
		// Pass the full image file + partition bounds so fat32 can flush writes.
		vol, err = fat32.Open(img.f, region.Start, region.Size(), ft)
	default:
		return nil, fmt.Errorf("mount partition %d: unsupported filesystem %q", index, ft)
	}
	if err != nil {
		return nil, fmt.Errorf("mount partition %d: %w", index, err)
	}

	img.mounts[index] = vol
	return vol, nil
}