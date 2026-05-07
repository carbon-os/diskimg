// Package btrfs implements a pure-Go Btrfs filesystem driver with full
// read/write support for single-device, uncompressed images.
//
// Architecture notes
// ──────────────────
//  • All B-tree nodes are accessed via a logical→physical chunk map
//    bootstrapped from the superblock's sys_chunk_array, then completed
//    by reading the chunk tree.
//  • Writes use a dirty-block cache keyed by physical byte offset.
//    Modified nodes stay at their existing logical addresses (no physical
//    CoW relocation); new nodes get fresh addresses via a high-water-mark
//    allocator.  Checksums are recomputed on every dirty node at flush.
//  • Full leaf splits are implemented; internal-node splits are deferred
//    (extremely rare for image-building workloads).
//  • Subvolumes are opened via OpenSubvol; the resulting Volume shares the
//    parent's dirty cache and writer so a single Unmount flushes everything.
package btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"time"

	volfs "github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/fs/fstype"
)

// ── on-disk constants ────────────────────────────────────────────────────────

const (
	sbOffset   = 0x10000    // primary superblock at 64 KiB
	sbSize     = 0x1000     // superblock occupies 4 KiB
	btrfsMagic = "_BHRfS_M" // at superblock+0x40

	nodeHdrSize  = 101 // 0x65 – node header in bytes
	keySize      = 17  // btrfs_disk_key
	keyPtrSize   = 33  // btrfs_key_ptr  (key + blockptr + generation)
	leafItemSize = 25  // btrfs_item     (key + offset + size)

	inodeItemSize = 160 // btrfs_inode_item
	dirItemHdr    = 30  // btrfs_dir_item fixed header
	inodeRefHdr   = 10  // btrfs_inode_ref fixed header
	extentDataHdr = 21  // btrfs_file_extent_item fixed part
	extentRegSize = 53  // extentDataHdr + 4×uint64 for regular extents
	chunkItemBase = 48  // btrfs_chunk base (before stripes)
	chunkStripe   = 32  // bytes per stripe entry
	rootItemSize  = 439 // btrfs_root_item

	// Well-known object IDs.
	objRootTree  = uint64(1)
	objFSTree    = uint64(5)
	objRootDir   = uint64(6)
	objFirstFree = uint64(256) // BTRFS_FIRST_FREE_OBJECTID (root dir inode)
	objChunkTree = uint64(256) // BTRFS_FIRST_CHUNK_TREE_OBJECTID

	// Item types.
	typeInodeItem  = uint8(0x01)
	typeInodeRef   = uint8(0x0c)
	typeDirItem    = uint8(0x54)
	typeDirIndex   = uint8(0x60)
	typeExtentData = uint8(0x6c)
	typeRootItem   = uint8(0x84)
	typeChunkItem  = uint8(0xe4)

	// Extent data sub-types.
	extInline   = uint8(0)
	extRegular  = uint8(1)
	extPrealloc = uint8(2)

	// Directory entry file-type codes.
	dirUnknown = uint8(0)
	dirReg     = uint8(1)
	dirDir     = uint8(2)
	dirChr     = uint8(3)
	dirBlk     = uint8(4)
	dirFifo    = uint8(5)
	dirSock    = uint8(6)
	dirLnk     = uint8(7)

	// i_mode type bits.
	ifmt   = uint32(0xF000)
	ifreg  = uint32(0x8000)
	ifdir  = uint32(0x4000)
	iflnk  = uint32(0xA000)
	ifchr  = uint32(0x2000)
	ifblk  = uint32(0x6000)
	ififo  = uint32(0x1000)
	ifsock = uint32(0xC000)

	// Inline extent threshold: files <= this many bytes stored inline.
	inlineMax = 2048

	// nameHashSeed is the CRC32c seed for Btrfs name hashing.
	nameHashSeed = ^uint32(1) // 0xFFFFFFFE

	// ROOT_ITEM.bytenr is at offset 176 (0xb0) in the 439-byte structure.
	rootItemBytNrOff = 176
	// ROOT_ITEM.level is at offset 238.
	rootItemLevelOff = 238
)

