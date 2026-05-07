package xfs

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// ── on-disk log constants ─────────────────────────────────────────────────────

const (
	xlogMagic      uint32 = 0xFEEDbabe // xlog_rec_header_t h_magicno
	xlogHdrSize           = 512        // log record header is always 512 bytes
	xlogOpHdrSize         = 12         // xlog_op_header_t size
	xlogBlfChunk          = 128        // bytes per data-map bit (XFS_BLF_CHUNK)
	xlogFmtLinuxLE uint32 = 1          // XLOG_FMT_LINUX_LE  (amd64, arm64)
	xlogFmtLinuxBE uint32 = 2          // XLOG_FMT_LINUX_BE

	// Log item types — written in host byte order (LE on all modern Linux arches).
	xliBuffer uint16 = 0x123b // XFS_LI_BUF
	xliInode  uint16 = 0x123e // XFS_LI_INODE

	// xlog_op_header_t oh_flags
	xlogFlagUnmount = 0x20 // XLOG_UNMOUNT_TRANS

	// xfs_inode_log_format ilf_fields bits (XFS_ILOG_*)
	xlogIlogCore  = 0x001
	xlogIlogDData = 0x010
	xlogIlogDExt  = 0x020
	xlogIlogDBRoot = 0x040
)

// ── log record header ─────────────────────────────────────────────────────────

// logRecHdr holds the fields we need from xlog_rec_header_t (512 bytes).
//
// Byte map (all big-endian except h_crc):
//   0   h_magicno   be32
//   4   h_cycle     be32
//   8   h_version   be32
//  12   h_len       be32  — data length in bytes (NOT including this header)
//  16   h_lsn       be64  — LSN of this record
//  24   h_tail_lsn  be64  — oldest LSN still needed for recovery
//  32   h_crc       le32  — CRC32c (v5 only; we skip it)
//  36   h_prev_block be32
//  40   h_num_logops be32
//  44   h_cycle_data[64] be32  — 64×4 = 256 bytes of saved sector-first-words
// 300   h_fmt       be32  — XLOG_FMT_LINUX_LE / LINUX_BE
// 304   h_fs_uuid   [16]byte
// 320   h_size      be32
type logRecHdr struct {
	lsn     uint64
	tailLSN uint64
	dataLen uint32
	numOps  uint32
	fmt     uint32
}

func parseLogRecHdr(buf []byte) logRecHdr {
	be := binary.BigEndian
	return logRecHdr{
		lsn:     be.Uint64(buf[16:24]),
		tailLSN: be.Uint64(buf[24:32]),
		dataLen: be.Uint32(buf[12:16]),
		numOps:  be.Uint32(buf[40:44]),
		fmt:     be.Uint32(buf[300:304]),
	}
}

// ── LSN helpers ───────────────────────────────────────────────────────────────

// LSN layout: cycle (upper 32 bits) | block-offset in 512-byte BBs (lower 32).

func lsnLess(a, b uint64) bool {
	ac, ao := uint32(a>>32), uint32(a)
	bc, bo := uint32(b>>32), uint32(b)
	if ac != bc {
		return ac < bc
	}
	return ao < bo
}

// ── cycle-data restoration ────────────────────────────────────────────────────

// restoreSectors repairs the cycle-number mangling XFS applies to the first
// 4 bytes of every 512-byte sector inside a log record's data area.
// Those 4 bytes are saved verbatim in h_cycle_data[i] (header offset 44+i*4).
func restoreSectors(hdr, data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	be := binary.BigEndian
	for i := 0; i*512 < len(out); i++ {
		cdOff := 44 + i*4 // h_cycle_data[i] inside the 512-byte header
		if cdOff+4 > len(hdr) {
			break // only 64 entries fit (covers 32 KB of data)
		}
		be.PutUint32(out[i*512:], be.Uint32(hdr[cdOff:]))
	}
	return out
}

// ── op cursor ─────────────────────────────────────────────────────────────────

// opCursor walks xlog_op_header_t entries inside one log record's data buffer.
type opCursor struct {
	data []byte
	pos  int
	rem  int  // ops remaining
	le   bool // true = Linux LE item format
}

func newOpCursor(data []byte, numOps int, le bool) *opCursor {
	return &opCursor{data: data, rem: numOps, le: le}
}

