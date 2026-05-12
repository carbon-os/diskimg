package ntfs

import (
	"encoding/binary"
	"fmt"

	"github.com/carbon-os/diskimg/fs"
)

// Unmount flushes all dirty state and marks the volume clean.
//
// The cluster bitmap ($Bitmap $DATA) and the MFT record bitmap ($MFT $BITMAP)
// are written back to their on-disk locations.  Any MFT records that were
// modified are already written through putRecord; this method handles the
// metadata bitmaps and the journal clean-up.
func (v *ntfsVolume) Unmount() error {
	v.mu.Lock()
	dirty := v.dirty
	v.mu.Unlock()
	if !dirty {
		return nil
	}

	if err := v.flushBitmap(); err != nil {
		return fmt.Errorf("ntfs.Unmount: flush $Bitmap: %w", err)
	}
	if err := v.flushMFTBitmap(); err != nil {
		return fmt.Errorf("ntfs.Unmount: flush MFT bitmap: %w", err)
	}
	if err := v.markVolumeClean(); err != nil {
		return fmt.Errorf("ntfs.Unmount: mark clean: %w", err)
	}

	v.mu.Lock()
	v.dirty = false
	v.mu.Unlock()
	return nil
}

// flushBitmap writes the in-memory cluster allocation bitmap back to $Bitmap.
func (v *ntfsVolume) flushBitmap() error {
	v.mu.Lock()
	bm := make([]byte, len(v.bitmap))
	copy(bm, v.bitmap)
	v.mu.Unlock()

	rec, err := v.getRecord(recBitmap)
	if err != nil {
		return err
	}
	attr := findAttr(rec, attrDATA, "")
	if attr == nil {
		return fmt.Errorf("$Bitmap has no $DATA attribute")
	}

	if attr[8] == 0 {
		// Resident: the bitmap fits inside the MFT record.  Update in place.
		rec = cloneRecord(rec)
		valOff := findAttrOffset(rec, attrDATA, "")
		if valOff >= 0 {
			av := valOff + int(binary.LittleEndian.Uint16(rec[valOff+0x14:]))
			copy(rec[av:], bm)
			return v.putRecord(recBitmap, rec)
		}
	}
	// Non-resident: write directly to the cluster runs.
	rlOff := int(binary.LittleEndian.Uint16(attr[0x20:]))
	if rlOff >= len(attr) {
		return fmt.Errorf("$Bitmap runlist offset out of bounds")
	}
	runs, err := decodeRunlist(attr[rlOff:])
	if err != nil {
		return err
	}
	return v.writeRunsData(runs, bm)
}

// flushMFTBitmap writes the MFT record allocation bitmap back to $MFT's $BITMAP.
func (v *ntfsVolume) flushMFTBitmap() error {
	if len(v.mftBitmap) == 0 {
		return nil
	}
	v.mu.Lock()
	bm := make([]byte, len(v.mftBitmap))
	copy(bm, v.mftBitmap)
	v.mu.Unlock()

	rec, err := v.getRecord(recMFT)
	if err != nil {
		return err
	}
	attr := findAttr(rec, attrBITMAP, "")
	if attr == nil {
		return nil // no MFT bitmap attribute to flush
	}
	if attr[8] == 0 {
		rec = cloneRecord(rec)
		off := findAttrOffset(rec, attrBITMAP, "")
		if off >= 0 {
			av := off + int(binary.LittleEndian.Uint16(rec[off+0x14:]))
			copy(rec[av:], bm)
			return v.putRecord(recMFT, rec)
		}
		return nil
	}
	rlOff := int(binary.LittleEndian.Uint16(attr[0x20:]))
	if rlOff >= len(attr) {
		return nil
	}
	runs, err := decodeRunlist(attr[rlOff:])
	if err != nil {
		return err
	}
	return v.writeRunsData(runs, bm)
}

// writeRunsData writes data sequentially across the cluster runs.
func (v *ntfsVolume) writeRunsData(runs []run, data []byte) error {
	var written int64
	for _, r := range runs {
		for c := int64(0); c < r.length; c++ {
			start := written
			end := written + v.clusterSize
			if start >= int64(len(data)) {
				return nil
			}
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			chunk := make([]byte, v.clusterSize)
			copy(chunk, data[start:end])
			off := (r.lcn + c) * v.clusterSize
			if _, err := v.partWrite(chunk, off); err != nil {
				return err
			}
			written += v.clusterSize
		}
	}
	return nil
}

// markVolumeClean clears the "volume dirty" flag in $Volume's
// $VOLUME_INFORMATION attribute so that Windows will not run chkdsk on next
// mount.  This is a best-effort operation; failure is non-fatal.
func (v *ntfsVolume) markVolumeClean() error {
	rec, err := v.getRecord(recVolume)
	if err != nil {
		return nil // non-fatal
	}
	attr := findAttr(rec, attrVOLUME_INFORMATION, "")
	if attr == nil || attr[8] != 0 {
		return nil
	}
	valOff := findAttrOffset(rec, attrVOLUME_INFORMATION, "")
	if valOff < 0 {
		return nil
	}
	av := valOff + int(binary.LittleEndian.Uint16(rec[valOff+0x14:]))
	// $VOLUME_INFORMATION value: [0-7] major/minor version, [8] flags.
	// Volume dirty flag is bit 0 of the flags word at offset 8.
	if av+10 > len(rec) {
		return nil
	}
	rec = cloneRecord(rec)
	rec[av+8] &^= 0x01 // clear dirty bit
	return v.putRecord(recVolume, rec)
}

// StatFS implements fs.Volume (forwarded here since flush.go has the full picture).
// The implementation lives in meta.go; this file just ensures the interface is
// complete without needing a separate forwarding stub.
var _ fs.Volume = (*ntfsVolume)(nil)