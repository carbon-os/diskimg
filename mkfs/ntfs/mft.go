// mft.go
package ntfs

import "time"

// ── Attribute type codes ──────────────────────────────────────────────────────

const (
	attrStandardInfo uint32 = 0x10
	attrFileName     uint32 = 0x30
	attrVolumeName   uint32 = 0x60
	attrVolumeInfo   uint32 = 0x70
	attrData         uint32 = 0x80
	attrIndexRoot    uint32 = 0x90
	attrBitmap       uint32 = 0xB0
	attrEnd          uint32 = 0xFFFFFFFF
)

// ── MFT record flags ─────────────────────────────────────────────────────────

const (
	mftInUse = uint16(0x0001)
	mftDir   = uint16(0x0002)
)

// ── File attribute flags (used in $STANDARD_INFORMATION and $FILE_NAME) ──────

const (
	faReadOnly = uint32(0x0001)
	faHidden   = uint32(0x0002)
	faSystem   = uint32(0x0004)
	faArchive  = uint32(0x0020)
)

// fileRef encodes an NTFS file reference: lower 48 bits = record number,
// upper 16 bits = sequence number.
func fileRef(recNum uint32, seqNum uint16) uint64 {
	return uint64(recNum) | uint64(seqNum)<<48
}

// ── mftRecord ─────────────────────────────────────────────────────────────────

// mftRecord builds a single 1024-byte NTFS 3.1 FILE record in memory.
// Attributes are appended between newMFTRecord and finalize.
type mftRecord struct {
	buf        []byte // 1024 bytes
	cursor     int    // write offset for next attribute (starts at 0x38)
	nextAttrID uint16
	recNum     uint32
	seqNum     uint16
	linkCount  uint16
	flags      uint16
}

func newMFTRecord(recNum uint32, seqNum, linkCount, flags uint16) *mftRecord {
	return &mftRecord{
		buf:       make([]byte, mftRecordSize),
		cursor:    0x38, // first attribute at offset 56
		recNum:    recNum,
		seqNum:    seqNum,
		linkCount: linkCount,
		flags:     flags,
	}
}

// appendResident appends an unnamed resident attribute. content is padded to 8 bytes.
func (r *mftRecord) appendResident(attrType uint32, content []byte) {
	padded := (len(content) + 7) &^ 7
	total := 0x18 + padded // 24-byte resident header + padded content

	attr := make([]byte, total)
	putU32(attr[0x00:], attrType)
	putU32(attr[0x04:], uint32(total))
	// attr[0x08] = 0: resident flag
	// attr[0x09] = 0: no name
	putU16(attr[0x0A:], 0x18) // name offset (conventionally points at content when no name)
	// attr[0x0C:0x0E] flags = 0
	putU16(attr[0x0E:], r.nextAttrID)
	putU32(attr[0x10:], uint32(len(content))) // content length (before padding)
	putU16(attr[0x14:], 0x18)                 // content offset from attr start
	copy(attr[0x18:], content)

	copy(r.buf[r.cursor:], attr)
	r.cursor += total
	r.nextAttrID++
}

// appendResidentNamed appends a named resident attribute.
// The attribute name (e.g. "$I30") is encoded as UTF-16LE and placed between
// the fixed header and the content, per the NTFS attribute header layout:
//
//	[0x00–0x17]  24-byte resident attribute header
//	[0x18–...]   attribute name (UTF-16LE, padded to 8 bytes)
//	[name_end–]  content (padded to 8 bytes)
//
// This is required for $INDEX_ROOT and related index attributes on directories,
// which must carry the name "$I30" so that NTFS drivers can locate them.
func (r *mftRecord) appendResidentNamed(attrType uint32, name string, content []byte) {
	nameBytes  := toUTF16LE(name)
	namePad    := (len(nameBytes) + 7) &^ 7
	contentPad := (len(content) + 7) &^ 7
	nameOff    := 0x18                 // name starts immediately after 24-byte header
	contentOff := nameOff + namePad    // content follows padded name
	total      := contentOff + contentPad

	attr := make([]byte, total)
	putU32(attr[0x00:], attrType)
	putU32(attr[0x04:], uint32(total))
	// attr[0x08] = 0: resident flag
	attr[0x09] = byte(len(nameBytes) / 2)   // name length in UTF-16 code units
	putU16(attr[0x0A:], uint16(nameOff))    // offset to name from start of attribute
	// attr[0x0C:0x0E] flags = 0
	putU16(attr[0x0E:], r.nextAttrID)
	putU32(attr[0x10:], uint32(len(content))) // content length (before padding)
	putU16(attr[0x14:], uint16(contentOff))   // offset to content from start of attribute
	copy(attr[nameOff:], nameBytes)
	copy(attr[contentOff:], content)

	copy(r.buf[r.cursor:], attr)
	r.cursor += total
	r.nextAttrID++
}

