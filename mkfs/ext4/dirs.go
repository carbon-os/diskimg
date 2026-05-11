package ext4

import "encoding/binary"

// buildRootDir returns one blockSize-byte block with the root directory entries:
//   . (inode 2)  .. (inode 2)  lost+found (inode 11)
func (fs *fsLayout) buildRootDir() []byte {
	buf := make([]byte, blockSize)
	off := 0

	// Each linear dir entry (ext4_dir_entry_2):
	//   inode    uint32
	//   rec_len  uint16   (must reach next 4-byte boundary; last fills to block end)
	//   name_len uint8
	//   file_type uint8
	//   name     [name_len]byte

	off += writeDirent(buf[off:], 2, ".", 2 /* FT_DIR */, 0)
	off += writeDirent(buf[off:], 2, "..", 2, 0)
	// lost+found entry fills to end of block
	remaining := blockSize - off
	writeDirentFull(buf[off:], inoLostFound, "lost+found", 2, remaining)

	return buf
}

// buildLostFound returns one blockSize-byte block for the lost+found directory.
//   . (inode 11)   .. (inode 2)
func (fs *fsLayout) buildLostFound() []byte {
	buf := make([]byte, blockSize)
	off := 0
	off += writeDirent(buf[off:], uint32(inoLostFound), ".", 2, 0)
	remaining := blockSize - off
	writeDirentFull(buf[off:], inoRoot, "..", 2, remaining)
	return buf
}

// writeDirent writes a dirent with a naturally-sized rec_len (rounded to 4).
// Returns the number of bytes written.
func writeDirent(dst []byte, ino uint32, name string, ftype uint8, _ int) int {
	nameLen := len(name)
	recLen := (8 + nameLen + 3) &^ 3 // round up to 4-byte boundary
	binary.LittleEndian.PutUint32(dst[0:], ino)
	binary.LittleEndian.PutUint16(dst[4:], uint16(recLen))
	dst[6] = uint8(nameLen)
	dst[7] = ftype
	copy(dst[8:], name)
	return recLen
}

// writeDirentFull writes a dirent with an explicit rec_len (e.g. to fill the block).
func writeDirentFull(dst []byte, ino uint32, name string, ftype uint8, recLen int) {
	binary.LittleEndian.PutUint32(dst[0:], ino)
	binary.LittleEndian.PutUint16(dst[4:], uint16(recLen))
	dst[6] = uint8(len(name))
	dst[7] = ftype
	copy(dst[8:], name)
}