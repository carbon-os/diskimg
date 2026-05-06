// Package ext4 implements a pure-Go ext4 filesystem driver with full read/write
// support.  All I/O goes through an io.SectionReader (zero copy, no RAM mapped).
package ext4

import (
	"encoding/binary"
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
	sbOffset    = 1024 // superblock byte offset from partition start
	ext4Magic   = 0xEF53
	extentMagic = 0xF30A

	// Well-known inode numbers.
	inodeBadBlocks = 1
	inodeRoot      = 2
	inodeJournal   = 8
	inodeFirstUser = 11

	// i_flags bits.
	inodeFlagExtents  = 0x00080000
	inodeFlagInline   = 0x10000000
	inodeFlagHugeFile = 0x00040000

	// i_mode type bits.
	modeFmt  = 0xF000
	modeFifo = 0x1000
	modeChr  = 0x2000
	modeDir  = 0x4000
	modeBlk  = 0x6000
	modeReg  = 0x8000
	modeLnk  = 0xA000
	modeSock = 0xC000

	// Directory file_type codes.
	ftUnknown = 0
	ftRegFile = 1
	ftDir     = 2
	ftChrdev  = 3
	ftBlkdev  = 4
	ftFifo    = 5
	ftSock    = 6
	ftSymlink = 7

	// Superblock feature flags.
	featureCompatHasJournal = 0x00000004
	featureIncompatExtents  = 0x00000040
	featureIncompat64bit    = 0x00000080
	featureIncompatFlexBg   = 0x00000200
	featureIncompatFiletype = 0x00000002

	featureRoCompatMetaCsum = 0x00000400
)

// Superblock parsed from the on-disk 1024-byte structure.
type superblock struct {
	inodesCount      uint32
	blocksCountLo    uint32
	freeBlocksLo     uint32
	freeInodes       uint32
	firstDataBlock   uint32
	logBlockSize     uint32
	blocksPerGroup   uint32
	inodesPerGroup   uint32
	magic            uint16
	state            uint16
	featureCompat    uint32
	featureIncompat  uint32
	featureRoCompat  uint32
	uuid             [16]byte
	firstIno         uint32
	inodeSize        uint16
	blockGroupNr     uint16
	descSize         uint16 // block group descriptor size
	logGroupsPerFlex uint8
	blocksCountHi    uint32
	freeBlocksHi     uint32
	journalInum      uint32

	// Computed.
	blockSize  uint32
	groupCount uint32
}

// groupDesc holds the decoded fields of a block group descriptor.
type groupDesc struct {
	blockBitmapLo  uint32
	inodeBitmapLo  uint32
	inodeTableLo   uint32
	freeBlocksLo   uint16
	freeInodesLo   uint16
	usedDirsLo     uint16
	flags          uint16
	blockBitmapHi  uint32
	inodeBitmapHi  uint32
	inodeTableHi   uint32
	freeBlocksHi   uint16
	freeInodesHi   uint16
	usedDirsHi     uint16
	itableUnusedLo uint16
	checksum       uint16
}

// inode is the in-memory representation of an ext4 inode.
type inode struct {
	mode       uint16
	uid        uint16
	sizeLo     uint32
	atime      uint32
	ctime      uint32
	mtime      uint32
	dtime      uint32
	gid        uint16
	linksCount uint16
	blocksLo   uint32
	flags      uint32
	iBlock     [60]byte // extent root or inline data
	generation uint32
	fileAclLo  uint32
	sizeHi     uint32
	// osd2 sub-fields
	blocksHi   uint16
	fileAclHi  uint16
	uidHi      uint16
	gidHi      uint16
	checksumLo uint16
	// extra fields (inode size > 128)
	extraIsize  uint16
	checksumHi  uint16
	ctimeExtra  uint32
	mtimeExtra  uint32
	atimeExtra  uint32
	crtime      uint32
	crtimeExtra uint32
	versionHi   uint32
}

// extentHeader is the 12-byte header at the root of an extent node.
type extentHeader struct {
	magic      uint16
	entries    uint16
	max        uint16
	depth      uint16
	generation uint32
}

// extentIdx is a 12-byte internal (index) node of the extent B-tree.
type extentIdx struct {
	block  uint32 // first file block covered
	leafLo uint32
	leafHi uint16
	unused uint16
}

// extentLeaf is a 12-byte leaf node of the extent B-tree.
type extentLeaf struct {
	block   uint32 // first file logical block
	len     uint16 // number of blocks (bit15 = uninitialized)
	startHi uint16 // high 16 bits of physical start
	startLo uint32 // low 32 bits of physical start
}

