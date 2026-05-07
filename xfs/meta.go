package xfs

import (
	"fmt"
	"io/fs"
	"time"
)

func (v *Volume) Chmod(name string, mode fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return err
	}
	in.mode = (in.mode & uint16(modeFmt)) | uint16(mode&0x1FF)
	in.ctime = nowSec()
	return v.writeInode(ino, &in)
}

func (v *Volume) Chown(name string, uid, gid int) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return err
	}
	in.uid = uint32(uid)
	in.gid = uint32(gid)
	in.ctime = nowSec()
	return v.writeInode(ino, &in)
}

func (v *Volume) Chtimes(name string, atime, mtime time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	ino, err := v.lookupPathFollow(name, 0)
	if err != nil {
		return err
	}
	in, err := v.readInode(ino)
	if err != nil {
		return err
	}
	in.atime = atime.Unix()
	in.atimeNsec = uint32(atime.Nanosecond())
	in.mtime = mtime.Unix()
	in.mtimeNsec = uint32(mtime.Nanosecond())
	in.ctime = nowSec()
	return v.writeInode(ino, &in)
}

func openFileCheck(name string) error {
	if name == "" {
		return fmt.Errorf("xfs: empty path")
	}
	return nil
}