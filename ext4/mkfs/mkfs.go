// Package mkfs builds a minimal single-block-group ext4 filesystem from a tar
// archive without any external tools.
//
// Constraints (all removable incrementally):
//   - Single block group → max 128 MiB partition
//   - 4096-byte blocks, 128-byte inodes
//   - Linear directories (no htree)
//   - Up to four inline extents per inode
//   - No journal, no metadata checksums
package mkfs

import (
	"archive/tar"
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

const (
	rootInodeNum     = uint32(2)
	inodeFlagExtents = uint32(0x00080000)
	extentMagic      = uint16(0xF30A)
	maxInlineExtents = 4
)

// ── public API ────────────────────────────────────────────────────────────────

// Build reads tr and returns a raw ext4 image of exactly partSize bytes.
func Build(tr *tar.Reader, partSize int64) (io.Reader, error) {
	raw, err := readAll(tr)
	if err != nil {
		return nil, fmt.Errorf("mkfs: read tar: %w", err)
	}

	root := buildTree(raw)

	l, err := Calculate(partSize, countNodes(root))
	if err != nil {
		return nil, err
	}

	// Pre-flight: rough check that data will fit.
	if needed := estimateBlocks(root); l.FirstDataBlock+needed > l.TotalBlocks {
		return nil, fmt.Errorf(
			"mkfs: data requires ~%d blocks but partition only has %d free blocks",
			needed, l.TotalBlocks-l.FirstDataBlock,
		)
	}

	img := make([]byte, partSize)
	bm := newBitmap(&l)

	// Inode 2 belongs to root; it falls inside the reserved 1–10 range already
	// marked used by newBitmap, so no extra bookkeeping needed.
	root.inodeNum = rootInodeNum
	assignInodes(root, bm)

	// Pass 1: write regular-file and long-symlink data blocks.
	writeFileData(img, root, bm)

	// Pass 2: write directory data blocks (all inodes are assigned by now).
	writeDirData(img, root, root, bm)

	// Pass 3: write inode table.
	tableBase := int64(l.InodeTableBlock) * BlockSize
	writeAllInodes(img, root, tableBase)

	// Fixed-position structures.
	var uuid [16]byte
	crand.Read(uuid[:]) //nolint:errcheck
	dirCount := countDirNodes(root)
	writeSuperblock(img, &l, uuid, bm, dirCount)
	writeGDT(img, &l, bm, dirCount)
	copy(img[int64(l.BlockBitmapBlock)*BlockSize:], bm.blockBmap)
	copy(img[int64(l.InodeBitmapBlock)*BlockSize:], bm.inodeBmap)

	return bytes.NewReader(img), nil
}

// ── tar reading ───────────────────────────────────────────────────────────────

type rawEntry struct {
	hdr  *tar.Header
	data []byte
}

func readAll(tr *tar.Reader) ([]rawEntry, error) {
	var out []rawEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		e := rawEntry{hdr: hdr}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			e.data = make([]byte, hdr.Size)
			if _, err := io.ReadFull(tr, e.data); err != nil {
				return nil, fmt.Errorf("read %q: %w", hdr.Name, err)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// ── tree ──────────────────────────────────────────────────────────────────────

type fsNode struct {
	name     string
	hdr      *tar.Header
	data     []byte     // regular file content or long symlink target bytes
	children []*fsNode
	byName   map[string]*fsNode

	// set during layout
	inodeNum   uint32
	dataBlocks []uint32 // physical block numbers
}

func newNode(name string, hdr *tar.Header) *fsNode {
	return &fsNode{name: name, hdr: hdr, byName: make(map[string]*fsNode)}
}

func buildTree(entries []rawEntry) *fsNode {
	root := newNode("", &tar.Header{
		Typeflag: tar.TypeDir,
		Mode:     0755,
		ModTime:  time.Now(),
	})
	for i := range entries {
		e := &entries[i]
		p := cleanPath(e.hdr.Name)
		if p == "" {
			if e.hdr.Typeflag == tar.TypeDir {
				root.hdr = e.hdr // update root metadata
			}
			continue
		}
		insert(root, strings.Split(p, "/"), e)
	}
	return root
}

func cleanPath(s string) string {
	s = path.Clean(s)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	if s == "." {
		return ""
	}
	return s
}

func insert(parent *fsNode, parts []string, e *rawEntry) {
	name := parts[0]
	if len(parts) == 1 {
		n := newNode(name, e.hdr)
		n.data = e.data
		if old, ok := parent.byName[name]; ok {
			// Last entry for this path wins.
			for i, c := range parent.children {
				if c == old {
					parent.children[i] = n
					break
				}
			}
		} else {
			parent.children = append(parent.children, n)
		}
		parent.byName[name] = n
		return
	}
	// Synthesise missing intermediate directories.
	dir, ok := parent.byName[name]
	if !ok {
		dir = newNode(name, &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name,
			Mode:     0755,
			ModTime:  time.Now(),
		})
		parent.children = append(parent.children, dir)
		parent.byName[name] = dir
	}
	insert(dir, parts[1:], e)
}

func countNodes(n *fsNode) int {
	c := 1
	for _, ch := range n.children {
		c += countNodes(ch)
	}
	return c
}

func countDirNodes(n *fsNode) uint32 {
	var c uint32
	if n.hdr.Typeflag == tar.TypeDir {
		c = 1
	}
	for _, ch := range n.children {
		c += countDirNodes(ch)
	}
	return c
}

func estimateBlocks(n *fsNode) uint32 {
	var c uint32
	switch n.hdr.Typeflag {
	case tar.TypeDir:
		c = 1
	case tar.TypeReg:
		if sz := int64(len(n.data)); sz > 0 {
			c = uint32((sz + BlockSize - 1) / BlockSize)
		}
	case tar.TypeSymlink:
		if len(n.hdr.Linkname) > 60 {
			c = 1
		}
	}
	for _, ch := range n.children {
		c += estimateBlocks(ch)
	}
	return c
}

// ── inode assignment ──────────────────────────────────────────────────────────

func assignInodes(n *fsNode, bm *bitmap) {
	for _, ch := range n.children {
		ch.inodeNum = bm.allocInode()
		assignInodes(ch, bm)
	}
}

// ── file data ─────────────────────────────────────────────────────────────────

func writeFileData(img []byte, n *fsNode, bm *bitmap) {
	switch n.hdr.Typeflag {
	case tar.TypeReg:
		if len(n.data) > 0 {
			numBlk := (len(n.data) + BlockSize - 1) / BlockSize
			n.dataBlocks = make([]uint32, numBlk)
			for i := range n.dataBlocks {
				b := bm.allocBlock()
				n.dataBlocks[i] = b
				src := n.data[i*BlockSize:]
				if len(src) > BlockSize {
					src = src[:BlockSize]
				}
				copy(img[int64(b)*BlockSize:], src)
			}
		}
	case tar.TypeSymlink:
		if len(n.hdr.Linkname) > 60 {
			b := bm.allocBlock()
			n.dataBlocks = []uint32{b}
			copy(img[int64(b)*BlockSize:], n.hdr.Linkname)
		}
	}
	for _, ch := range n.children {
		writeFileData(img, ch, bm)
	}
}

// ── directory data ────────────────────────────────────────────────────────────

type dirEnt struct {
	inodeNum uint32
	name     string
	fileType uint8
}

func writeDirData(img []byte, n *fsNode, parent *fsNode, bm *bitmap) {
	if n.hdr.Typeflag == tar.TypeDir {
		ents := []dirEnt{
			{n.inodeNum, ".", 2},
			{parent.inodeNum, "..", 2},
		}
		for _, ch := range n.children {
			ents = append(ents, dirEnt{ch.inodeNum, ch.name, tarFileType(ch)})
		}
		blks := packDir(ents)
		n.dataBlocks = make([]uint32, len(blks))
		for i, blk := range blks {
			b := bm.allocBlock()
			n.dataBlocks[i] = b
			copy(img[int64(b)*BlockSize:], blk)
		}
	}
	for _, ch := range n.children {
		writeDirData(img, ch, n, bm)
	}
}

func tarFileType(n *fsNode) uint8 {
	switch n.hdr.Typeflag {
	case tar.TypeReg:
		return 1
	case tar.TypeDir:
		return 2
	case tar.TypeSymlink:
		return 7
	case tar.TypeChar:
		return 3
	case tar.TypeBlock:
		return 4
	case tar.TypeFifo:
		return 5
	default:
		return 0
	}
}

// packDir packs directory entries into one or more 4096-byte blocks.
//
// Record layout: [inode:4][rec_len:2][name_len:1][file_type:1][name…] padded to
// a 4-byte boundary.  The last record in every block stretches its rec_len to
// reach the block boundary, as the kernel expects.
func packDir(ents []dirEnt) [][]byte {
	if len(ents) == 0 {
		return nil
	}

	var blocks [][]byte
	blk := make([]byte, BlockSize)
	pos, lastPos := 0, 0

	flush := func() {
		// Stretch the last written entry to reach the block boundary.
		binary.LittleEndian.PutUint16(blk[lastPos+4:], uint16(BlockSize-lastPos))
		blocks = append(blocks, blk)
		blk = make([]byte, BlockSize)
		pos, lastPos = 0, 0
	}

	for i, e := range ents {
		nl := len(e.name)
		minRec := 8 + nl
		if minRec%4 != 0 {
			minRec += 4 - minRec%4
		}
		// Flush current block if the next entry won't fit.
		if pos > 0 && pos+minRec > BlockSize {
			flush()
		}

		isLast := i == len(ents)-1
		rec := minRec
		if isLast {
			rec = BlockSize - pos // last entry fills remaining space
		}

		lastPos = pos
		le := binary.LittleEndian
		le.PutUint32(blk[pos:], e.inodeNum)
		le.PutUint16(blk[pos+4:], uint16(rec))
		blk[pos+6] = uint8(nl)
		blk[pos+7] = e.fileType
		copy(blk[pos+8:], e.name)
		pos += rec

		if isLast {
			blocks = append(blocks, blk)
		}
	}
	return blocks
}

// ── inode serialization ───────────────────────────────────────────────────────

func writeAllInodes(img []byte, n *fsNode, tableBase int64) {
	off := tableBase + int64(n.inodeNum-1)*InodeSize
	copy(img[off:], serializeNode(n))
	for _, ch := range n.children {
		writeAllInodes(img, ch, tableBase)
	}
}

func serializeNode(n *fsNode) []byte {
	buf := make([]byte, InodeSize)
	le := binary.LittleEndian
	hdr := n.hdr

	mtime := uint32(hdr.ModTime.Unix())
	if mtime == 0 {
		mtime = uint32(time.Now().Unix())
	}

	var (
		mode   uint16
		flags  uint32
		iblock [60]byte
		size   int64
		links  uint16
		blocks uint32 // 512-byte disk sectors (i_blocks_lo)
	)

	switch hdr.Typeflag {
	case tar.TypeDir:
		mode = 0x4000 | permBits(hdr, 0755)
		flags = inodeFlagExtents
		size = int64(len(n.dataBlocks)) * BlockSize
		blocks = uint32(len(n.dataBlocks)) * 8
		links = uint16(2 + countSubdirs(n))
		if len(n.dataBlocks) > 0 {
			iblock, _ = buildIBlock(n.dataBlocks)
		}

	case tar.TypeReg:
		mode = 0x8000 | permBits(hdr, 0644)
		flags = inodeFlagExtents
		size = hdr.Size
		blocks = uint32((size + 511) / 512)
		links = 1
		if len(n.dataBlocks) > 0 {
			iblock, _ = buildIBlock(n.dataBlocks)
		}

	case tar.TypeSymlink:
		mode = 0xA000 | permBits(hdr, 0777)
		links = 1
		size = int64(len(hdr.Linkname))
		if len(hdr.Linkname) <= 60 {
			// Fast symlink: target stored inline in IBlock, no extents.
			copy(iblock[:], hdr.Linkname)
		} else {
			flags = inodeFlagExtents
			blocks = uint32(len(n.dataBlocks)) * 8
			iblock, _ = buildIBlock(n.dataBlocks)
		}

	case tar.TypeChar:
		mode = 0x2000 | permBits(hdr, 0600)
		links = 1
		encodeDevice(hdr, &iblock)

	case tar.TypeBlock:
		mode = 0x6000 | permBits(hdr, 0600)
		links = 1
		encodeDevice(hdr, &iblock)

	case tar.TypeFifo:
		mode = 0x1000 | permBits(hdr, 0600)
		links = 1
	}

	le.PutUint16(buf[0x00:], mode)
	le.PutUint16(buf[0x02:], uint16(hdr.Uid))
	le.PutUint32(buf[0x04:], uint32(size))         // i_size_lo
	le.PutUint32(buf[0x08:], mtime)                // i_atime
	le.PutUint32(buf[0x0C:], mtime)                // i_ctime
	le.PutUint32(buf[0x10:], mtime)                // i_mtime
	le.PutUint32(buf[0x14:], 0)                    // i_dtime (not deleted)
	le.PutUint16(buf[0x18:], uint16(hdr.Gid))
	le.PutUint16(buf[0x1A:], links)                // i_links_count
	le.PutUint32(buf[0x1C:], blocks)               // i_blocks_lo (512-byte units)
	le.PutUint32(buf[0x20:], flags)                // i_flags
	copy(buf[0x28:], iblock[:])                    // i_block (60 bytes)
	le.PutUint32(buf[0x6C:], uint32(size>>32))     // i_size_high
	le.PutUint16(buf[0x78:], uint16(hdr.Uid>>16))  // i_uid_high (in i_osd2)
	le.PutUint16(buf[0x7A:], uint16(hdr.Gid>>16))  // i_gid_high (in i_osd2)
	return buf
}

func permBits(hdr *tar.Header, def uint16) uint16 {
	if p := uint16(hdr.Mode & 0x0FFF); p != 0 {
		return p
	}
	return def
}

func countSubdirs(n *fsNode) int {
	c := 0
	for _, ch := range n.children {
		if ch.hdr.Typeflag == tar.TypeDir {
			c++
		}
	}
	return c
}

func encodeDevice(hdr *tar.Header, ib *[60]byte) {
	maj := uint32(hdr.Devmajor)
	min := uint32(hdr.Devminor)
	// Old encoding at i_block[0], new encoding at i_block[1].
	binary.LittleEndian.PutUint32(ib[0:], (maj<<8)|min)
	binary.LittleEndian.PutUint32(ib[4:], (maj<<20)|min)
}

// ── extent tree ───────────────────────────────────────────────────────────────

type extLeaf struct {
	logStart uint32
	physBlk  uint64
	length   uint16
}

// buildIBlock encodes a list of physical block numbers into a 60-byte inline
// extent tree (header + up to four leaf extents).
func buildIBlock(blocks []uint32) ([60]byte, error) {
	var ib [60]byte
	exts := toExtents(blocks)
	if len(exts) > maxInlineExtents {
		return ib, fmt.Errorf("mkfs: file needs %d extents, inline limit is %d — add support for extent index nodes to handle highly fragmented files", len(exts), maxInlineExtents)
	}
	le := binary.LittleEndian
	le.PutUint16(ib[0:], extentMagic)
	le.PutUint16(ib[2:], uint16(len(exts))) // eh_entries
	le.PutUint16(ib[4:], maxInlineExtents)  // eh_max
	// ib[6:8] = depth 0 (leaf), ib[8:12] = generation 0 — already zero.
	for i, ex := range exts {
		off := 12 + i*12
		le.PutUint32(ib[off:], ex.logStart)
		le.PutUint16(ib[off+4:], ex.length)
		le.PutUint16(ib[off+6:], uint16(ex.physBlk>>32)) // ee_start_hi
		le.PutUint32(ib[off+8:], uint32(ex.physBlk))     // ee_start_lo
	}
	return ib, nil
}

// toExtents converts a flat block list into a minimal run-length encoded slice.
func toExtents(blocks []uint32) []extLeaf {
	if len(blocks) == 0 {
		return nil
	}
	var exts []extLeaf
	logStart := uint32(0)
	physRun := uint64(blocks[0])
	runLen := uint16(1)
	for i := 1; i < len(blocks); i++ {
		if uint64(blocks[i]) == physRun+uint64(runLen) && runLen < 0x7FFF {
			runLen++
		} else {
			exts = append(exts, extLeaf{logStart, physRun, runLen})
			logStart = uint32(i)
			physRun = uint64(blocks[i])
			runLen = 1
		}
	}
	return append(exts, extLeaf{logStart, physRun, runLen})
}