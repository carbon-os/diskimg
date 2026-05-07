// Package xfs implements a read/write XFS filesystem driver (v4 and v5).
// All metadata is big-endian as required by the XFS specification.
// I/O is zero-copy via an io.SectionReader; writes land in a dirty-block
// cache that is flushed to the backing store on Unmount.
package xfs

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
	xfsMagic     = 0x58465342 // "XFSB"
	inodeMagic   = 0x494E     // "IN"
	agfMagic     = 0x58414746 // "XAGF"
	agiMagic     = 0x58414749 // "XAGI"
	xfsBtreeMagic = 0x424D4133 // "BMA3" – bmbt leaf/node v5

	// Superblock feature flags (sb_features_incompat)
	featIncompatFType    = 0x00000001
	featIncompatSparsInodes = 0x00000008

	// Superblock version nibble
	xfsVersionMask = 0x000F
	xfsVersion5    = 5

	// Inode data-fork format codes
	fmtDev     = 0
	fmtLocal   = 1
	fmtExtents = 2
	fmtBtree   = 3

	// Inode mode type bits (same as POSIX)
	modeFmt  = 0xF000
	modeFifo = 0x1000
	modeChr  = 0x2000
	modeDir  = 0x4000
	modeBlk  = 0x6000
	modeReg  = 0x8000
	modeLnk  = 0xA000
	modeSock = 0xC000

	// Directory file-type codes (XFS_DIR3_FT_*)
	ftUnknown = 0
	ftReg     = 1
	ftDir     = 2
	ftChrdev  = 3
	ftBlkdev  = 4
	ftFifo    = 5
	ftSock    = 6
	ftSymlink = 7

	// Well-known inode numbers
	inodeRootDefault = 128 // typical default for 512-byte inodes

	// Sizes
	sbSize  = 512 // superblock occupies first sector of each AG
	agfOff  = 512 // AGF is in the second 512-byte sector of the AG
	agiOff  = 1024 // AGI is in the third 512-byte sector of the AG
)

// ── superblock ────────────────────────────────────────────────────────────────

// superblock holds the decoded primary XFS superblock (AG 0, offset 0).
type superblock struct {
	blockSize   uint32
	dblocks     uint64 // total data blocks
	rblocks     uint64 // real-time blocks (usually 0)
	agBlocks    uint32 // blocks per AG
	agCount     uint32 // number of AGs
	rootIno     uint64 // root directory inode
	logStart    uint64 // journal start block (absolute)
	logBlocks   uint32 // journal length in blocks
	sectSize    uint16 // sector size (usually 512)
	inodeSize   uint16 // inode size in bytes
	inopBlock   uint16 // inodes per block
	version     uint16 // version (low nibble is version number)
	featIncompat uint32
	featROCompat uint32
	agBlkLog    uint8  // log2(agBlocks) rounded up
	inopBLog    uint8  // log2(inopBlock)
	dirBlkLog   uint8  // log2(dir block size in fs blocks)
	uuid        [16]byte
	// v5 only
	hasV5CRC    bool
}

// ── AG free-block info ────────────────────────────────────────────────────────

type agf struct {
	freeBlks  uint32 // total free blocks in this AG
	longest   uint32 // longest free run
}

// ── AG inode info ─────────────────────────────────────────────────────────────

type agi struct {
	count     uint32 // total inodes allocated
	root      uint32 // AG-relative block of inode B+tree root
	level     uint32 // levels in inode B+tree
	freeCount uint32 // free inodes
	newIno    uint32 // most recently allocated AG-relative inode
	dirIno    uint32 // last directory inode (0xFFFFFFFF if none)
	// inode B+tree root stored in agiRoot block
}

// ── inode ────────────────────────────────────────────────────────────────────

// inode is the in-memory representation of an XFS v3 inode core (176 bytes)
// plus the literal-area (data fork + optional attr fork).
type inode struct {
	magic      uint16
	mode       uint16
	version    uint8
	format     uint8   // data fork format
	uid        uint32
	gid        uint32
	nlink      uint32
	projIDLo   uint16
	projIDHi   uint16
	atime      int64  // seconds (v3: nanosecond counter when bigtime)
	atimeNsec  uint32
	mtime      int64
	mtimeNsec  uint32
	ctime      int64
	ctimeNsec  uint32
	btime      int64  // v3 only
	btimeNsec  uint32
	size       int64  // bytes
	nblocks    uint64 // FS blocks used
	nextents   uint32 // number of data extents
	naextents  uint16 // number of attr extents
	forkoff    uint8  // attr fork offset (×8 bytes from end of core)
	aformat    uint8  // attr fork format
	flags      uint32
	gen        uint32
	inum       uint64 // v3: absolute inode number stored in inode

	// raw literal area (data fork starts at offset 0 within it)
	literal [1896]byte // enough for 2048-byte inodes
	litSize int        // actual bytes available (inodeSize - coreSize)
}

