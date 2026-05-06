package diskimg

import (
	"fmt"
	"io"
	"os"
)

const copyBufSize = 32 * 1024 // 32 KB – never holds more than this in RAM

// Detach unmounts all volumes, flushes all changes, and closes the image.
//
// If outPath is non-empty, changes are written to outPath and the original
// file is left untouched.  If outPath is empty, changes are flushed in place.
func (img *Image) Detach(outPath string) error {
	// Unmount all mounted volumes so their dirty blocks are flushed.
	for idx, v := range img.mounts {
		if err := v.Unmount(); err != nil {
			return fmt.Errorf("detach: unmount partition %d: %w", idx, err)
		}
		delete(img.mounts, idx)
	}

	if outPath == "" {
		// In-place: just close.
		return img.f.Close()
	}

	// Write to output file, region by region.
	out, err := os.Create(outPath)
	if err != nil {
		img.f.Close()
		return fmt.Errorf("detach: create %s: %w", outPath, err)
	}
	// Pre-allocate to source size.
	if err := out.Truncate(img.size); err != nil {
		out.Close()
		img.f.Close()
		return fmt.Errorf("detach: truncate: %w", err)
	}

	for _, region := range img.regions {
		if err := copyRange(img.f, out, region.Start, region.Start, region.Size()); err != nil {
			out.Close()
			img.f.Close()
			return fmt.Errorf("detach: copy region %v: %w", region.Kind, err)
		}
	}

	if err := out.Sync(); err != nil {
		out.Close()
		img.f.Close()
		return fmt.Errorf("detach: sync: %w", err)
	}
	out.Close()
	return img.f.Close()
}

// copyRange copies size bytes from src at srcOff to dst at dstOff using a
// fixed 32 KB buffer — never allocates more than that regardless of size.
func copyRange(src io.ReaderAt, dst io.WriterAt, srcOff, dstOff, size int64) error {
	buf := make([]byte, copyBufSize)
	for size > 0 {
		n := int64(len(buf))
		if n > size {
			n = size
		}
		rn, err := src.ReadAt(buf[:n], srcOff)
		if err != nil && err != io.EOF {
			return err
		}
		if rn == 0 {
			break
		}
		if _, err := dst.WriteAt(buf[:rn], dstOff); err != nil {
			return err
		}
		srcOff += int64(rn)
		dstOff += int64(rn)
		size -= int64(rn)
	}
	return nil
}