// appendNonResident appends a non-resident attribute with a single contiguous run.
// dataSize is the logical data size; allocSize is rounded to whole clusters.
func (r *mftRecord) appendNonResident(attrType uint32,
	startLCN, numClusters, clusterSize, dataSize int64) {

	rl := encodeRunList(startLCN, numClusters)
	rlPad := (len(rl) + 7) &^ 7
	total := 0x40 + rlPad // 64-byte non-resident header + padded run list
	allocSize := numClusters * clusterSize

	attr := make([]byte, total)
	putU32(attr[0x00:], attrType)
	putU32(attr[0x04:], uint32(total))
	attr[0x08] = 1 // non-resident flag
	// attr[0x09] = 0: no name
	putU16(attr[0x0A:], 0x40) // name offset (= run list offset when no name)
	putU16(attr[0x0E:], r.nextAttrID)
	// starting VCN = 0
	putU64(attr[0x18:], uint64(numClusters-1)) // ending VCN
	putU16(attr[0x20:], 0x40)                  // run list offset from attr start
	// compression unit = 0; padding 4 bytes
	putU64(attr[0x28:], uint64(allocSize)) // allocated size
	putU64(attr[0x30:], uint64(dataSize))  // data size
	putU64(attr[0x38:], uint64(dataSize))  // initialized size
	copy(attr[0x40:], rl)

	copy(r.buf[r.cursor:], attr)
	r.cursor += total
	r.nextAttrID++
}

// finalize writes the FILE record header, applies the Update Sequence Array,
// and returns the 1024-byte record buffer.
//
// NTFS 3.1 FILE record header layout:
//
//	0x00  4  magic "FILE"
//	0x04  2  USA offset = 0x30
//	0x06  2  USA count  = 3 (seq + 2 sector-end slots for 1024-byte record)
//	0x08  8  $LogFile sequence number (0 for fresh records)
//	0x10  2  sequence number
//	0x12  2  link count
//	0x14  2  offset to first attribute = 0x38
//	0x16  2  flags
//	0x18  4  used size of record
//	0x1C  4  allocated size = 1024
//	0x20  8  base file reference (0 = base record)
//	0x28  2  next attribute identifier
//	0x2A  2  padding (NTFS 3.1)
//	0x2C  4  MFT record number (NTFS 3.1)
//	0x30  2  USA[0] check value
//	0x32  2  USA[1] saved last 2 bytes of sector 1 (buf[510:512])
//	0x34  2  USA[2] saved last 2 bytes of sector 2 (buf[1022:1024])
func (r *mftRecord) finalize() []byte {
	// End-of-record marker
	putU32(r.buf[r.cursor:], attrEnd)
	usedSize := r.cursor + 4

	// Header
	copy(r.buf[0:4], "FILE")
	putU16(r.buf[0x04:], 0x30) // USA offset
	putU16(r.buf[0x06:], 3)    // USA count: 1 check + 2 sectors
	putU64(r.buf[0x08:], 0)    // LSN
	putU16(r.buf[0x10:], r.seqNum)
	putU16(r.buf[0x12:], r.linkCount)
	putU16(r.buf[0x14:], 0x38) // first attribute offset
	putU16(r.buf[0x16:], r.flags)
	putU32(r.buf[0x18:], uint32(usedSize))
	putU32(r.buf[0x1C:], uint32(mftRecordSize))
	putU64(r.buf[0x20:], 0) // base record = 0
	putU16(r.buf[0x28:], r.nextAttrID)
	// 0x2A padding = 0
	putU32(r.buf[0x2C:], r.recNum)

	// Update Sequence Array — protect sector boundaries.
	// Check value = 1; save the natural last 2 bytes of each 512-byte sector,
	// then overwrite them with the check value so readers can verify integrity.
	checkVal := uint16(1)
	usa1 := uint16(r.buf[510]) | uint16(r.buf[511])<<8
	usa2 := uint16(r.buf[1022]) | uint16(r.buf[1023])<<8
	putU16(r.buf[0x30:], checkVal)
	putU16(r.buf[0x32:], usa1)
	putU16(r.buf[0x34:], usa2)
	putU16(r.buf[510:], checkVal)
	putU16(r.buf[1022:], checkVal)

	return r.buf
}

