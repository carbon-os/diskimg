package ext4

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	volfs "github.com/carbon-os/diskimg/fs"
)

// ── file write ────────────────────────────────────────────────────────────────

// writeFileRange writes data to the file at byteOffset, extending as needed.
func (v *Volume) writeFileRange(num uint32, in *inode, byteOffset int64, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	bs := int64(v.sb.blockSize)
	total := 0
	off := byteOffset

	for len(data) > 0 {
		logBlk := uint32(off / bs)
		inBlockOff := off % bs

		physBlk, err := v.logicalToPhysical(in, logBlk)
		if err != nil {
			return total, err
		}

		var blockData []byte
		if physBlk == 0 {
			// Allocate a new block.
			physBlk, err = v.allocBlock(v.inodeBlockGroup(num))
			if err != nil {
				return total, fmt.Errorf("ext4: alloc block: %w", err)
			}
			blockData = make([]byte, bs)
			if err := v.appendExtentToInode(num, in, logBlk, physBlk, 1); err != nil {
				return total, err
			}
		} else {
			blockData, err = v.readBlock(physBlk)
			if err != nil {
				return total, err
			}
		}

		canWrite := bs - inBlockOff
		if int64(len(data)) < canWrite {
			canWrite = int64(len(data))
		}
		copy(blockData[inBlockOff:], data[:canWrite])
		v.writeBlock(physBlk, blockData)

		data = data[canWrite:]
		off += canWrite
		total += int(canWrite)
	}

	// Update inode size if we grew the file.
	newSize := byteOffset + int64(total)
	if newSize > inodeFileSize(in) {
		in.sizeLo = uint32(newSize)
		in.sizeHi = uint32(newSize >> 32)
		in.mtime = nowSec()
		in.ctime = nowSec()
		if err := v.writeInode(num, in); err != nil {
			return total, err
		}
	}
	return total, nil
}

// ── extent tree modification ──────────────────────────────────────────────────

// appendExtentToInode adds a new extent mapping logBlock→physBlock for count blocks.
// It handles both inline (≤4 extents in iBlock) and one-level indirect trees.
func (v *Volume) appendExtentToInode(num uint32, in *inode, logBlock uint32, physBlock uint64, count uint16) error {
	le := binary.LittleEndian
	raw := in.iBlock[:]
	hdr := parseExtentHeader(raw)

	if hdr.magic != extentMagic {
		// Initialise extent tree.
		le.PutUint16(raw[0:2], extentMagic)
		le.PutUint16(raw[2:4], 0)    // entries = 0
		le.PutUint16(raw[4:6], 4)    // max = 4 inline
		le.PutUint16(raw[6:8], 0)    // depth = 0
		le.PutUint32(raw[8:12], 0)   // generation
		hdr = parseExtentHeader(raw)
	}

	if hdr.depth == 0 && hdr.entries < 4 {
		// Inline leaf: append directly.
		off := 12 + int(hdr.entries)*12
		le.PutUint32(raw[off:off+4], logBlock)
		le.PutUint16(raw[off+4:off+6], count)
		le.PutUint16(raw[off+6:off+8], uint16(physBlock>>32))
		le.PutUint32(raw[off+8:off+12], uint32(physBlock))
		hdr.entries++
		le.PutUint16(raw[2:4], hdr.entries)
		copy(in.iBlock[:], raw)
		return v.writeInode(num, in)
	}

	// Convert to one-level indirect tree or extend existing one.
	return v.appendExtentIndirect(num, in, logBlock, physBlock, count)
}