// coreSize for v1/v2 is 96 bytes; for v3 (version==3) it is 176 bytes.
func (in *inode) coreSize() int {
	if in.version >= 3 {
		return 176
	}
	return 96
}

// ── extent (bmbt record) ──────────────────────────────────────────────────────

// bmbtRec is a packed 128-bit (16-byte) extent record stored big-endian.
//
// Layout (MSB first):
//   bit 127     : flag (unwritten)
//   bits 126–73 : logical file block offset (54 bits)
//   bits 72–21  : absolute start block (52 bits)
//   bits 20–0   : block count (21 bits)
type bmbtRec struct {
	l0, l1 uint64
}

func parseBmbt(b []byte) bmbtRec {
	be := binary.BigEndian
	return bmbtRec{
		l0: be.Uint64(b[0:8]),
		l1: be.Uint64(b[8:16]),
	}
}

func (r bmbtRec) startOff() uint64 {
	return (r.l0 & 0x7FFFFFFFFFFFFFFF) >> 9
}

func (r bmbtRec) startBlock() uint64 {
	return ((r.l0 & 0x1FF) << 43) | (r.l1 >> 21)
}

func (r bmbtRec) blockCount() uint32 {
	return uint32(r.l1 & 0x1FFFFF)
}

func (r bmbtRec) unwritten() bool {
	return r.l0>>63 != 0
}

// ── B+tree block header (v5, bmbt) ───────────────────────────────────────────

// bmbtBlock is the header of an on-disk bmbt (block-map B+tree) block.
type bmbtBlock struct {
	magic   uint32
	level   uint16
	numrecs uint16
}

const (
	bmbtMagicLeaf = 0x424D4133 // "BMA3" – leaf (v5)
	bmbtMagicNode = 0x424D4133 // same magic for both in v5; level distinguishes
	// v4 uses 0x424D4150 / 0x424D4149
	bmbtMagicLeafV4 = 0x424D4150 // "BMAP"
	bmbtMagicNodeV4 = 0x424D4149 // "BMAI"
)

// bmbtKey is a B+tree index key (logical block number, 8 bytes).
// bmbtPtr is an AG-relative or absolute block pointer (8 bytes).

// ── Volume ────────────────────────────────────────────────────────────────────

// Volume implements fs.Volume for an XFS partition.
type Volume struct {
	mu sync.Mutex

	sr       *io.SectionReader
	wa       io.WriterAt
	srOffset int64

	sb   superblock
	agfs []agf
	agis []agi

	// dirty block cache: absolute block number → blockSize bytes
	dirty map[uint64][]byte

	// dirty inode cache: absolute inode number → raw inode bytes
	dirtyInodes map[uint64][]byte

	castagnoliTable *crc32.Table
}

// Open parses the XFS superblock and AG headers from the given partition bounds
// and returns a ready-to-use Volume.
func Open(ra io.ReaderAt, offset, size int64) (*Volume, error) {
	v := &Volume{
		sr:              io.NewSectionReader(ra, offset, size),
		srOffset:        offset, // store unconditionally (needed for log replay)
		dirty:           make(map[uint64][]byte),
		dirtyInodes:     make(map[uint64][]byte),
		castagnoliTable: crc32.MakeTable(crc32.Castagnoli),
	}
	if wa, ok := ra.(io.WriterAt); ok {
		v.wa = wa
	}
	if err := v.readSuperblock(); err != nil {
		return nil, err
	}
	if err := v.readAGHeaders(); err != nil {
		return nil, err
	}
	if err := v.replayLog(); err != nil {
		// Non-fatal: log replay is best-effort for read access.
		// A failure here means the FS may appear incomplete (dirty log),
		// but we should not block all access.
		_ = err
	}
	return v, nil
}

// Type returns "xfs".
func (v *Volume) Type() fstype.Type { return fstype.XFS }

// ── superblock parsing ────────────────────────────────────────────────────────