var errLeafFull = errors.New("btrfs: leaf full")

// ── key ──────────────────────────────────────────────────────────────────────

type btrfsKey struct {
	objectID uint64
	itemType uint8
	offset   uint64
}

func encodeKey(k btrfsKey, dst []byte) {
	le := binary.LittleEndian
	le.PutUint64(dst[0:8], k.objectID)
	dst[8] = k.itemType
	le.PutUint64(dst[9:17], k.offset)
}

func decodeKey(src []byte) btrfsKey {
	le := binary.LittleEndian
	return btrfsKey{
		objectID: le.Uint64(src[0:8]),
		itemType: src[8],
		offset:   le.Uint64(src[9:17]),
	}
}

func cmpKey(a, b btrfsKey) int {
	if a.objectID != b.objectID {
		if a.objectID < b.objectID {
			return -1
		}
		return 1
	}
	if a.itemType != b.itemType {
		if a.itemType < b.itemType {
			return -1
		}
		return 1
	}
	if a.offset != b.offset {
		if a.offset < b.offset {
			return -1
		}
		return 1
	}
	return 0
}

// ── superblock (parsed fields only) ──────────────────────────────────────────

type superblock struct {
	fsID         [16]byte
	generation   uint64
	rootLogical  uint64 // root tree root node
	chunkLogical uint64 // chunk tree root node
	totalBytes   uint64
	bytesUsed    uint64
	sectorSize   uint32
	nodeSize     uint32
	stripeSize   uint32
	sysArrSize   uint32 // sys_chunk_array used bytes
	csumType     uint16
	rootLevel    uint8
	chunkLevel   uint8
}

// ── chunk mapping ─────────────────────────────────────────────────────────────

// chunkMap maps a logical byte range to a physical offset on the single device.
type chunkMap struct {
	logStart  uint64 // logical start
	length    uint64
	physStart uint64 // physical start on device (single stripe)
}

// ── inode item ────────────────────────────────────────────────────────────────

type inodeItem struct {
	generation uint64
	transID    uint64
	size       uint64
	nbytes     uint64
	blockGroup uint64
	nlink      uint32
	uid        uint32
	gid        uint32
	mode       uint32
	rdev       uint64
	iflags     uint64
	sequence   uint64
	atime      int64
	atimeNsec  uint32
	ctime      int64
	ctimeNsec  uint32
	mtime      int64
	mtimeNsec  uint32
	otime      int64
	otimeNsec  uint32
}

func encodeInodeItem(in *inodeItem) []byte {
	buf := make([]byte, inodeItemSize)
	le := binary.LittleEndian
	le.PutUint64(buf[0:], in.generation)
	le.PutUint64(buf[8:], in.transID)
	le.PutUint64(buf[16:], in.size)
	le.PutUint64(buf[24:], in.nbytes)
	le.PutUint64(buf[32:], in.blockGroup)
	le.PutUint32(buf[40:], in.nlink)
	le.PutUint32(buf[44:], in.uid)
	le.PutUint32(buf[48:], in.gid)
	le.PutUint32(buf[52:], in.mode)
	le.PutUint64(buf[56:], in.rdev)
	le.PutUint64(buf[64:], in.iflags)
	le.PutUint64(buf[72:], in.sequence)
	// reserved[0..3] at 80-111 stay zero
	le.PutUint64(buf[112:], uint64(in.atime))
	le.PutUint32(buf[120:], in.atimeNsec)
	le.PutUint64(buf[124:], uint64(in.ctime))
	le.PutUint32(buf[132:], in.ctimeNsec)
	le.PutUint64(buf[136:], uint64(in.mtime))
	le.PutUint32(buf[144:], in.mtimeNsec)
	le.PutUint64(buf[148:], uint64(in.otime))
	le.PutUint32(buf[156:], in.otimeNsec)
	return buf
}

