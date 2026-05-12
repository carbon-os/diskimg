package diskimg

import (
	"fmt"
	"io"

	"github.com/carbon-os/diskimg/btrfs"
	"github.com/carbon-os/diskimg/ext4"
	"github.com/carbon-os/diskimg/fat32"
	"github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/fs/fstype"
	"github.com/carbon-os/diskimg/ntfs"
	"github.com/carbon-os/diskimg/xfs"
)

// MountOptions controls optional behaviour of Mount.
// The zero value is valid and selects all defaults.
type MountOptions struct {
	// Subvol names a Btrfs subvolume to mount instead of the default FS tree.
	// Ignored for non-Btrfs partitions.
	Subvol string
}

// Mount mounts partition number index (1-based) and returns a Volume.
// An optional MountOptions may be supplied to select a Btrfs subvolume.
// The returned Volume must be Unmount()ed before Detach() is called.
//
//	img.Mount(4)                                      // default FS tree
//	img.Mount(4, diskimg.MountOptions{Subvol:"root"}) // Fedora root subvolume
func (img *Image) Mount(index int, opts ...MountOptions) (fs.Volume, error) {
	var opt MountOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	if opt.Subvol == "" {
		if v, ok := img.mounts[index]; ok {
			return v, nil
		}
	}

	region, err := img.findPartitionRegion(index)
	if err != nil {
		return nil, fmt.Errorf("mount partition %d: %w", index, err)
	}

	sr := io.NewSectionReader(img.f, region.Start, region.Size())
	ft := fstype.Detect(func(off int64, buf []byte) error {
		_, err := sr.ReadAt(buf, off)
		return err
	})

	var vol fs.Volume
	switch ft {
	case fstype.Btrfs:
		base, err := btrfs.Open(img.f, region.Start, region.Size())
		if err != nil {
			return nil, fmt.Errorf("mount partition %d: %w", index, err)
		}
		if opt.Subvol != "" {
			vol, err = base.OpenSubvol(opt.Subvol)
			if err != nil {
				return nil, fmt.Errorf("mount partition %d subvol %q: %w", index, opt.Subvol, err)
			}
		} else {
			vol = base
		}

	case fstype.Ext4:
		vol, err = ext4.Open(img.f, region.Start, region.Size())

	case fstype.XFS:
		vol, err = xfs.Open(img.f, region.Start, region.Size())

	case fstype.NTFS:
		vol, err = ntfs.Open(img.f, region.Start, region.Size())

	case fstype.FAT32, fstype.FAT16, fstype.FAT12:
		vol, err = fat32.Open(img.f, region.Start, region.Size(), ft)

	default:
		return nil, fmt.Errorf("mount partition %d: unsupported filesystem %q", index, ft)
	}
	if err != nil {
		return nil, fmt.Errorf("mount partition %d: %w", index, err)
	}

	if opt.Subvol == "" {
		img.mounts[index] = vol
	}
	return vol, nil
}