// Volume implements fs.Volume for an ext4 partition.
type Volume struct {
	mu sync.Mutex

	sr       *io.SectionReader // raw partition access
	wa       io.WriterAt       // backing writer for flush ops
	srOffset int64             // base byte offset
	sb       superblock
	groups   []groupDesc

	// dirty block cache: blockNum → 4096-byte data
	// All writes land here first; Unmount flushes to sr.
	dirty map[uint64][]byte

	// dirty inode cache: inodeNum → raw 256-byte inode bytes
	dirtyInodes map[uint32][]byte

	castagnoliTable *crc32.Table // for metadata checksums
}

// Open parses the ext4 superblock and group descriptors from the given bounds
// and returns a ready-to-use Volume.
func Open(ra io.ReaderAt, offset, size int64) (*Volume, error) {
	v := &Volume{
		sr:          io.NewSectionReader(ra, offset, size),
		dirty:       make(map[uint64][]byte),
		dirtyInodes: make(map[uint32][]byte),
	}
	// Plumb the underlying writer if possible so we can flush updates
	if wa, ok := ra.(io.WriterAt); ok {
		v.wa = wa
		v.srOffset = offset
	}

	if err := v.readSuperblock(); err != nil {
		return nil, err
	}
	if err := v.readGroupDescriptors(); err != nil {
		return nil, err
	}
	return v, nil
}

// Type returns "ext4".
func (v *Volume) Type() fstype.Type { return fstype.Ext4 }

// ── superblock ───────────────────────────────────────────────────────────────

func (v *Volume) readSuperblock() error {
	buf := make([]byte, 1024)
	if _, err := v.sr.ReadAt(buf, sbOffset); err != nil {
		return fmt.Errorf("ext4: read superblock: %w", err)
	}
	le := binary.LittleEndian

	magic := le.Uint16(buf[56:58])
	if magic != ext4Magic {
		return fmt.Errorf("ext4: bad magic 0x%04X (want 0x%04X)", magic, ext4Magic)
	}

	sb := &v.sb
	sb.inodesCount = le.Uint32(buf[0:4])
	sb.blocksCountLo = le.Uint32(buf[4:8])
	sb.freeBlocksLo = le.Uint32(buf[12:16])
	sb.freeInodes = le.Uint32(buf[16:20])
	sb.firstDataBlock = le.Uint32(buf[20:24])
	sb.logBlockSize = le.Uint32(buf[24:28])
	sb.blocksPerGroup = le.Uint32(buf[32:36])
	sb.inodesPerGroup = le.Uint32(buf[40:44])
	sb.magic = magic
	sb.state = le.Uint16(buf[58:60])
	sb.featureCompat = le.Uint32(buf[92:96])
	sb.featureIncompat = le.Uint32(buf[96:100])
	sb.featureRoCompat = le.Uint32(buf[100:104])
	copy(sb.uuid[:], buf[104:120])
	sb.firstIno = le.Uint32(buf[84:88])
	sb.inodeSize = le.Uint16(buf[88:90])
	sb.blockGroupNr = le.Uint16(buf[90:92])
	sb.descSize = le.Uint16(buf[254:256])
	sb.journalInum = le.Uint32(buf[224:228])
	if sb.featureIncompat&featureIncompat64bit != 0 {
		sb.blocksCountHi = le.Uint32(buf[336:340])
		sb.freeBlocksHi = le.Uint32(buf[344:348])
	}
	if sb.featureIncompat&featureIncompatFlexBg != 0 {
		sb.logGroupsPerFlex = buf[372]
	}

	sb.blockSize = 1024 << sb.logBlockSize
	if sb.descSize == 0 {
		sb.descSize = 32
	}

	totalBlocks := uint64(sb.blocksCountLo) | uint64(sb.blocksCountHi)<<32
	sb.groupCount = uint32((totalBlocks + uint64(sb.blocksPerGroup) - 1) / uint64(sb.blocksPerGroup))

	return nil
}

// ── group descriptors ────────────────────────────────────────────────────────

