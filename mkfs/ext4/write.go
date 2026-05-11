package ext4

import (
	"fmt"
	"io"
)

// write serialises the entire filesystem to w in one pass.
func (fs *fsLayout) write(w io.Writer) error {
	// The write cursor tracks how many bytes we've emitted so far,
	// so we can zero-pad to any block boundary.
	var cur uint64

	emit := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return err
		}
		cur += uint64(len(b))
		return nil
	}
	pad := func(target uint64) error {
		if cur > target {
			return fmt.Errorf("ext4: write cursor %d already past target %d", cur, target)
		}
		if cur == target {
			return nil
		}
		zeros := make([]byte, target-cur)
		return emit(zeros)
	}
	block := func(n uint64) error { return pad(n * blockSize) }

	// ── Group 0 ────────────────────────────────────────────────────────────
	// Block 0: first 1024 bytes unused (bootloader space), then superblock.
	if err := pad(1024); err != nil {
		return err
	}
	sb := fs.buildSuperblock(0)
	if err := emit(sb); err != nil {
		return err
	}
	// rest of block 0
	if err := block(1); err != nil {
		return err
	}

	// GDT in blocks 1..gdtBlocks
	gdt := fs.buildGDT()
	if err := emit(gdt); err != nil {
		return err
	}
	if err := block(1 + fs.gdtBlocks); err != nil {
		return err
	}

	// Reserved GDT expansion blocks (zeroed)
	if err := block(1 + fs.gdtBlocks + uint64(fs.opts.reservedGDT)); err != nil {
		return err
	}

	// Block bitmap
	g0 := &fs.groups[0]
	if err := block(g0.blockBitmap); err != nil {
		return err
	}
	if err := emit(fs.buildBlockBitmap(0)); err != nil {
		return err
	}

	// Inode bitmap
	if err := block(g0.inodeBitmap); err != nil {
		return err
	}
	if err := emit(fs.buildInodeBitmap(0)); err != nil {
		return err
	}

	// Inode table
	if err := block(g0.inodeTable); err != nil {
		return err
	}
	inodeTable0 := fs.buildInodeTable(0)
	if err := emit(inodeTable0); err != nil {
		return err
	}

	// Journal (inode 8)
	if err := block(fs.journalBlock); err != nil {
		return err
	}
	if err := fs.writeJournal(w, &cur); err != nil {
		return err
	}

	// Root directory (inode 2) data
	if err := block(fs.rootDirBlock); err != nil {
		return err
	}
	if err := emit(fs.buildRootDir()); err != nil {
		return err
	}

	// lost+found directory (inode 11) data
	if err := block(fs.lfDirBlock); err != nil {
		return err
	}
	if err := emit(fs.buildLostFound()); err != nil {
		return err
	}

	// ── Remaining block groups ─────────────────────────────────────────────
	for g := uint32(1); g < fs.numGroups; g++ {
		gl := &fs.groups[g]

		if gl.hasSB {
			// Backup superblock
			if err := block(gl.firstBlock); err != nil {
				return err
			}
			bsb := fs.buildSuperblock(g)
			if err := emit(bsb); err != nil {
				return err
			}
			if err := block(gl.firstBlock + 1); err != nil {
				return err
			}
			// Backup GDT
			if err := emit(gdt); err != nil {
				return err
			}
			if err := block(gl.firstBlock + 1 + fs.gdtBlocks); err != nil {
				return err
			}
			// Reserved GDT
			if err := block(gl.firstBlock + 1 + fs.gdtBlocks + uint64(fs.opts.reservedGDT)); err != nil {
				return err
			}
		}

		// Block bitmap
		if err := block(gl.blockBitmap); err != nil {
			return err
		}
		if err := emit(fs.buildBlockBitmap(g)); err != nil {
			return err
		}

		// Inode bitmap
		if err := block(gl.inodeBitmap); err != nil {
			return err
		}
		if err := emit(fs.buildInodeBitmap(g)); err != nil {
			return err
		}

		// Inode table (all zero — no pre-allocated inodes in non-zero groups)
		if err := block(gl.inodeTable); err != nil {
			return err
		}
		tableBytes := uint64(gl.inodeTableBlocks) * blockSize
		if err := emit(make([]byte, tableBytes)); err != nil {
			return err
		}
	}

	// Pad to full image size
	return pad(fs.totalBlocks * blockSize)
}