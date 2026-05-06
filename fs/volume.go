// Package fs defines the Volume interface and supporting types shared by
// all filesystem drivers.
package fs

import (
	"io/fs"
	"time"

	"github.com/carbon-os/diskimg/fs/fstype"
)

// Volume is the unified API for reading and writing a mounted partition.
// Method signatures mirror os.* exactly so callers already know them.
type Volume interface {
	// ── read ────────────────────────────────────────────────────────
	ReadFile(name string) ([]byte, error)
	Open(name string) (fs.File, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Stat(name string) (fs.FileInfo, error)
	Lstat(name string) (fs.FileInfo, error)
	Readlink(name string) (string, error)

	// ── write ───────────────────────────────────────────────────────
	WriteFile(name string, data []byte, perm fs.FileMode) error
	Create(name string) (*File, error)
	OpenFile(name string, flag int, perm fs.FileMode) (*File, error)
	Mkdir(name string, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Remove(name string) error
	RemoveAll(path string) error
	Rename(oldpath, newpath string) error
	Symlink(oldname, newname string) error
	Link(oldname, newname string) error

	// ── metadata ────────────────────────────────────────────────────
	Chmod(name string, mode fs.FileMode) error
	Chown(name string, uid, gid int) error
	Chtimes(name string, atime, mtime time.Time) error

	// ── volume info ─────────────────────────────────────────────────
	StatFS() (VolumeInfo, error)
	Type() fstype.Type

	// ── lifecycle ───────────────────────────────────────────────────
	Unmount() error
}

// VolumeInfo mirrors syscall.Statfs_t in a cross-platform way.
type VolumeInfo struct {
	TotalBytes int64
	FreeBytes  int64
	UsedBytes  int64
	BlockSize  int64
	Inodes     int64
	InodesFree int64
}

// fileBackend is implemented by each filesystem driver.
type fileBackend interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
	Close() error
	Stat() (fs.FileInfo, error)
	ReadDir(n int) ([]fs.DirEntry, error)
}

// File is a streaming handle returned by Create / OpenFile.
// It implements io.Reader, io.Writer, io.Seeker, io.Closer, and fs.File.
type File struct {
	b fileBackend
}

// NewFile wraps a driver-provided backend.  Called only from drivers.
func NewFile(b fileBackend) *File { return &File{b: b} }

func (f *File) Read(p []byte) (int, error)                       { return f.b.Read(p) }
func (f *File) Write(p []byte) (int, error)                      { return f.b.Write(p) }
func (f *File) Seek(offset int64, whence int) (int64, error)     { return f.b.Seek(offset, whence) }
func (f *File) Close() error                                     { return f.b.Close() }
func (f *File) Stat() (fs.FileInfo, error)                       { return f.b.Stat() }
func (f *File) ReadDir(n int) ([]fs.DirEntry, error)             { return f.b.ReadDir(n) }