func (v *Volume) readGroupDescriptors() error {
	// GDT starts at block 1 (or block 2 for 1K block size where firstDataBlock=1).
	gdtBlock := uint64(v.sb.firstDataBlock) + 1
	gdtOff := int64(gdtBlock) * int64(v.sb.blockSize)

	totalBytes := int(v.sb.groupCount) * int(v.sb.descSize)
	buf := make([]byte, totalBytes)
	if _, err := v.sr.ReadAt(buf, gdtOff); err != nil {
		return fmt.Errorf("ext4: read GDT: %w", err)
	}

	v.groups = make([]groupDesc, v.sb.groupCount)
	le := binary.LittleEndian
	ds := int(v.sb.descSize)
	for i := range v.groups {
		b := buf[i*ds:]
		gd := &v.groups[i]
		gd.blockBitmapLo = le.Uint32(b[0:4])
		gd.inodeBitmapLo = le.Uint32(b[4:8])
		gd.inodeTableLo = le.Uint32(b[8:12])
		gd.freeBlocksLo = le.Uint16(b[12:14])
		gd.freeInodesLo = le.Uint16(b[14:16])
		gd.usedDirsLo = le.Uint16(b[16:18])
		gd.flags = le.Uint16(b[18:20])
		if ds >= 64 {
			gd.blockBitmapHi = le.Uint32(b[32:36])
			gd.inodeBitmapHi = le.Uint32(b[36:40])
			gd.inodeTableHi = le.Uint32(b[40:44])
			gd.freeBlocksHi = le.Uint16(b[44:46])
			gd.freeInodesHi = le.Uint16(b[46:48])
			gd.usedDirsHi = le.Uint16(b[48:50])
		}
		gd.checksum = le.Uint16(b[30:32])
	}
	return nil
}

// ── helper: block I/O ────────────────────────────────────────────────────────

// readBlock reads one filesystem block (blockSize bytes) from block number blk.
// Dirty (in-memory) data takes precedence over on-disk data.
func (v *Volume) readBlock(blk uint64) ([]byte, error) {
	if data, ok := v.dirty[blk]; ok {
		cpy := make([]byte, len(data))
		copy(cpy, data)
		return cpy, nil
	}
	buf := make([]byte, v.sb.blockSize)
	if _, err := v.sr.ReadAt(buf, int64(blk)*int64(v.sb.blockSize)); err != nil {
		return nil, fmt.Errorf("ext4: read block %d: %w", blk, err)
	}
	return buf, nil
}

// writeBlock stages a block write into the dirty cache.
func (v *Volume) writeBlock(blk uint64, data []byte) {
	cpy := make([]byte, v.sb.blockSize)
	copy(cpy, data)
	v.dirty[blk] = cpy
}

// ── helper: inode I/O ────────────────────────────────────────────────────────

// inodeBlockGroup returns the 0-based block group index for an inode number.
func (v *Volume) inodeBlockGroup(num uint32) uint32 {
	return (num - 1) / v.sb.inodesPerGroup
}

// inodeLocalIndex returns the 0-based index within its group's inode table.
func (v *Volume) inodeLocalIndex(num uint32) uint32 {
	return (num - 1) % v.sb.inodesPerGroup
}

// inodeTableBlock returns the physical block number of the inode table for
// the given 0-based block group.
func (v *Volume) inodeTableBlock(grp uint32) uint64 {
	gd := &v.groups[grp]
	lo := uint64(gd.inodeTableLo)
	hi := uint64(gd.inodeTableHi)
	return lo | (hi << 32)
}

// readRawInode reads the raw inode bytes for inode number num (1-based).
func (v *Volume) readRawInode(num uint32) ([]byte, error) {
	if b, ok := v.dirtyInodes[num]; ok {
		cpy := make([]byte, len(b))
		copy(cpy, b)
		return cpy, nil
	}
	grp := v.inodeBlockGroup(num)
	localIdx := v.inodeLocalIndex(num)
	tableBlk := v.inodeTableBlock(grp)
	byteOff := int64(tableBlk)*int64(v.sb.blockSize) + int64(localIdx)*int64(v.sb.inodeSize)

	buf := make([]byte, v.sb.inodeSize)
	if _, err := v.sr.ReadAt(buf, byteOff); err != nil {
		return nil, fmt.Errorf("ext4: read inode %d: %w", num, err)
	}
	return buf, nil
}

// writeRawInode stages a raw inode into the dirty inode cache.
func (v *Volume) writeRawInode(num uint32, raw []byte) {
	cpy := make([]byte, len(raw))
	copy(cpy, raw)
	v.dirtyInodes[num] = cpy
}

