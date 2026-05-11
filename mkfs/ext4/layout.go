package ext4

import (
	"math"
)

const (
	blockSize      = 4096
	inodeSize      = 256
	blocksPerGroup = 32768 // 8 * blockSize bits in one bitmap block
	gdtEntrySize   = 64    // 64-bit group descriptor
	flexGroupLog   = 4     // s_log_groups_per_flex; flex size = 2^4 = 16
	flexGroupSize  = 1 << flexGroupLog

	// Reserved inode numbers (ext4 standard)
	inoBad       = 1
	inoRoot      = 2
	inoUserQuota = 3
	inoGrpQuota  = 4
	inoBoot      = 5
	inoUndel     = 6
	inoResv7     = 7
	inoJournal   = 8
	inoExclude   = 9
	inoReplica   = 10
	inoLostFound = 11 // first non-reserved inode; mkfs puts lost+found here
)

type groupLayout struct {
	groupNum         uint32
	firstBlock       uint64
	lastBlock        uint64 // inclusive
	hasSB            bool   // carries SB + GDT backup
	blockBitmap      uint64 // block number of block bitmap
	inodeBitmap      uint64 // block number of inode bitmap
	inodeTable       uint64 // first block of inode table
	inodeTableBlocks uint32
	dataStart        uint64 // first usable data block (mutable during layout)
	freeBlocks       uint32
	freeInodes       uint32
	usedDirs         uint16
}

type fsLayout struct {
	opts         *Options
	now          int64
	totalBlocks  uint64
	totalInodes  uint32
	numGroups    uint32
	inodesPerGrp uint32
	gdtBlocks    uint64 // GDT size in blocks (same for all groups)
	groups       []groupLayout

	// Allocated block addresses
	journalBlock   uint64
	journalSize    uint32
	rootDirBlock   uint64
	lfDirBlock     uint64 // lost+found data block

	// crc32c(^0, uuid) — seed for all metadata checksums
	csumSeed uint32
}

func computeLayout(sizeBytes int64, opts *Options) (*fsLayout, error) {
	totalBlocks := uint64(sizeBytes) / blockSize
	numGroups := uint32((totalBlocks + blocksPerGroup - 1) / blocksPerGroup)

	// Inode count
	rawTotalInodes := sizeBytes / opts.InodeRatio
	inodesPerGrp := uint32((rawTotalInodes + int64(numGroups) - 1) / int64(numGroups))
	
	// Round up to a multiple of (blockSize/inodeSize) so inode table is block-aligned.
	alignInodes := uint32(blockSize / inodeSize) // 16
	inodesPerGrp = ((inodesPerGrp + alignInodes - 1) / alignInodes) * alignInodes
	if inodesPerGrp < alignInodes {
		inodesPerGrp = alignInodes
	}
	if inodesPerGrp > blocksPerGroup {
		inodesPerGrp = blocksPerGroup
	}
	totalInodes := inodesPerGrp * numGroups

	// GDT size
	gdtBlocks := (uint64(numGroups)*gdtEntrySize + blockSize - 1) / blockSize

	// Reserved GDT: allow filesystem to grow by up to 1024×.
	// Reserve enough blocks to hold GDT for (numGroups * 1024) groups, minus current.
	maxGroups := uint64(numGroups) * 1024
	if maxGroups > math.MaxUint32 {
		maxGroups = math.MaxUint32
	}
	maxGDTBlocks := (maxGroups*gdtEntrySize + blockSize - 1) / blockSize
	reservedGDT := uint32(0)
	if maxGDTBlocks > gdtBlocks {
		reservedGDT = uint32(maxGDTBlocks - gdtBlocks)
	}
	if reservedGDT > 1024 {
		reservedGDT = 1024
	}
	opts.reservedGDT = reservedGDT

	csumSeed := crc32cUpdate(^uint32(0), opts.UUID[:])

	fs := &fsLayout{
		opts:         opts,
		now:          timeNow(),
		totalBlocks:  totalBlocks,
		totalInodes:  totalInodes,
		numGroups:    numGroups,
		inodesPerGrp: inodesPerGrp,
		gdtBlocks:    gdtBlocks,
		csumSeed:     csumSeed,
	}
	fs.groups = make([]groupLayout, numGroups)

	inodeTableBlocks := (uint64(inodesPerGrp) * inodeSize + blockSize - 1) / blockSize

	for g := uint32(0); g < numGroups; g++ {
		first := uint64(g) * blocksPerGroup
		last := first + blocksPerGroup - 1
		if last >= totalBlocks {
			last = totalBlocks - 1
		}
		groupBlocks := last - first + 1

		gl := groupLayout{
			groupNum:         g,
			firstBlock:       first,
			lastBlock:        last,
			hasSB:            hasSuperBackup(g),
			inodeTableBlocks: uint32(inodeTableBlocks),
		}

		cur := first
		if gl.hasSB {
			cur++                      // SB block (or padding+SB in group 0)
			cur += gdtBlocks           // GDT
			cur += uint64(reservedGDT) // reserved GDT expansion space
		}
		gl.blockBitmap = cur; cur++
		gl.inodeBitmap = cur; cur++
		gl.inodeTable = cur
		cur += inodeTableBlocks
		gl.dataStart = cur

		used := cur - first
		if groupBlocks > used {
			gl.freeBlocks = uint32(groupBlocks - used)
		}
		gl.freeInodes = inodesPerGrp
		fs.groups[g] = gl
	}

	// ── Journal: placed in group 0 data area ───────────────────────────────
	jBlocks := uint32(32768)
	if uint64(jBlocks) > totalBlocks/4 {
		jBlocks = uint32(totalBlocks / 4)
	}
	if jBlocks < 1024 {
		jBlocks = 1024
	}

	// Clamp the journal size to fit within Group 0's free blocks without spilling.
	// We subtract 2 to leave space for the root dir and lost+found.
	var maxJBlocks uint32
	if fs.groups[0].freeBlocks > 2 {
		maxJBlocks = fs.groups[0].freeBlocks - 2
	}
	if jBlocks > maxJBlocks {
		jBlocks = maxJBlocks
	}

	fs.journalSize = jBlocks
	fs.journalBlock = fs.groups[0].dataStart
	fs.groups[0].dataStart += uint64(jBlocks)
	fs.groups[0].freeBlocks -= jBlocks

	// ── Root directory data block ──────────────────────────────────────────
	fs.rootDirBlock = fs.groups[0].dataStart
	fs.groups[0].dataStart++
	fs.groups[0].freeBlocks--
	fs.groups[0].usedDirs++

	// ── lost+found data block ──────────────────────────────────────────────
	fs.lfDirBlock = fs.groups[0].dataStart
	fs.groups[0].dataStart++
	fs.groups[0].freeBlocks--
	fs.groups[0].usedDirs++

	// Group 0: inodes 1–inoLostFound are used (11 inodes)
	fs.groups[0].freeInodes = inodesPerGrp - uint32(inoLostFound)

	return fs, nil
}

// hasSuperBackup reports whether group g holds a superblock backup.
// Groups 0, 1, and those whose number is an exact power of 3, 5, or 7.
func hasSuperBackup(g uint32) bool {
	if g <= 1 {
		return true
	}
	for _, b := range [3]uint32{3, 5, 7} {
		x := b
		for x < g {
			x *= b
		}
		if x == g {
			return true
		}
	}
	return false
}