func decodeInodeItem(src []byte) inodeItem {
	le := binary.LittleEndian
	var in inodeItem
	in.generation = le.Uint64(src[0:])
	in.transID = le.Uint64(src[8:])
	in.size = le.Uint64(src[16:])
	in.nbytes = le.Uint64(src[24:])
	in.blockGroup = le.Uint64(src[32:])
	in.nlink = le.Uint32(src[40:])
	in.uid = le.Uint32(src[44:])
	in.gid = le.Uint32(src[48:])
	in.mode = le.Uint32(src[52:])
	in.rdev = le.Uint64(src[56:])
	in.iflags = le.Uint64(src[64:])
	in.sequence = le.Uint64(src[72:])
	in.atime = int64(le.Uint64(src[112:]))
	in.atimeNsec = le.Uint32(src[120:])
	in.ctime = int64(le.Uint64(src[124:]))
	in.ctimeNsec = le.Uint32(src[132:])
	in.mtime = int64(le.Uint64(src[136:]))
	in.mtimeNsec = le.Uint32(src[144:])
	in.otime = int64(le.Uint64(src[148:]))
	in.otimeNsec = le.Uint32(src[156:])
	return in
}

// ── Volume ────────────────────────────────────────────────────────────────────

// Volume implements fs.Volume for a Btrfs partition.
type Volume struct {
	mu    sync.Mutex
	sr    *io.SectionReader
	wa    io.WriterAt
	srOff int64 // partition start in underlying file

	sb     superblock
	chunks []chunkMap

	// B-tree roots (logical addresses; may change after splits).
	rootTreeRoot uint64
	fsTreeRoot   uint64

	// Dirty block cache: physical offset → nodeSize bytes.
	// Shared with parent when this is a subvolume so a single flush covers all
	// writes regardless of which Volume they went through.
	dirty map[int64][]byte

	// High-water allocator: next free physical/logical byte for new nodes.
	allocPtr uint64

	// Next available objectID in the FS tree.
	nextObjID uint64

	// Next DIR_INDEX sequence number per directory (objectID → sequence).
	dirSeq map[uint64]uint64

	crcTable *crc32.Table

	// parent is non-nil when this Volume was created via OpenSubvol.
	// Unmount delegates flush to the parent so the root tree and superblock
	// are written exactly once, by the owner of the backing writer.
	parent *Volume
}

// Open parses the Btrfs superblock and tree metadata from the given partition
// bounds and returns a ready-to-use Volume.
func Open(ra io.ReaderAt, offset, size int64) (*Volume, error) {
	v := &Volume{
		sr:       io.NewSectionReader(ra, offset, size),
		dirty:    make(map[int64][]byte),
		dirSeq:   make(map[uint64]uint64),
		crcTable: crc32.MakeTable(crc32.Castagnoli),
	}
	if wa, ok := ra.(io.WriterAt); ok {
		v.wa = wa
		v.srOff = offset
	}
	if err := v.readSuperblock(); err != nil {
		return nil, err
	}
	if err := v.loadChunks(); err != nil {
		return nil, err
	}
	if err := v.findFSTreeRoot(); err != nil {
		return nil, err
	}
	v.allocPtr = v.findAllocPtr()
	v.nextObjID = v.scanMaxObjID() + 1
	return v, nil
}