func (v *Volume) readSuperblock() error {
	buf := make([]byte, sbSize)
	if _, err := v.sr.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("xfs: read superblock: %w", err)
	}
	be := binary.BigEndian

	magic := be.Uint32(buf[0:4])
	if magic != xfsMagic {
		return fmt.Errorf("xfs: bad magic 0x%08X", magic)
	}

	sb := &v.sb
	sb.blockSize   = be.Uint32(buf[4:8])
	sb.dblocks     = be.Uint64(buf[8:16])
	sb.rblocks     = be.Uint64(buf[16:24])
	// buf[24:32] = rextents (ignored)
	copy(sb.uuid[:], buf[32:48])
	sb.logStart    = be.Uint64(buf[48:56])
	sb.rootIno     = be.Uint64(buf[56:64])
	// buf[64:80] = rbmino, rsumino (real-time, ignored)
	sb.agBlocks    = be.Uint32(buf[84:88])
	sb.agCount     = be.Uint32(buf[88:92])
	// buf[92:96] = rbmblocks
	sb.logBlocks   = be.Uint32(buf[96:100])
	sb.version     = be.Uint16(buf[100:102])
	sb.sectSize    = be.Uint16(buf[102:104])
	sb.inodeSize   = be.Uint16(buf[104:106])
	sb.inopBlock   = be.Uint16(buf[106:108])
	sb.agBlkLog    = buf[124]
	sb.inopBLog    = buf[123]
	sb.dirBlkLog   = buf[192] // sb_dirblklog

	// v5 fields
	if sb.version&xfsVersionMask == xfsVersion5 {
		sb.featIncompat = be.Uint32(buf[216:220])
		sb.featROCompat = be.Uint32(buf[212:216])
		sb.hasV5CRC = true
	}

	if sb.blockSize == 0 {
		return fmt.Errorf("xfs: zero block size")
	}
	if sb.inodeSize == 0 {
		sb.inodeSize = 256
	}
	if sb.inopBlock == 0 && sb.blockSize > 0 && sb.inodeSize > 0 {
		sb.inopBlock = uint16(sb.blockSize / uint32(sb.inodeSize))
	}
	return nil
}

// ── AG header reading ─────────────────────────────────────────────────────────

func (v *Volume) readAGHeaders() error {
	v.agfs = make([]agf, v.sb.agCount)
	v.agis = make([]agi, v.sb.agCount)

	for ag := uint32(0); ag < v.sb.agCount; ag++ {
		agBase := int64(ag) * int64(v.sb.agBlocks) * int64(v.sb.blockSize)

		// AGF: second sector of AG
		agfBuf := make([]byte, v.sb.sectSize)
		if _, err := v.sr.ReadAt(agfBuf, agBase+agfOff); err != nil {
			return fmt.Errorf("xfs: read AGF %d: %w", ag, err)
		}
		be := binary.BigEndian
		if be.Uint32(agfBuf[0:4]) == agfMagic {
			v.agfs[ag].freeBlks = be.Uint32(agfBuf[28:32])
			v.agfs[ag].longest  = be.Uint32(agfBuf[32:36])
		}

		// AGI: third sector of AG
		agiBuf := make([]byte, v.sb.sectSize)
		if _, err := v.sr.ReadAt(agiBuf, agBase+agiOff); err != nil {
			return fmt.Errorf("xfs: read AGI %d: %w", ag, err)
		}
		if be.Uint32(agiBuf[0:4]) == agiMagic {
			v.agis[ag].count     = be.Uint32(agiBuf[16:20])
			v.agis[ag].root      = be.Uint32(agiBuf[20:24])
			v.agis[ag].level     = be.Uint32(agiBuf[24:28])
			v.agis[ag].freeCount = be.Uint32(agiBuf[28:32])
			v.agis[ag].newIno    = be.Uint32(agiBuf[32:36])
			v.agis[ag].dirIno    = be.Uint32(agiBuf[36:40])
		}
	}
	return nil
}

// ── block I/O ─────────────────────────────────────────────────────────────────

// readBlock reads one filesystem block (blockSize bytes).
// Dirty (in-memory) data takes precedence over on-disk data.
func (v *Volume) readBlock(blk uint64) ([]byte, error) {
	if data, ok := v.dirty[blk]; ok {
		cpy := make([]byte, len(data))
		copy(cpy, data)
		return cpy, nil
	}
	buf := make([]byte, v.sb.blockSize)
	off := int64(blk) * int64(v.sb.blockSize)
	if _, err := v.sr.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("xfs: read block %d: %w", blk, err)
	}
	return buf, nil
}

