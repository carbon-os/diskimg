package diskimg

import (
	"fmt"
	"io"
	"os"
)

// Rebuild creates a new image at outPath that is identical to the source image
// except partition partNum (1-based) is replaced with the bytes from newPartData.
//
// All other slices — boot gap, GPT header, inter-partition gaps, GPT backup
// header — are copied verbatim from the source image, preserving:
//   - GRUB core.img embedded in the boot gap
//   - Exact partition offsets (bootloader block references remain valid)
//   - GPT backup structures
//
// newPartData must produce exactly the same number of bytes as the original
// partition, or the GPT/MBR partition table will be inconsistent.
func (img *Image) Rebuild(outPath string, partNum int, newPartData io.Reader) error {
	targetPart, err := img.partition(partNum)
	if err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("diskimg: rebuild create %q: %w", outPath, err)
	}
	defer out.Close()

	// Pre-allocate output file to the same size as the source image.
	if err := out.Truncate(img.size); err != nil {
		return fmt.Errorf("diskimg: rebuild truncate: %w", err)
	}

	for _, sl := range img.slices {
		if sl.Kind == SliceKindPartition && sl.PartitionIndex == partNum {
			// Write new filesystem data at the exact same byte offset.
			if _, err := out.Seek(sl.Start, io.SeekStart); err != nil {
				return fmt.Errorf("diskimg: rebuild seek to partition: %w", err)
			}
			n, err := io.Copy(out, io.LimitReader(newPartData, sl.Size()))
			if err != nil {
				return fmt.Errorf("diskimg: rebuild write partition: %w", err)
			}
			if n != sl.Size() {
				return fmt.Errorf("diskimg: rebuild: new partition data is %d bytes, need %d",
					n, sl.Size())
			}
			_ = targetPart
		} else {
			// Copy verbatim from source.
			if err := copyRange(out, img.f, sl.Start, sl.Size()); err != nil {
				return fmt.Errorf("diskimg: rebuild copy slice at 0x%X: %w", sl.Start, err)
			}
		}
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("diskimg: rebuild sync: %w", err)
	}
	fi, _ := out.Stat()
	fmt.Printf("Rebuilt: %s (%s)\n", outPath, humanBytes(fi.Size()))
	return nil
}

// copyRange copies size bytes starting at offset from src to dst.
func copyRange(dst, src *os.File, offset, size int64) error {
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if _, err := dst.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	_, err := io.CopyN(dst, src, size)
	return err
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}