// OpenSubvol returns a new Volume scoped to the named subvolume.
//
// The subvolume is located by scanning DIR_INDEX entries in the current
// volume's FS tree for an entry whose location key has itemType ==
// typeRootItem, then resolving that objectID to its tree root via the
// root tree.  The returned Volume shares the parent's dirty cache and
// writer; calling Unmount on it flushes through the parent.
func (v *Volume) OpenSubvol(name string) (*Volume, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	rootLogical, err := v.findSubvolByName(name)
	if err != nil {
		return nil, err
	}

	sub := &Volume{
		sr:           v.sr,
		wa:           v.wa,
		srOff:        v.srOff,
		sb:           v.sb,
		chunks:       v.chunks, // slice header copy; underlying array shared
		rootTreeRoot: v.rootTreeRoot,
		fsTreeRoot:   rootLogical,
		dirty:        v.dirty, // shared — writes from either Volume flush together
		dirSeq:       make(map[uint64]uint64),
		crcTable:     v.crcTable,
		parent:       v,
	}
	sub.allocPtr = sub.findAllocPtr()
	sub.nextObjID = sub.scanMaxObjID() + 1
	return sub, nil
}

// ListSubvols returns the names of all direct subvolumes visible in the
// current volume's FS tree (i.e. DIR_INDEX entries pointing to ROOT_ITEMs).
func (v *Volume) ListSubvols() ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	le := binary.LittleEndian
	var names []string
	err := v.walkTree(v.fsTreeRoot, func(k btrfsKey, d []byte) error {
		if k.itemType != typeDirIndex || len(d) < dirItemHdr {
			return nil
		}
		loc := decodeKey(d[0:keySize])
		if loc.itemType != typeRootItem {
			return nil // ordinary directory entry, not a subvolume
		}
		nameLen := int(le.Uint16(d[27:]))
		if 30+nameLen > len(d) {
			return nil
		}
		names = append(names, string(d[30:30+nameLen]))
		return nil
	})
	return names, err
}

