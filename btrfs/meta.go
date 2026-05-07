package btrfs

import (
	"fmt"
	"io/fs"
	"time"
)

func (v *Volume) Chmod(name string, mode fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return err
	}
	in.mode = (in.mode & ifmt) | uint32(mode&0x1FF)
	in.ctime = nowSec()
	return v.writeInodeItem(objID, &in)
}

func (v *Volume) Chown(name string, uid, gid int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return err
	}
	in.uid = uint32(uid)
	in.gid = uint32(gid)
	in.ctime = nowSec()
	return v.writeInodeItem(objID, &in)
}

func (v *Volume) Chtimes(name string, atime, mtime time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	objID, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInodeItem(objID)
	if err != nil {
		return err
	}
	in.atime = atime.Unix()
	in.atimeNsec = uint32(atime.Nanosecond())
	in.mtime = mtime.Unix()
	in.mtimeNsec = uint32(mtime.Nanosecond())
	in.ctime = nowSec()
	return v.writeInodeItem(objID, &in)
}

// openFileCheck returns a non-nil error for obviously invalid paths.
func openFileCheck(name string) error {
	if name == "" {
		return fmt.Errorf("btrfs: empty path")
	}
	return nil
}