// next reads the next op header and returns its payload slice and flags byte.
// Returns (nil, 0, false) when exhausted.
func (c *opCursor) next() (payload []byte, flags byte, ok bool) {
	if c.rem == 0 || c.pos+xlogOpHdrSize > len(c.data) {
		return nil, 0, false
	}
	be := binary.BigEndian
	opLen := int(be.Uint32(c.data[c.pos+4:]))
	flags = c.data[c.pos+9]
	c.pos += xlogOpHdrSize
	c.rem--

	end := c.pos + opLen
	if opLen > 0 && end <= len(c.data) {
		payload = c.data[c.pos:end]
		c.pos = end
	} else {
		// Clamp to avoid a panic on partial/torn records at the log boundary.
		if c.pos < len(c.data) {
			payload = c.data[c.pos:]
		}
		c.pos = len(c.data)
	}
	return payload, flags, true
}

// itemType reads the two-byte log item type from a format-header payload.
func itemType(payload []byte, le bool) uint16 {
	if len(payload) < 2 {
		return 0
	}
	if le {
		return binary.LittleEndian.Uint16(payload[:2])
	}
	return binary.BigEndian.Uint16(payload[:2])
}

// ── main entry point ──────────────────────────────────────────────────────────

// replayLog reads the XFS internal log and applies any pending buffer and inode
// log items into v.dirty / v.dirtyInodes so that subsequent reads see a
// consistent on-disk state without requiring a live kernel mount.
//
// This handles the common case of cloud images whose qcow2 was snapshotted
// without a clean unmount, leaving a dirty log whose tail ≠ head.
func (v *Volume) replayLog() error {
	if v.sb.logStart == 0 || v.sb.logBlocks == 0 {
		return nil
	}

	logOff := int64(v.sb.logStart) * int64(v.sb.blockSize)
	logLen := int64(v.sb.logBlocks) * int64(v.sb.blockSize)

	logBuf := make([]byte, logLen)
	if _, err := v.sr.ReadAt(logBuf, logOff); err != nil {
		return fmt.Errorf("xfs: replayLog: read log: %w", err)
	}

	be := binary.BigEndian

	// ── 1. Collect all valid log record headers. ──────────────────────────────
	type recEntry struct {
		off int
		hdr logRecHdr
	}
	var recs []recEntry

	for off := 0; off+xlogHdrSize <= len(logBuf); off += 512 {
		if be.Uint32(logBuf[off:]) != xlogMagic {
			continue
		}
		recs = append(recs, recEntry{off: off, hdr: parseLogRecHdr(logBuf[off:])})
	}
	if len(recs) == 0 {
		return nil // fresh / empty log
	}

	// ── 2. Identify the head (highest LSN) and read tail from it. ─────────────
	headIdx := 0
	for i := 1; i < len(recs); i++ {
		if lsnLess(recs[headIdx].hdr.lsn, recs[i].hdr.lsn) {
			headIdx = i
		}
	}
	head := recs[headIdx]

	// If tail == head the log is already clean.
	if head.hdr.tailLSN == head.hdr.lsn {
		return nil
	}
	tailLSN := head.hdr.tailLSN
	logFmt := head.hdr.fmt

	// ── 3. Sort records by LSN so we apply them in commit order. ─────────────
	sort.Slice(recs, func(i, j int) bool {
		return lsnLess(recs[i].hdr.lsn, recs[j].hdr.lsn)
	})

	// ── 4. Apply records in the range [tailLSN, headLSN]. ────────────────────
	for _, rec := range recs {
		if lsnLess(rec.hdr.lsn, tailLSN) {
			continue
		}
		if rec.hdr.numOps == 0 || rec.hdr.dataLen == 0 {
			continue
		}
		dataStart := rec.off + xlogHdrSize
		dataEnd := dataStart + int(rec.hdr.dataLen)
		if dataEnd > len(logBuf) {
			dataEnd = len(logBuf)
		}
		if dataEnd <= dataStart {
			continue
		}
		// Restore the 4 bytes per sector that XFS overwrote with the cycle number.
		recData := restoreSectors(logBuf[rec.off:rec.off+xlogHdrSize], logBuf[dataStart:dataEnd])
		v.applyLogOps(recData, int(rec.hdr.numOps), logFmt)
	}
	return nil
}

// ── op dispatcher ─────────────────────────────────────────────────────────────