// appendExtentIndirect promotes the inline extents to an indirect tree and
// appends the new extent into a leaf block.
func (v *Volume) appendExtentIndirect(num uint32, in *inode, logBlock uint32, physBlock uint64, count uint16) error {
	le := binary.LittleEndian
	raw := in.iBlock[:]
	hdr := parseExtentHeader(raw)

	if hdr.depth == 0 {
		// Promote: allocate an index block, move inline leaves there, point iBlock to it.
		leafBlk, err := v.allocBlock(v.inodeBlockGroup(num))
		if err != nil {
			return err
		}
		leafData := make([]byte, v.sb.blockSize)
		// Write header for the new leaf block.
		le.PutUint16(leafData[0:2], extentMagic)
		le.PutUint16(leafData[2:4], hdr.entries) // same entries
		le.PutUint16(leafData[4:6], uint16((int(v.sb.blockSize)-12)/12))
		le.PutUint16(leafData[6:8], 0) // depth 0
		le.PutUint32(leafData[8:12], 0)
		copy(leafData[12:], raw[12:12+int(hdr.entries)*12])
		v.writeBlock(leafBlk, leafData)

		// Rewrite iBlock as depth-1 index.
		newRaw := make([]byte, 60)
		le.PutUint16(newRaw[0:2], extentMagic)
		le.PutUint16(newRaw[2:4], 1)   // 1 index entry
		le.PutUint16(newRaw[4:6], 4)   // max 4 index entries inline
		le.PutUint16(newRaw[6:8], 1)   // depth 1
		le.PutUint32(newRaw[8:12], 0)
		// Index entry 0.
		le.PutUint32(newRaw[12:16], 0) // first file block covered = 0
		le.PutUint32(newRaw[16:20], uint32(leafBlk))
		le.PutUint16(newRaw[20:22], uint16(leafBlk>>32))
		copy(in.iBlock[:], newRaw)
		hdr = parseExtentHeader(newRaw)
		raw = in.iBlock[:]
	}

	// Now depth == 1.  Find the last index entry (it holds current leaf block),
	// and try to append to it; if full, allocate a new leaf block.
	idxCount := int(hdr.entries)
	lastIdxOff := 12 + (idxCount-1)*12
	lastLeafLo := le.Uint32(raw[lastIdxOff+4 : lastIdxOff+8])
	lastLeafHi := le.Uint16(raw[lastIdxOff+8 : lastIdxOff+10])
	lastLeafBlk := uint64(lastLeafLo) | uint64(lastLeafHi)<<32

	leafData, err := v.readBlock(lastLeafBlk)
	if err != nil {
		return err
	}
	leafHdr := parseExtentHeader(leafData)
	maxLeafEntries := (int(v.sb.blockSize) - 12) / 12

	if int(leafHdr.entries) < maxLeafEntries {
		// Append to existing leaf block.
		off := 12 + int(leafHdr.entries)*12
		le.PutUint32(leafData[off:off+4], logBlock)
		le.PutUint16(leafData[off+4:off+6], count)
		le.PutUint16(leafData[off+6:off+8], uint16(physBlock>>32))
		le.PutUint32(leafData[off+8:off+12], uint32(physBlock))
		leafHdr.entries++
		le.PutUint16(leafData[2:4], leafHdr.entries)
		v.writeBlock(lastLeafBlk, leafData)
		return v.writeInode(num, in)
	}

	// Current leaf block full: allocate a new one.
	newLeafBlk, err := v.allocBlock(v.inodeBlockGroup(num))
	if err != nil {
		return err
	}
	newLeafData := make([]byte, v.sb.blockSize)
	le.PutUint16(newLeafData[0:2], extentMagic)
	le.PutUint16(newLeafData[2:4], 1)
	le.PutUint16(newLeafData[4:6], uint16(maxLeafEntries))
	le.PutUint16(newLeafData[6:8], 0)
	le.PutUint32(newLeafData[8:12], 0)
	le.PutUint32(newLeafData[12:16], logBlock)
	le.PutUint16(newLeafData[16:18], count)
	le.PutUint16(newLeafData[18:20], uint16(physBlock>>32))
	le.PutUint32(newLeafData[20:24], uint32(physBlock))
	v.writeBlock(newLeafBlk, newLeafData)

	// Add new index entry to iBlock if space permits.
	if idxCount < 4 {
		off := 12 + idxCount*12
		le.PutUint32(raw[off:off+4], logBlock)
		le.PutUint32(raw[off+4:off+8], uint32(newLeafBlk))
		le.PutUint16(raw[off+8:off+10], uint16(newLeafBlk>>32))
		hdr.entries++
		le.PutUint16(raw[2:4], hdr.entries)
		copy(in.iBlock[:], raw)
	}
	// (Deeper tree levels not implemented; most files won't need them.)
	return v.writeInode(num, in)
}

// ── directory modification ────────────────────────────────────────────────────

