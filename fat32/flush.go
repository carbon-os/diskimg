package fat32

import (
	"encoding/binary"
	"fmt"

	"github.com/carbon-os/diskimg/fs/fstype"
)

// Unmount flushes all dirty state to the image and releases in-memory
// resources. After Unmount the Volume must not be used again.
func (v *Volume) Unmount() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	err := v.flush()
	// Nil out the writer so any subsequent flush call (e.g. from Detach's
	// internal unmount loop after we've already flushed) is a guaranteed
	// no-op rather than a write to a closing or closed file descriptor.
	v.rw = nil
	return err
}

// flush writes the cached FAT table and all dirty data/directory sectors
// back through v.rw (the full-image WriterAt) using v.start as the
// partition base offset. It is safe to call more than once (second call
// is a no-op because Unmount nils v.rw).
func (v *Volume) flush() error {
	if v.rw == nil {
		return nil
	}
	if len(v.dirty) == 0 && len(v.fat1) == 0 {
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
	v.dirty = make(map[uint32][]byte)

	// ── FSInfo sector (sector 1) — update free cluster count ─────────────────
	// Per the FAT32 spec the FSInfo free_count and nxt_free fields should
	// reflect the current state so that mounting OSes don't need to scan
	// the entire FAT on mount.
	if v.b.fatType == fstype.FAT32 {
		if err := v.flushFSInfo(); err != nil {
			return fmt.Errorf("fat32: flush FSInfo: %w", err)
		}
	}

	return nil
}

// flushFSInfo rewrites the FSInfo sector with current free-cluster statistics.
func (v *Volume) flushFSInfo() error {
	var free uint32
	var nxtFree uint32 = 0xFFFFFFFF
	for n := uint32(2); n < v.b.cntClusters+2; n++ {
		if v.fatEntry(n) == fatFREE {
			free++
			if nxtFree == 0xFFFFFFFF {
				nxtFree = n
			}
		}
	}

	sec := make([]byte, v.b.bytesPerSec)
	binary.LittleEndian.PutUint32(sec[0:],   0x41615252) // lead signature
	binary.LittleEndian.PutUint32(sec[484:], 0x61417272) // structure signature
	binary.LittleEndian.PutUint32(sec[488:], free)        // free cluster count
	binary.LittleEndian.PutUint32(sec[492:], nxtFree)     // next free cluster hint
	binary.LittleEndian.PutUint32(sec[508:], 0xAA550000)  // trail signature

	// Primary FSInfo at sector 1.
	off := v.start + int64(v.b.bytesPerSec)
	if _, err := v.rw.WriteAt(sec, off); err != nil {
		return err
	}
	// Backup copy at sector 7 (mirrors the primary per spec).
	off = v.start + int64(7)*int64(v.b.bytesPerSec)
	_, err := v.rw.WriteAt(sec, off)
	return err
}