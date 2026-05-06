package ext4

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
)

const rootInodeNum = 2 // ext4 root directory is always inode 2

// Extract walks the entire ext4 filesystem rooted at inode 2 and writes
// every file, directory, and symlink into tw as a streaming tar archive.
func Extract(r io.ReaderAt, sb *Superblock, tw *tar.Writer, verbose bool) error {
	// Write the root directory entry.
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "./",
		Mode:     0755,
	}); err != nil {
		return fmt.Errorf("ext4: tar root: %w", err)
	}
	return walkDir(r, sb, tw, rootInodeNum, "", verbose)
}

// walkDir recursively walks directory inodeNum, writing all entries to tw.
func walkDir(r io.ReaderAt, sb *Superblock, tw *tar.Writer, inodeNum uint32, prefix string, verbose bool) error {
	inode, err := ReadInode(r, sb, inodeNum)
	if err != nil {
		return fmt.Errorf("ext4: read dir inode %d: %w", inodeNum, err)
	}

	entries, err := ReadDir(r, sb, inode)
	if err != nil {
		return fmt.Errorf("ext4: readdir inode %d: %w", inodeNum, err)
	}

	for _, de := range entries {
		childInode, err := ReadInode(r, sb, de.InodeNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [warn] read inode %d (%s): %v\n", de.InodeNum, de.Name, err)
			continue
		}

		relPath := prefix + de.Name
		tarName := "./" + relPath

		switch {
		case childInode.IsDir():
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     tarName + "/",
				Mode:     int64(childInode.Perm()),
				ModTime:  childInode.ModTime(),
				Uid:      int(childInode.UID()),
				Gid:      int(childInode.GID()),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("ext4: tar dir %q: %w", tarName, err)
			}
			if verbose {
				fmt.Printf("  d  %s/\n", relPath)
			}
			if err := walkDir(r, sb, tw, de.InodeNum, relPath+"/", verbose); err != nil {
				return err
			}

		case childInode.IsSymlink():
			target, err := ReadSymlink(r, sb, childInode)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [warn] symlink %q: %v\n", relPath, err)
				continue
			}
			hdr := &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     tarName,
				Linkname: target,
				Mode:     int64(childInode.Perm()),
				ModTime:  childInode.ModTime(),
				Uid:      int(childInode.UID()),
				Gid:      int(childInode.GID()),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("ext4: tar symlink %q: %w", tarName, err)
			}
			if verbose {
				fmt.Printf("  l  %s -> %s\n", relPath, target)
			}

		case childInode.IsRegular():
			size := childInode.Size()
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     tarName,
				Size:     size,
				Mode:     int64(childInode.Perm()),
				ModTime:  childInode.ModTime(),
				Uid:      int(childInode.UID()),
				Gid:      int(childInode.GID()),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("ext4: tar file %q: %w", tarName, err)
			}
			if size > 0 {
				ir, err := NewInodeReader(r, sb, childInode)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  [warn] reader %q: %v\n", relPath, err)
					continue
				}
				n, err := io.Copy(tw, io.LimitReader(ir, size))
				if err != nil {
					fmt.Fprintf(os.Stderr, "  [warn] copy %q (%d/%d bytes): %v\n", relPath, n, size, err)
				} else if verbose {
					fmt.Printf("  f  %s (%d bytes)\n", relPath, n)
				}
			}

		default:
			if verbose {
				fmt.Printf("  -  %s [type 0x%04X — skipped]\n", relPath, childInode.Mode&ModeTypeMask)
			}
		}
	}
	return nil
}