// addDirEntry appends name→childIno into directory dirIno.
func (v *Volume) addDirEntry(dirIno uint32, name string, childIno uint32, fileType uint8) error {
	dirIn, err := v.readInode(dirIno)
	if err != nil {
		return err
	}

	nameBytes := []byte(name)
	nameLen := len(nameBytes)
	// Required record size (8-byte header + name, rounded to 4).
	needed := ((8 + nameLen + 3) / 4) * 4

	bs := int64(v.sb.blockSize)
	fileSize := inodeFileSize(&dirIn)
	le := binary.LittleEndian

	// Walk existing blocks looking for a gap.
	for off := int64(0); off < fileSize; off += bs {
		logBlk := uint32(off / bs)
		physBlk, err := v.logicalToPhysical(&dirIn, logBlk)
		if err != nil {
			return err
		}
		blockData, err := v.readBlock(physBlk)
		if err != nil {
			return err
		}
		pos := 0
		for pos < len(blockData) {
			ino := le.Uint32(blockData[pos : pos+4])
			recLen := int(le.Uint16(blockData[pos+4 : pos+6]))
			nameLen8 := int(blockData[pos+6])

			if recLen == 0 {
				break
			}
			// Calculate actual minimal size of this entry.
			minSize := ((8 + nameLen8 + 3) / 4) * 4
			if minSize < 8 {
				minSize = 8
			}
			slack := recLen - minSize
			if ino == 0 {
				slack = recLen // deleted entry: reuse full space
				minSize = 0
			}
			if slack >= needed {
				// Split: shrink this entry, write new one after.
				if ino != 0 {
					le.PutUint16(blockData[pos+4:pos+6], uint16(minSize))
					pos += minSize
					recLen = slack
				}
				le.PutUint32(blockData[pos:pos+4], childIno)
				le.PutUint16(blockData[pos+4:pos+6], uint16(recLen))
				blockData[pos+6] = byte(nameLen)
				blockData[pos+7] = fileType
				copy(blockData[pos+8:], nameBytes)
				v.writeBlock(physBlk, blockData)
				return nil
			}
			pos += recLen
		}
	}

	// No space in existing blocks: allocate a new directory block.
	newPhysBlk, err := v.allocBlock(v.inodeBlockGroup(dirIno))
	if err != nil {
		return err
	}
	newBlock := make([]byte, bs)
	le.PutUint32(newBlock[0:4], childIno)
	le.PutUint16(newBlock[4:6], uint16(bs)) // spans whole block
	newBlock[6] = byte(nameLen)
	newBlock[7] = fileType
	copy(newBlock[8:], nameBytes)
	v.writeBlock(newPhysBlk, newBlock)

	logBlk := uint32(fileSize / bs)
	if err := v.appendExtentToInode(dirIno, &dirIn, logBlk, newPhysBlk, 1); err != nil {
		return err
	}
	dirIn.sizeLo = uint32(fileSize + bs)
	dirIn.mtime = nowSec()
	dirIn.ctime = nowSec()
	return v.writeInode(dirIno, &dirIn)
}

// removeDirEntry removes the entry with the given name from directory dirIno.
func (v *Volume) removeDirEntry(dirIno uint32, name string) error {
	dirIn, err := v.readInode(dirIno)
	if err != nil {
		return err
	}
	bs := int64(v.sb.blockSize)
	fileSize := inodeFileSize(&dirIn)
	le := binary.LittleEndian

	for off := int64(0); off < fileSize; off += bs {
		logBlk := uint32(off / bs)
		physBlk, err := v.logicalToPhysical(&dirIn, logBlk)
		if err != nil {
			return err
		}
		blockData, err := v.readBlock(physBlk)
		if err != nil {
			return err
		}

		pos := 0
		var prevPos int = -1
		modified := false
		for pos < len(blockData) {
			ino := le.Uint32(blockData[pos : pos+4])
			recLen := int(le.Uint16(blockData[pos+4 : pos+6]))
			nameLen := int(blockData[pos+6])
			if recLen == 0 {
				break
			}
			entName := string(blockData[pos+8 : pos+8+nameLen])
			if ino != 0 && entName == name {
				// Mark as deleted: zero the inode field.
				le.PutUint32(blockData[pos:pos+4], 0)
				// Merge with previous entry if possible.
				if prevPos >= 0 {
					prevRecLen := int(le.Uint16(blockData[prevPos+4 : prevPos+6]))
					le.PutUint16(blockData[prevPos+4:prevPos+6], uint16(prevRecLen+recLen))
					le.PutUint32(blockData[pos:pos+4], 0) // zero inode
				}
				modified = true
				break
			}
			prevPos = pos
			pos += recLen
		}
		if modified {
			v.writeBlock(physBlk, blockData)
			dirIn.mtime = nowSec()
			dirIn.ctime = nowSec()
			return v.writeInode(dirIno, &dirIn)
		}
	}
	return fmt.Errorf("ext4: %q: not found in directory", name)
}

