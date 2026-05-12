package fat32

import "fmt"

// Unmount flushes all dirty state to the image and releases in-memory
// resources. After Unmount the Volume must not be used again.
func (v *Volume) Unmount() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.flush()
}

// flush writes the cached FAT table and all dirty data/directory sectors
// back through v.rw (the full-image WriterAt) using v.start as the
// partition base offset. It is safe to call more than once.
func (v *Volume) flush() error {
	if v.rw == nil || (len(v.dirty) == 0 && len(v.fat1) == 0) {
		return nil
	}

	secSize := int64(v.b.bytesPerSec)

	// ── FAT tables ────────────────────────────────────────────────────────────
	// Write all copies (typically 2) from the single in-memory fat1 slice.
	fatStart := int64(v.b.rsvdSecCnt) * secSize
	fatLen   := int64(v.b.fatSz) * secSize
	for i := int64(0); i < int64(v.b.numFATs); i++ {
		off := v.start + fatStart + i*fatLen
		if _, err := v.rw.WriteAt(v.fat1, off); err != nil {
			return fmt.Errorf("fat32: flush FAT%d: %w", i+1, err)
		}
	}

	// ── dirty data / directory sectors ────────────────────────────────────────
	for sec, data := range v.dirty {
		off := v.start + int64(sec)*secSize
		if _, err := v.rw.WriteAt(data, off); err != nil {
			return fmt.Errorf("fat32: flush sector %d: %w", sec, err)
		}
	}
	// Reset dirty map so a second call (or Unmount after Detach) is a no-op.
	v.dirty = make(map[uint32][]byte)
	return nil
}