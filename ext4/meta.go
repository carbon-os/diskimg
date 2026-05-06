package ext4

import (
	"fmt"
	"io/fs"
	"time"
)

func (v *Volume) Chmod(name string, mode fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInode(num)
	if err != nil {
		return err
	}
	// Preserve file type bits, replace permission bits.
	in.mode = (in.mode & uint16(modeFmt)) | uint16(mode&0x1FF)
	in.ctime = nowSec()
	return v.writeInode(num, &in)
}

func (v *Volume) Chown(name string, uid, gid int) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInode(num)
	if err != nil {
		return err
	}
	in.uid = uint16(uid & 0xFFFF)
	in.uidHi = uint16(uid >> 16)
	in.gid = uint16(gid & 0xFFFF)
	in.gidHi = uint16(gid >> 16)
	in.ctime = nowSec()
	return v.writeInode(num, &in)
}

func (v *Volume) Chtimes(name string, atime, mtime time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInode(num)
	if err != nil {
		return err
	}
	in.atime = timeToExt4(atime)
	in.mtime = timeToExt4(mtime)
	in.ctime = nowSec()
	return v.writeInode(num, &in)
}

// ── Readlink / Stat wrappers already in read.go ───────────────────────────────

// StatFS is in open.go.

// openFileCheck returns an error if name is a special reserved path.
func openFileCheck(name string) error {
	if name == "" {
		return fmt.Errorf("ext4: empty path")
	}
	return nil
}