// ── Volume.WriteFile ──────────────────────────────────────────────────────────

func (v *Volume) WriteFile(name string, data []byte, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Try to find existing file.
	num, err := v.lookupPath(name)
	if err != nil {
		// File doesn't exist: create it.
		return v.createFile(name, data, perm)
	}
	// Truncate and overwrite.
	in, err := v.readInode(num)
	if err != nil {
		return err
	}
	in.sizeLo = 0
	in.sizeHi = 0
	in.mtime = nowSec()
	// TODO: free old blocks; for now just overwrite from offset 0.
	if err := v.writeInode(num, &in); err != nil {
		return err
	}
	if len(data) > 0 {
		_, err = v.writeFileRange(num, &in, 0, data)
	}
	return err
}

// createFile creates a new regular file at name with the given data and perm.
func (v *Volume) createFile(name string, data []byte, perm fs.FileMode) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return fmt.Errorf("ext4: create %q: parent dir: %w", name, err)
	}

	dirIn, err := v.readInode(dirIno)
	if err != nil {
		return err
	}
	grp := v.inodeBlockGroup(dirIno)
	// Prefer same group as parent directory.
	newIno, err := v.allocInode(grp, false)
	if err != nil {
		return err
	}

	now := nowSec()
	newIn := inode{
		mode:       uint16(modeReg | (perm & 0x1FF)),
		uid:        0,
		gid:        0,
		linksCount: 1,
		atime:      now,
		ctime:      now,
		mtime:      now,
		flags:      inodeFlagExtents,
	}
	// Initialise extent tree header in iBlock.
	le := binary.LittleEndian
	le.PutUint16(newIn.iBlock[0:2], extentMagic)
	le.PutUint16(newIn.iBlock[2:4], 0)
	le.PutUint16(newIn.iBlock[4:6], 4)
	le.PutUint16(newIn.iBlock[6:8], 0)
	le.PutUint32(newIn.iBlock[8:12], 0)
	if err := v.writeInode(newIno, &newIn); err != nil {
		return err
	}

	if len(data) > 0 {
		if _, err := v.writeFileRange(newIno, &newIn, 0, data); err != nil {
			return err
		}
	}

	// Add directory entry.
	if err := v.addDirEntry(dirIno, base, newIno, ftRegFile); err != nil {
		return err
	}
	dirIn.mtime = now
	dirIn.ctime = now
	return v.writeInode(dirIno, &dirIn)
}

// ── Volume.Create ─────────────────────────────────────────────────────────────

func (v *Volume) Create(name string) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := v.createFile(name, nil, 0644); err != nil {
		// File might already exist; truncate it.
		num, lerr := v.lookupPath(name)
		if lerr != nil {
			return nil, err
		}
		in, _ := v.readInode(num)
		in.sizeLo = 0
		in.sizeHi = 0
		_ = v.writeInode(num, &in)
	}
	num, err := v.lookupPath(name)
	if err != nil {
		return nil, err
	}
	in, err := v.readInode(num)
	if err != nil {
		return nil, err
	}
	f := &ext4File{v: v, num: num, in: in, writable: true}
	return volfs.NewFile(f), nil
}

// ── Volume.OpenFile ───────────────────────────────────────────────────────────

func (v *Volume) OpenFile(name string, flag int, perm fs.FileMode) (*volfs.File, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	create := flag&os.O_CREATE != 0
	trunc := flag&os.O_TRUNC != 0
	excl := flag&os.O_EXCL != 0
	appendMode := flag&os.O_APPEND != 0

	num, err := v.lookupPath(name)
	if err != nil {
		if !create {
			return nil, err
		}
		if e2 := v.createFile(name, nil, perm); e2 != nil {
			return nil, e2
		}
		num, _ = v.lookupPath(name)
	} else if excl {
		return nil, fmt.Errorf("ext4: %q already exists", name)
	}

	in, err := v.readInode(num)
	if err != nil {
		return nil, err
	}
	offset := int64(0)
	if trunc && writable {
		in.sizeLo = 0
		in.sizeHi = 0
		in.mtime = nowSec()
		_ = v.writeInode(num, &in)
	}
	if appendMode {
		offset = inodeFileSize(&in)
	}
	f := &ext4File{v: v, num: num, in: in, offset: offset, writable: writable, flags: flag}
	return volfs.NewFile(f), nil
}