// applyLogOps walks all op headers in one record's restored data buffer
// and dispatches buffer / inode log items.
func (v *Volume) applyLogOps(data []byte, numOps int, logFmt uint32) {
	le := logFmt == xlogFmtLinuxLE || logFmt == 0 // 0 = IRIX (treat as LE)
	cur := newOpCursor(data, numOps, le)

	for {
		payload, flags, ok := cur.next()
		if !ok {
			return
		}
		if flags&xlogFlagUnmount != 0 {
			return // clean unmount marker — nothing to replay past here
		}
		if len(payload) < 2 {
			continue
		}
		switch itemType(payload, le) {
		case xliBuffer:
			v.applyBufItem(cur, payload, le)
		case xliInode:
			v.applyInodeItem(cur, payload, le)
		}
	}
}

// ── buffer log item ───────────────────────────────────────────────────────────

// applyBufItem handles an XFS_LI_BUF log item.
//
// xfs_buf_log_format layout (host byte order = LE on Linux):
//  [0:2]   blf_type    (0x123b)
//  [2:4]   blf_size    — total number of ops for this item (incl. format op)
//  [4:8]   blf_flags
//  [8:16]  blf_blkno   — starting device block, in 512-byte sectors
//  [16:18] blf_len     — length in 512-byte sectors
//  [18:20] blf_map_size — number of uint32s in the dirty-chunk bitmap
//  [20:…]  blf_data_map[blf_map_size]
//
// The blf_size-1 ops that follow contain the dirty 128-byte chunks in bitmap order.
func (v *Volume) applyBufItem(cur *opCursor, fmtPayload []byte, le bool) {
	if len(fmtPayload) < 20 {
		return
	}

	var (
		blfSize    uint16
		blfBlkno   int64
		blfLen     uint16
		blfMapSize uint16
	)
	if le {
		blfSize    = binary.LittleEndian.Uint16(fmtPayload[2:4])
		blfBlkno   = int64(binary.LittleEndian.Uint64(fmtPayload[8:16]))
		blfLen     = binary.LittleEndian.Uint16(fmtPayload[16:18])
		blfMapSize = binary.LittleEndian.Uint16(fmtPayload[18:20])
	} else {
		blfSize    = binary.BigEndian.Uint16(fmtPayload[2:4])
		blfBlkno   = int64(binary.BigEndian.Uint64(fmtPayload[8:16]))
		blfLen     = binary.BigEndian.Uint16(fmtPayload[16:18])
		blfMapSize = binary.BigEndian.Uint16(fmtPayload[18:20])
	}

	if blfSize < 1 || blfLen == 0 || blfMapSize == 0 {
		return
	}

	mapBytes := int(blfMapSize) * 4
	if 20+mapBytes > len(fmtPayload) {
		return
	}

	// Read the dirty-chunk bitmap.
	dataMap := make([]uint32, blfMapSize)
	for i := range dataMap {
		off := 20 + i*4
		if le {
			dataMap[i] = binary.LittleEndian.Uint32(fmtPayload[off:])
		} else {
			dataMap[i] = binary.BigEndian.Uint32(fmtPayload[off:])
		}
	}

	// Read current on-disk buffer data.
	partOff   := blfBlkno * 512          // byte offset within the partition SectionReader
	totalBytes := int64(blfLen) * 512
	if partOff < 0 || totalBytes <= 0 || totalBytes > 64*1024*1024 {
		return // sanity guard
	}
	bufData := make([]byte, totalBytes)
	if _, err := v.sr.ReadAt(bufData, partOff); err != nil {
		return
	}

	// Collect all data payloads from the blfSize-1 following ops.
	var dataBytes []byte
	for i := 1; i < int(blfSize); i++ {
		p, _, ok := cur.next()
		if !ok {
			break
		}
		dataBytes = append(dataBytes, p...)
	}

	// Overlay dirty 128-byte chunks in bitmap order.
	dataPos := 0
	for wordIdx := 0; wordIdx < int(blfMapSize) && dataPos < len(dataBytes); wordIdx++ {
		for bitIdx := uint(0); bitIdx < 32 && dataPos < len(dataBytes); bitIdx++ {
			if dataMap[wordIdx]&(1<<bitIdx) == 0 {
				continue
			}
			chunkNum := wordIdx*32 + int(bitIdx)
			bufOff   := int64(chunkNum) * xlogBlfChunk
			if bufOff+xlogBlfChunk > totalBytes {
				break
			}
			end := dataPos + xlogBlfChunk
			if end > len(dataBytes) {
				end = len(dataBytes)
			}
			copy(bufData[bufOff:], dataBytes[dataPos:end])
			dataPos += xlogBlfChunk
		}
	}

	// Write each filesystem block of the recovered buffer into the dirty cache.
	bs          := int64(v.sb.blockSize)
	startFSBlk  := partOff / bs
	numFSBlks   := (totalBytes + bs - 1) / bs

	for i := int64(0); i < numFSBlks; i++ {
		blkData := make([]byte, bs)
		srcOff := i * bs
		srcEnd := srcOff + bs
		if srcEnd > totalBytes {
			srcEnd = totalBytes
		}
		copy(blkData, bufData[srcOff:srcEnd])
		v.writeBlock(uint64(startFSBlk+i), blkData)
	}
}