// findSubvolByName is the un-locked core of OpenSubvol.
// It walks the current FS tree for a DIR_INDEX entry whose name matches and
// whose location.itemType == typeRootItem, then looks up the ROOT_ITEM in the
// root tree to obtain the subvolume's tree-root logical address.
func (v *Volume) findSubvolByName(name string) (uint64, error) {
	le := binary.LittleEndian
	var subvolObjID uint64

	err := v.walkTree(v.fsTreeRoot, func(k btrfsKey, d []byte) error {
		if k.itemType != typeDirIndex || len(d) < dirItemHdr {
			return nil
		}
		loc := decodeKey(d[0:keySize])
		if loc.itemType != typeRootItem {
			return nil
		}
		nameLen := int(le.Uint16(d[27:]))
		if 30+nameLen > len(d) {
			return nil
		}
		if string(d[30:30+nameLen]) == name {
			subvolObjID = loc.objectID
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("btrfs: scan for subvolume %q: %w", name, err)
	}
	if subvolObjID == 0 {
		return 0, fmt.Errorf("btrfs: subvolume %q not found", name)
	}

	// Resolve objectID → tree root via the root tree.
	target := btrfsKey{objectID: subvolObjID, itemType: typeRootItem, offset: 0}
	data, ok, err := v.searchTree(v.rootTreeRoot, target)
	if err != nil {
		return 0, fmt.Errorf("btrfs: root tree lookup for subvolume %q: %w", name, err)
	}
	if !ok || len(data) < rootItemBytNrOff+8 {
		return 0, fmt.Errorf("btrfs: ROOT_ITEM not found for subvolume %q (objectID %d)", name, subvolObjID)
	}
	return le.Uint64(data[rootItemBytNrOff:]), nil
}

// Type returns "btrfs".
func (v *Volume) Type() fstype.Type { return fstype.Btrfs }

// ── superblock ────────────────────────────────────────────────────────────────

func (v *Volume) readSuperblock() error {
	buf := make([]byte, sbSize)
	if _, err := v.sr.ReadAt(buf, sbOffset); err != nil {
		return fmt.Errorf("btrfs: read superblock: %w", err)
	}
	if string(buf[0x40:0x48]) != btrfsMagic {
		return fmt.Errorf("btrfs: bad magic %q", buf[0x40:0x48])
	}
	le := binary.LittleEndian
	sb := &v.sb
	copy(sb.fsID[:], buf[0x20:0x30])
	sb.generation = le.Uint64(buf[0x48:])
	sb.rootLogical = le.Uint64(buf[0x50:])
	sb.chunkLogical = le.Uint64(buf[0x58:])
	sb.totalBytes = le.Uint64(buf[0x70:])
	sb.bytesUsed = le.Uint64(buf[0x78:])
	sb.sectorSize = le.Uint32(buf[0x90:])
	sb.nodeSize = le.Uint32(buf[0x94:])
	sb.stripeSize = le.Uint32(buf[0x9c:])
	sb.sysArrSize = le.Uint32(buf[0xa0:])
	sb.csumType = le.Uint16(buf[0xc4:])
	sb.rootLevel = buf[0xc6]
	sb.chunkLevel = buf[0xc7]

	if sb.nodeSize == 0 {
		sb.nodeSize = 16384
	}
	v.rootTreeRoot = sb.rootLogical
	return nil
}

// ── chunk map bootstrap ───────────────────────────────────────────────────────

func (v *Volume) loadChunks() error {
	if err := v.parseSysChunkArray(); err != nil {
		return err
	}
	return v.loadChunkTree()
}

func (v *Volume) parseSysChunkArray() error {
	buf := make([]byte, sbSize)
	if _, err := v.sr.ReadAt(buf, sbOffset); err != nil {
		return fmt.Errorf("btrfs: read superblock (chunks): %w", err)
	}
	arr := buf[0x32b : 0x32b+int(v.sb.sysArrSize)]
	pos := 0
	le := binary.LittleEndian
	for pos+keySize < len(arr) {
		pos += keySize
		if pos+chunkItemBase > len(arr) {
			break
		}
		numStripes := int(le.Uint16(arr[pos+44:]))
		itemLen := chunkItemBase + numStripes*chunkStripe
		if pos+itemLen > len(arr) {
			break
		}
		logicalStart := le.Uint64(arr[pos-keySize+9:])
		length := le.Uint64(arr[pos:])
		physStart := uint64(0)
		if numStripes > 0 {
			physStart = le.Uint64(arr[pos+chunkItemBase+8:])
		}
		v.addChunk(logicalStart, length, physStart)
		pos += itemLen
	}
	return nil
}

func (v *Volume) loadChunkTree() error {
	return v.walkTree(v.sb.chunkLogical, func(k btrfsKey, data []byte) error {
		if k.itemType != typeChunkItem {
			return nil
		}
		le := binary.LittleEndian
		if len(data) < chunkItemBase {
			return nil
		}
		logicalStart := k.offset
		length := le.Uint64(data[0:])
		numStripes := int(le.Uint16(data[44:]))
		physStart := uint64(0)
		if numStripes > 0 && len(data) >= chunkItemBase+chunkStripe {
			physStart = le.Uint64(data[chunkItemBase+8:])
		}
		v.addChunk(logicalStart, length, physStart)
		return nil
	})
}

func (v *Volume) addChunk(logStart, length, physStart uint64) {
	for _, c := range v.chunks {
		if c.logStart == logStart {
			return
		}
	}
	v.chunks = append(v.chunks, chunkMap{logStart: logStart, length: length, physStart: physStart})
}

// ── FS tree root ──────────────────────────────────────────────────────────────

func (v *Volume) findFSTreeRoot() error {
	target := btrfsKey{objectID: objFSTree, itemType: typeRootItem, offset: 0}
	data, ok, err := v.searchTree(v.rootTreeRoot, target)
	if err != nil {
		return fmt.Errorf("btrfs: read root tree: %w", err)
	}
	if !ok || len(data) < rootItemBytNrOff+8 {
		return fmt.Errorf("btrfs: FS_TREE ROOT_ITEM not found")
	}
	v.fsTreeRoot = binary.LittleEndian.Uint64(data[rootItemBytNrOff:])
	return nil
}

// ── allocator helpers ─────────────────────────────────────────────────────────

func (v *Volume) findAllocPtr() uint64 {
	var end uint64
	for _, c := range v.chunks {
		if c.logStart+c.length > end {
			end = c.logStart + c.length
		}
	}
	ns := uint64(v.sb.nodeSize)
	return (end + ns - 1) / ns * ns
}

func (v *Volume) scanMaxObjID() uint64 {
	var maxID uint64 = objFirstFree
	_ = v.walkTree(v.fsTreeRoot, func(k btrfsKey, _ []byte) error {
		if k.objectID > maxID {
			maxID = k.objectID
		}
		return nil
	})
	return maxID
}

// ── low-level node I/O ────────────────────────────────────────────────────────

func (v *Volume) logToPhys(logical uint64) (int64, error) {
	for _, c := range v.chunks {
		if logical >= c.logStart && logical < c.logStart+c.length {
			return int64(c.physStart + (logical - c.logStart)), nil
		}
	}
	return 0, fmt.Errorf("btrfs: no chunk mapping for logical %#x", logical)
}

func (v *Volume) readNode(logical uint64) ([]byte, error) {
	phys, err := v.logToPhys(logical)
	if err != nil {
		return nil, err
	}
	if data, ok := v.dirty[phys]; ok {
		cpy := make([]byte, len(data))
		copy(cpy, data)
		return cpy, nil
	}
	buf := make([]byte, v.sb.nodeSize)
	if _, err := v.sr.ReadAt(buf, phys); err != nil {
		return nil, fmt.Errorf("btrfs: read node @%#x: %w", logical, err)
	}
	return buf, nil
}

func (v *Volume) writeNode(logical uint64, data []byte) error {
	phys, err := v.logToPhys(logical)
	if err != nil {
		return err
	}
	v.checksumNode(data)
	cpy := make([]byte, len(data))
	copy(cpy, data)
	v.dirty[phys] = cpy
	return nil
}

func (v *Volume) allocNode() (uint64, []byte) {
	logical := v.allocPtr
	ns := uint64(v.sb.nodeSize)
	v.allocPtr += ns

	if _, err := v.logToPhys(logical); err != nil {
		if len(v.chunks) > 0 {
			last := &v.chunks[len(v.chunks)-1]
			if logical == last.logStart+last.length {
				last.length += ns
			} else {
				v.chunks = append(v.chunks, chunkMap{
					logStart:  logical,
					length:    ns,
					physStart: logical,
				})
			}
		}
	}

	buf := make([]byte, v.sb.nodeSize)
	copy(buf[32:48], v.sb.fsID[:])
	return logical, buf
}

// ── checksums ─────────────────────────────────────────────────────────────────

func (v *Volume) csum32(data []byte) uint32 {
	return crc32.Update(0, v.crcTable, data)
}

func (v *Volume) checksumNode(data []byte) {
	sum := v.csum32(data[32:])
	binary.LittleEndian.PutUint32(data[0:4], sum)
	for i := 4; i < 32; i++ {
		data[i] = 0
	}
}

func (v *Volume) checksumSuperblock(buf []byte) {
	sum := v.csum32(buf[32:sbSize])
	binary.LittleEndian.PutUint32(buf[0:4], sum)
	for i := 4; i < 32; i++ {
		buf[i] = 0
	}
}

// ── StatFS ────────────────────────────────────────────────────────────────────

func (v *Volume) StatFS() (volfs.VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return volfs.VolumeInfo{
		TotalBytes: int64(v.sb.totalBytes),
		FreeBytes:  int64(v.sb.totalBytes - v.sb.bytesUsed),
		UsedBytes:  int64(v.sb.bytesUsed),
		BlockSize:  int64(v.sb.sectorSize),
	}, nil
}

// ── time helpers ──────────────────────────────────────────────────────────────

func nowSec() int64          { return time.Now().Unix() }
func toTime(sec int64) time.Time { return time.Unix(sec, 0) }