// ── Volume.Mkdir / MkdirAll ───────────────────────────────────────────────────

func (v *Volume) Mkdir(name string, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mkdir(name, perm)
}

func (v *Volume) mkdir(name string, perm fs.FileMode) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return fmt.Errorf("ext4: mkdir %q: parent: %w", name, err)
	}

	grp := v.inodeBlockGroup(dirIno)
	newIno, err := v.allocInode(grp, true)
	if err != nil {
		return err
	}

	now := nowSec()
	le := binary.LittleEndian
	newIn := inode{
		mode:       uint16(modeDir | (perm & 0x1FF)),
		linksCount: 2, // . and parent link
		atime:      now,
		ctime:      now,
		mtime:      now,
		flags:      inodeFlagExtents,
	}
	le.PutUint16(newIn.iBlock[0:2], extentMagic)
	le.PutUint16(newIn.iBlock[2:4], 0)
	le.PutUint16(newIn.iBlock[4:6], 4)
	le.PutUint16(newIn.iBlock[6:8], 0)
	le.PutUint32(newIn.iBlock[8:12], 0)
	if err := v.writeInode(newIno, &newIn); err != nil {
		return err
	}

	// Add . and .. entries.
	if err := v.addDirEntry(newIno, ".", newIno, ftDir); err != nil {
		return err
	}
	if err := v.addDirEntry(newIno, "..", dirIno, ftDir); err != nil {
		return err
	}

	// Link into parent directory.
	if err := v.addDirEntry(dirIno, base, newIno, ftDir); err != nil {
		return err
	}

	// Increment parent link count for the ".." in the new dir.
	dirIn, err := v.readInode(dirIno)
	if err != nil {
		return err
	}
	dirIn.linksCount++
	dirIn.mtime = now
	dirIn.ctime = now
	return v.writeInode(dirIno, &dirIn)
}

func (v *Volume) MkdirAll(p string, perm fs.FileMode) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	p = path.Clean("/" + p)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := "/"
	for _, part := range parts {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		_, err := v.lookupPath(cur)
		if err != nil {
			if e2 := v.mkdir(cur, perm); e2 != nil {
				return e2
			}
		}
	}
	return nil
}

// ── Volume.Remove / RemoveAll ─────────────────────────────────────────────────

func (v *Volume) Remove(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.remove(name, false)
}

func (v *Volume) remove(name string, allowDir bool) error {
	dir, base := path.Split(path.Clean("/" + name))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return err
	}

	num, err := v.lookupPath(name)
	if err != nil {
		return err
	}
	in, err := v.readInode(num)
	if err != nil {
		return err
	}
	if in.mode&modeFmt == modeDir && !allowDir {
		return fmt.Errorf("ext4: %q is a directory", name)
	}

	if err := v.removeDirEntry(dirIno, base); err != nil {
		return err
	}
	in.linksCount--
	in.ctime = nowSec()
	if in.linksCount == 0 {
		in.dtime = nowSec()
		v.freeInode(num)
		// TODO: free all data blocks via extent tree walk.
	}
	return v.writeInode(num, &in)
}

func (v *Volume) RemoveAll(p string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPath(p)
	if err != nil {
		return nil // already gone
	}
	in, err := v.readInode(num)
	if err != nil {
		return err
	}
	if in.mode&modeFmt == modeDir {
		entries, err := v.readDir(&in)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.name == "." || e.name == ".." {
				continue
			}
			child := path.Join(p, e.name)
			if err := v.RemoveAll(child); err != nil {
				return err
			}
		}
	}
	return v.remove(p, true)
}

// ── Volume.Rename ─────────────────────────────────────────────────────────────