// writeBlock stages a block write into the dirty cache.
func (v *Volume) writeBlock(blk uint64, data []byte) {
	cpy := make([]byte, v.sb.blockSize)
	copy(cpy, data)
	v.dirty[blk] = cpy
}

// ── inode address arithmetic ──────────────────────────────────────────────────

// inodeAG returns the AG number and AG-relative inode index for an absolute
// inode number.
func (v *Volume) inodeAG(ino uint64) (ag uint32, rel uint32) {
	// inopBLog = log2(inopBlock); agBlkLog = log2(agBlocks)
	inoBits := uint(v.sb.inopBLog) + uint(v.sb.agBlkLog)
	ag = uint32(ino >> inoBits)
	rel = uint32(ino & ((1 << inoBits) - 1))
	return
}

// inodePhysBlock returns the absolute block number and in-block offset for ino.
func (v *Volume) inodePhysBlock(ino uint64) (blk uint64, off int64) {
	ag, rel := v.inodeAG(ino)
	agBase := uint64(ag) * uint64(v.sb.agBlocks)
	blkInAG := uint64(rel) / uint64(v.sb.inopBlock)
	offInBlk := (uint64(rel) % uint64(v.sb.inopBlock)) * uint64(v.sb.inodeSize)
	return agBase + blkInAG, int64(offInBlk)
}

// ── inode I/O ─────────────────────────────────────────────────────────────────

// readRawInode reads the raw inode bytes for the given absolute inode number.
func (v *Volume) readRawInode(ino uint64) ([]byte, error) {
	if b, ok := v.dirtyInodes[ino]; ok {
		cpy := make([]byte, len(b))
		copy(cpy, b)
		return cpy, nil
	}
	blk, offInBlk := v.inodePhysBlock(ino)
	blkData, err := v.readBlock(blk)
	if err != nil {
		return nil, fmt.Errorf("xfs: read inode %d: %w", ino, err)
	}
	raw := make([]byte, v.sb.inodeSize)
	copy(raw, blkData[offInBlk:offInBlk+int64(v.sb.inodeSize)])
	return raw, nil
}

// writeRawInode stages raw inode bytes into the dirty cache.
func (v *Volume) writeRawInode(ino uint64, raw []byte) {
	cpy := make([]byte, len(raw))
	copy(cpy, raw)
	v.dirtyInodes[ino] = cpy
}

// readInode reads and parses inode ino.
func (v *Volume) readInode(ino uint64) (inode, error) {
	raw, err := v.readRawInode(ino)
	if err != nil {
		return inode{}, err
	}
	return parseInode(raw, int(v.sb.inodeSize)), nil
}

// writeInode encodes in and stages it for write.
func (v *Volume) writeInode(ino uint64, in *inode) error {
	raw, err := v.readRawInode(ino)
	if err != nil {
		raw = make([]byte, v.sb.inodeSize)
	}
	encodeInode(in, raw)
	v.writeRawInode(ino, raw)
	return nil
}

// ── inode encode/decode ───────────────────────────────────────────────────────

