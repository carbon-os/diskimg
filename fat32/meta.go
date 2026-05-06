package fat32

import (
	"fmt"
	"io/fs"
	"time"
)

// Chmod maps Unix permission bits to FAT's read-only attribute.
// Only the owner-write bit (0200) is consulted; all other FAT attributes
// are preserved.
func (v *Volume) Chmod(name string, mode fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, err := v.lookupPath(name)
	if err != nil {
		return err
	}
	if mode&0200 == 0 {
		e.attr |= attrReadOnly
	} else {
		e.attr &^= attrReadOnly
	}
	return v.writeDirEntry(e)
}

// Chown is not supported on FAT filesystems; it always returns an error.
func (v *Volume) Chown(name string, uid, gid int) error {
	return fmt.Errorf("fat32: Chown not supported")
}

// Chtimes updates the last-modified timestamp of name. The access time is
// accepted but ignored (FAT stores only a date for last access, no time).
func (v *Volume) Chtimes(name string, atime, mtime time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, err := v.lookupPath(name)
	if err != nil {
		return err
	}
	e.wrtDate = fatDate(mtime)
	e.wrtTime = fatTime(mtime)
	return v.writeDirEntry(e)
}