func (v *Volume) Rename(oldpath, newpath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	oldDir, oldBase := path.Split(path.Clean("/" + oldpath))
	newDir, newBase := path.Split(path.Clean("/" + newpath))

	oldDirIno, err := v.lookupPathFollow(oldDir, 0)
	if err != nil {
		return err
	}
	newDirIno, err := v.lookupPathFollow(newDir, 0)
	if err != nil {
		return err
	}
	targetIno, err := v.lookupPath(oldpath)
	if err != nil {
		return err
	}
	in, err := v.readInode(targetIno)
	if err != nil {
		return err
	}

	ft := ftRegFile
	if in.mode&modeFmt == modeDir {
		ft = ftDir
	} else if in.mode&modeFmt == modeLnk {
		ft = ftSymlink
	}

	// Remove destination if it exists.
	if destIno, err := v.lookupPath(newpath); err == nil {
		destIn, _ := v.readInode(destIno)
		if destIn.mode&modeFmt == modeDir {
			entries, _ := v.readDir(&destIn)
			if len(entries) > 2 {
				return fmt.Errorf("ext4: rename: destination directory not empty")
			}
		}
		_ = v.removeDirEntry(newDirIno, newBase)
	}

	// Add in new location, remove from old.
	if err := v.addDirEntry(newDirIno, newBase, targetIno, uint8(ft)); err != nil {
		return err
	}
	if err := v.removeDirEntry(oldDirIno, oldBase); err != nil {
		return err
	}

	// Update ".." if directory moved to different parent.
	if oldDirIno != newDirIno && in.mode&modeFmt == modeDir {
		// Update ".." entry in the moved directory.
		entries, _ := v.readDir(&in)
		for _, e := range entries {
			if e.name == ".." {
				_ = v.removeDirEntry(targetIno, "..")
				_ = v.addDirEntry(targetIno, "..", newDirIno, ftDir)
				break
			}
		}
	}
	in.ctime = nowSec()
	return v.writeInode(targetIno, &in)
}

// ── Volume.Symlink ────────────────────────────────────────────────────────────

func (v *Volume) Symlink(oldname, newname string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	dir, base := path.Split(path.Clean("/" + newname))
	dirIno, err := v.lookupPathFollow(dir, 0)
	if err != nil {
		return err
	}

	grp := v.inodeBlockGroup(dirIno)
	newIno, err := v.allocInode(grp, false)
	if err != nil {
		return err
	}

	now := nowSec()
	le := binary.LittleEndian
	newIn := inode{
		mode:       uint16(modeLnk | 0777),
		linksCount: 1,
		atime:      now,
		ctime:      now,
		mtime:      now,
	}
	target := []byte(oldname)
	if len(target) < 60 {
		// Fast symlink: store in i_block directly.
		copy(newIn.iBlock[:], target)
		newIn.sizeLo = uint32(len(target))
	} else {
		// Long symlink: store in data block.
		newIn.flags = inodeFlagExtents
		le.PutUint16(newIn.iBlock[0:2], extentMagic)
		le.PutUint16(newIn.iBlock[2:4], 0)
		le.PutUint16(newIn.iBlock[4:6], 4)
		le.PutUint16(newIn.iBlock[6:8], 0)
		le.PutUint32(newIn.iBlock[8:12], 0)
		if err := v.writeInode(newIno, &newIn); err != nil {
			return err
		}
		if _, err := v.writeFileRange(newIno, &newIn, 0, target); err != nil {
			return err
		}
		newIn.sizeLo = uint32(len(target))
	}
	if err := v.writeInode(newIno, &newIn); err != nil {
		return err
	}
	return v.addDirEntry(dirIno, base, newIno, ftSymlink)
}

// ── Volume.Link ───────────────────────────────────────────────────────────────

func (v *Volume) Link(oldname, newname string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	num, err := v.lookupPath(oldname)
	if err != nil {
		return err
	}
	in, err := v.readInode(num)
	if err != nil {
		return err
	}
	if in.mode&modeFmt == modeDir {
		return fmt.Errorf("ext4: link: cannot hard-link directories")
	}

	newDir, newBase := path.Split(path.Clean("/" + newname))
	newDirIno, err := v.lookupPathFollow(newDir, 0)
	if err != nil {
		return err
	}

	ft := ftRegFile
	if in.mode&modeFmt == modeLnk {
		ft = ftSymlink
	}

	if err := v.addDirEntry(newDirIno, newBase, num, uint8(ft)); err != nil {
		return err
	}
	in.linksCount++
	in.ctime = nowSec()
	return v.writeInode(num, &in)
}