// parseInode decodes an XFS inode from raw bytes.
// XFS metadata is big-endian.
func parseInode(raw []byte, inodeSize int) inode {
	be := binary.BigEndian
	var in inode
	in.magic   = be.Uint16(raw[0:2])
	in.mode    = be.Uint16(raw[2:4])
	in.version = raw[4]
	in.format  = raw[5]
	// raw[6:8] = di_onlink (v1 only)
	in.uid   = be.Uint32(raw[8:12])
	in.gid   = be.Uint32(raw[12:16])
	in.nlink = be.Uint32(raw[16:20])

	if in.version >= 3 {
		// v3 inode core is 176 bytes
		in.atime     = int64(be.Uint32(raw[32:36]))
		in.atimeNsec = be.Uint32(raw[36:40])
		in.mtime     = int64(be.Uint32(raw[40:44]))
		in.mtimeNsec = be.Uint32(raw[44:48])
		in.ctime     = int64(be.Uint32(raw[48:52]))
		in.ctimeNsec = be.Uint32(raw[52:56])
		in.size      = int64(be.Uint64(raw[56:64]))
		in.nblocks   = be.Uint64(raw[64:72])
		// raw[72:76] = extsize hint
		in.nextents  = be.Uint32(raw[76:80])
		in.naextents = be.Uint16(raw[80:82])
		in.forkoff   = raw[82]
		in.aformat   = raw[83]
		// raw[84:88] = dmevmask
		// raw[88:90] = dmstate
		in.flags     = be.Uint32(raw[90:94])
		in.gen        = be.Uint32(raw[94:98])
		// raw[98:100] = next_unlinked
		// raw[100:104] = crc (v5)
		// raw[104:108] = changecount
		// raw[108:116] = lsn
		// raw[116:124] = flags2
		// raw[124:128] = cowextsize
		// raw[128:136] = pad2
		in.btime     = int64(be.Uint32(raw[136:140]))
		in.btimeNsec = be.Uint32(raw[140:144])
		in.inum      = be.Uint64(raw[144:152])
		// raw[152:168] = uuid
		in.litSize = inodeSize - 176
		copy(in.literal[:in.litSize], raw[176:inodeSize])
	} else {
		// v1/v2 core is 96 bytes
		in.atime     = int64(be.Uint32(raw[32:36]))
		in.atimeNsec = be.Uint32(raw[36:40])
		in.mtime     = int64(be.Uint32(raw[40:44]))
		in.mtimeNsec = be.Uint32(raw[44:48])
		in.ctime     = int64(be.Uint32(raw[48:52]))
		in.ctimeNsec = be.Uint32(raw[52:56])
		in.size      = int64(be.Uint64(raw[56:64]))
		in.nblocks   = be.Uint64(raw[64:72])
		in.nextents  = be.Uint32(raw[76:80])
		in.naextents = be.Uint16(raw[80:82])
		in.forkoff   = raw[82]
		in.aformat   = raw[83]
		in.flags     = be.Uint32(raw[90:94])
		in.gen        = be.Uint32(raw[94:98])
		in.litSize = inodeSize - 96
		copy(in.literal[:in.litSize], raw[96:inodeSize])
	}
	return in
}

// encodeInode writes the inode struct back into raw bytes.
func encodeInode(in *inode, raw []byte) {
	be := binary.BigEndian
	be.PutUint16(raw[0:2], in.magic)
	be.PutUint16(raw[2:4], in.mode)
	raw[4] = in.version
	raw[5] = in.format
	be.PutUint32(raw[8:12], in.uid)
	be.PutUint32(raw[12:16], in.gid)
	be.PutUint32(raw[16:20], in.nlink)

	coreEnd := 96
	if in.version >= 3 {
		coreEnd = 176
	}

	be.PutUint32(raw[32:36], uint32(in.atime))
	be.PutUint32(raw[36:40], in.atimeNsec)
	be.PutUint32(raw[40:44], uint32(in.mtime))
	be.PutUint32(raw[44:48], in.mtimeNsec)
	be.PutUint32(raw[48:52], uint32(in.ctime))
	be.PutUint32(raw[52:56], in.ctimeNsec)
	be.PutUint64(raw[56:64], uint64(in.size))
	be.PutUint64(raw[64:72], in.nblocks)
	be.PutUint32(raw[76:80], in.nextents)
	be.PutUint16(raw[80:82], in.naextents)
	raw[82] = in.forkoff
	raw[83] = in.aformat
	be.PutUint32(raw[90:94], in.flags)
	be.PutUint32(raw[94:98], in.gen)

	if in.version >= 3 {
		be.PutUint32(raw[136:140], uint32(in.btime))
		be.PutUint32(raw[140:144], in.btimeNsec)
		be.PutUint64(raw[144:152], in.inum)
	}

	// Write literal area back.
	if in.litSize > 0 {
		copy(raw[coreEnd:coreEnd+in.litSize], in.literal[:in.litSize])
	}
}

// ── time helpers ───────────────────────────────────────────────────────────────

func nowSec() int64 { return time.Now().Unix() }

func xfsToTime(sec int64, nsec uint32) time.Time {
	return time.Unix(sec, int64(nsec))
}

// ── StatFS ────────────────────────────────────────────────────────────────────

func (v *Volume) StatFS() (volfs.VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	bs := int64(v.sb.blockSize)
	total := int64(v.sb.dblocks) * bs
	var free int64
	var freeInodes int64
	var totalInodes int64
	for i := range v.agfs {
		free += int64(v.agfs[i].freeBlks) * bs
		freeInodes += int64(v.agis[i].freeCount)
		totalInodes += int64(v.agis[i].count)
	}
	return volfs.VolumeInfo{
		TotalBytes: total,
		FreeBytes:  free,
		UsedBytes:  total - free,
		BlockSize:  bs,
		Inodes:     totalInodes,
		InodesFree: freeInodes,
	}, nil
}