// ── Attribute content builders ────────────────────────────────────────────────

// buildStdInfo returns a 72-byte $STANDARD_INFORMATION (NTFS 3.1).
func buildStdInfo(t time.Time, fileAttrs uint32) []byte {
	ft := windowsFiletime(t)
	si := make([]byte, 72)
	putU64(si[0x00:], ft) // creation time
	putU64(si[0x08:], ft) // last data-modified time
	putU64(si[0x10:], ft) // MFT-modified time
	putU64(si[0x18:], ft) // last access time
	putU32(si[0x20:], fileAttrs)
	// max_versions, version, class_id, owner_id, security_id, quota, USN = 0
	return si
}

// buildFileName returns the $FILE_NAME attribute content.
// namespace 3 = Win32+DOS (suitable for all-ASCII system file names).
func buildFileName(parentRef uint64, t time.Time,
	allocSize, dataSize int64, fileAttrs uint32, name string) []byte {

	nameUTF16 := toUTF16LE(name)
	numChars := len(nameUTF16) / 2
	fn := make([]byte, 0x42+len(nameUTF16))
	ft := windowsFiletime(t)
	putU64(fn[0x00:], parentRef)
	putU64(fn[0x08:], ft)
	putU64(fn[0x10:], ft)
	putU64(fn[0x18:], ft)
	putU64(fn[0x20:], ft)
	putU64(fn[0x28:], uint64(allocSize)) // allocated size
	putU64(fn[0x30:], uint64(dataSize))  // real size
	putU32(fn[0x38:], fileAttrs)
	fn[0x40] = byte(numChars) // filename length in UTF-16 code units
	fn[0x41] = 3              // namespace: Win32+DOS
	copy(fn[0x42:], nameUTF16)
	return fn
}

// buildVolumeInfoAttr returns a 12-byte $VOLUME_INFORMATION (NTFS version 3.1).
func buildVolumeInfoAttr() []byte {
	vi := make([]byte, 12)
	// Bytes 0-7: reserved (zero)
	vi[8] = 3 // major version
	vi[9] = 1 // minor version
	// Bytes 10-11: flags (zero = clean, no upgrade needed)
	return vi
}

// buildIndexRoot returns the $INDEX_ROOT content for an empty directory.
//
// Structure:
//
//	[0x00] INDEX_ROOT_HEADER (16 bytes)
//	[0x10] INDEX_HEADER      (16 bytes)
//	[0x20] End INDEX_ENTRY   (16 bytes)  ← only entry: the end/last marker
//
// Note: this content is always written via appendResidentNamed with name "$I30"
// so that NTFS drivers can locate the directory's filename index.
func buildIndexRoot(indexAllocSize int64, clustersPerIdx uint8) []byte {
	ir := make([]byte, 48)

	// INDEX_ROOT_HEADER
	putU32(ir[0x00:], 0x30) // attribute type indexed = $FILE_NAME
	putU32(ir[0x04:], 1)    // collation rule = COLLATION_FILE_NAME
	putU32(ir[0x08:], uint32(indexAllocSize))
	ir[0x0C] = clustersPerIdx

	// INDEX_HEADER at 0x10
	putU32(ir[0x10:], 16) // entries_offset: first entry is 16 bytes past INDEX_HEADER start
	putU32(ir[0x14:], 32) // index_length: INDEX_HEADER (16) + end entry (16)
	putU32(ir[0x18:], 32) // allocated_size = index_length (no overflow)
	// ir[0x1C] = 0: flags — small dir, no $INDEX_ALLOCATION

	// End INDEX_ENTRY at 0x20
	// file_reference = 0 (no file)
	putU16(ir[0x28:], 16) // entry_length = 16 (header only, no key)
	// key_length = 0
	putU16(ir[0x2C:], 2) // flags = INDEX_ENTRY_LAST (0x02)
	return ir
}