// parseInode decodes raw inode bytes into the inode struct.
func parseInode(raw []byte) inode {
	le := binary.LittleEndian
	var in inode
	in.mode = le.Uint16(raw[0:2])
	in.uid = le.Uint16(raw[2:4])
	in.sizeLo = le.Uint32(raw[4:8])
	in.atime = le.Uint32(raw[8:12])
	in.ctime = le.Uint32(raw[12:16])
	in.mtime = le.Uint32(raw[16:20])
	in.dtime = le.Uint32(raw[20:24])
	in.gid = le.Uint16(raw[24:26])
	in.linksCount = le.Uint16(raw[26:28])
	in.blocksLo = le.Uint32(raw[28:32])
	in.flags = le.Uint32(raw[32:36])
	copy(in.iBlock[:], raw[40:100])
	in.generation = le.Uint32(raw[100:104])
	in.fileAclLo = le.Uint32(raw[104:108])
	in.sizeHi = le.Uint32(raw[108:112])
	if len(raw) >= 128 {
		in.blocksHi = le.Uint16(raw[116:118])
		in.fileAclHi = le.Uint16(raw[118:120])
		in.uidHi = le.Uint16(raw[120:122])
		in.gidHi = le.Uint16(raw[122:124])
	}
	if len(raw) >= 160 {
		in.extraIsize = le.Uint16(raw[128:130])
		in.ctimeExtra = le.Uint32(raw[132:136])
		in.mtimeExtra = le.Uint32(raw[136:140])
		in.atimeExtra = le.Uint32(raw[140:144])
		in.crtime = le.Uint32(raw[144:148])
	}
	return in
}

// encodeInode writes the inode struct back into a raw byte slice.
func encodeInode(in *inode, raw []byte) {
	le := binary.LittleEndian
	le.PutUint16(raw[0:2], in.mode)
	le.PutUint16(raw[2:4], in.uid)
	le.PutUint32(raw[4:8], in.sizeLo)
	le.PutUint32(raw[8:12], in.atime)
	le.PutUint32(raw[12:16], in.ctime)
	le.PutUint32(raw[16:20], in.mtime)
	le.PutUint32(raw[20:24], in.dtime)
	le.PutUint16(raw[24:26], in.gid)
	le.PutUint16(raw[26:28], in.linksCount)
	le.PutUint32(raw[28:32], in.blocksLo)
	le.PutUint32(raw[32:36], in.flags)
	copy(raw[40:100], in.iBlock[:])
	le.PutUint32(raw[100:104], in.generation)
	le.PutUint32(raw[104:108], in.fileAclLo)
	le.PutUint32(raw[108:112], in.sizeHi)
	if len(raw) >= 128 {
		le.PutUint16(raw[116:118], in.blocksHi)
		le.PutUint16(raw[118:120], in.fileAclHi)
		le.PutUint16(raw[120:122], in.uidHi)
		le.PutUint16(raw[122:124], in.gidHi)
	}
	if len(raw) >= 160 {
		le.PutUint16(raw[128:130], in.extraIsize)
		le.PutUint32(raw[132:136], in.ctimeExtra)
		le.PutUint32(raw[136:140], in.mtimeExtra)
		le.PutUint32(raw[140:144], in.atimeExtra)
		le.PutUint32(raw[144:148], in.crtime)
	}
}

// readInode reads and parses inode num.
func (v *Volume) readInode(num uint32) (inode, error) {
	raw, err := v.readRawInode(num)
	if err != nil {
		return inode{}, err
	}
	return parseInode(raw), nil
}

// writeInode encodes in and stages it for write.
func (v *Volume) writeInode(num uint32, in *inode) error {
	raw, err := v.readRawInode(num)
	if err != nil {
		// Allocate fresh bytes if not yet on disk.
		raw = make([]byte, v.sb.inodeSize)
	}
	encodeInode(in, raw)
	v.writeRawInode(num, raw)
	return nil
}

// inodeSize returns the logical file size combining sizeLo and sizeHi.
func inodeFileSize(in *inode) int64 {
	return int64(in.sizeLo) | int64(in.sizeHi)<<32
}

// ── helper: time ─────────────────────────────────────────────────────────────

func timeToExt4(t time.Time) uint32 {
	return uint32(t.Unix())
}

func ext4ToTime(sec uint32) time.Time {
	return time.Unix(int64(sec), 0)
}

// nowSec returns the current time as a uint32 Unix timestamp.
func nowSec() uint32 { return timeToExt4(time.Now()) }

// ── StatFS ───────────────────────────────────────────────────────────────────

func (v *Volume) StatFS() (volfs.VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	totalBlocks := uint64(v.sb.blocksCountLo) | uint64(v.sb.blocksCountHi)<<32
	freeBlocks := uint64(v.sb.freeBlocksLo) | uint64(v.sb.freeBlocksHi)<<32

	bs := int64(v.sb.blockSize)
	return volfs.VolumeInfo{
		TotalBytes: int64(totalBlocks) * bs,
		FreeBytes:  int64(freeBlocks) * bs,
		UsedBytes:  int64(totalBlocks-freeBlocks) * bs,
		BlockSize:  bs,
		Inodes:     int64(v.sb.inodesCount),
		InodesFree: int64(v.sb.freeInodes),
	}, nil
}