// ── inode log item ────────────────────────────────────────────────────────────

// applyInodeItem handles an XFS_LI_INODE log item.
//
// xfs_inode_log_format layout (host byte order = LE on Linux):
//  [0:2]   ilf_type    (0x123e)
//  [2:4]   ilf_size    — total ops for this item (incl. format op)
//  [4:8]   ilf_fields  — XFS_ILOG_CORE | XFS_ILOG_DEXT | …
//  [8:10]  ilf_asize
//  [10:12] ilf_dsize
//  [12:16] ilf_pad
//  [16:24] ilf_ino     — absolute inode number
//  [24:40] ilf_u       (rdev / uuid)
//  [40:48] ilf_blkno   — inode's device block (512-byte sectors)
//  [48:52] ilf_len     — in 512-byte sectors
//  [52:56] ilf_boffset — byte offset of inode within that buffer
//
// Ops after the format header arrive in field order:
//   1. XFS_ILOG_CORE  → raw inode core (176 bytes for v3, 96 for v1/v2)
//   2. XFS_ILOG_DEXT / XFS_ILOG_DDATA / XFS_ILOG_DBROOT → data fork literal area
//   (attr fork ops are ignored for read-only recovery)
func (v *Volume) applyInodeItem(cur *opCursor, fmtPayload []byte, le bool) {
	if len(fmtPayload) < 56 {
		return
	}

	var (
		ilfSize   uint16
		ilfFields uint32
		ilfIno    uint64
	)
	if le {
		ilfSize   = binary.LittleEndian.Uint16(fmtPayload[2:4])
		ilfFields = binary.LittleEndian.Uint32(fmtPayload[4:8])
		ilfIno    = binary.LittleEndian.Uint64(fmtPayload[16:24])
	} else {
		ilfSize   = binary.BigEndian.Uint16(fmtPayload[2:4])
		ilfFields = binary.BigEndian.Uint32(fmtPayload[4:8])
		ilfIno    = binary.BigEndian.Uint64(fmtPayload[16:24])
	}

	if ilfIno == 0 || ilfSize < 2 {
		return
	}

	// Fetch current raw inode bytes (may be in dirty cache already).
	raw, err := v.readRawInode(ilfIno)
	if err != nil {
		raw = make([]byte, v.sb.inodeSize)
	}

	// Detect inode version from the on-disk core to pick the right core size.
	coreSize := 176 // v3 (modern XFS)
	if len(raw) > 4 && raw[4] < 3 {
		coreSize = 96 // v1/v2
	}

	opNum := 1 // ops consumed so far (format header was op 0)

	// ── Core region (always first if present) ────────────────────────────────
	if ilfFields&xlogIlogCore != 0 && opNum < int(ilfSize) {
		p, _, ok := cur.next()
		opNum++
		if ok && len(p) >= coreSize {
			copy(raw[:coreSize], p[:coreSize])
			// Re-detect core size from the freshly applied bytes.
			if raw[4] < 3 {
				coreSize = 96
			} else {
				coreSize = 176
			}
		}
	}

	// ── Data fork region (extents / inline data / btree root) ────────────────
	dataForkMask := uint32(xlogIlogDData | xlogIlogDExt | xlogIlogDBRoot)
	if ilfFields&dataForkMask != 0 && opNum < int(ilfSize) {
		p, _, ok := cur.next()
		opNum++
		if ok && len(p) > 0 {
			litStart := coreSize
			n := len(p)
			if litStart+n > len(raw) {
				n = len(raw) - litStart
			}
			if n > 0 {
				copy(raw[litStart:litStart+n], p[:n])
			}
		}
	}

	// Drain any remaining ops for this item (attr fork, etc.) so the cursor
	// stays aligned for the next item.
	for opNum < int(ilfSize) {
		cur.next()
		opNum++
	}

	v.writeRawInode(ilfIno, raw)
}