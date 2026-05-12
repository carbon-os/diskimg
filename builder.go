package diskimg

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"unicode/utf16"

	diskfs "github.com/carbon-os/diskimg/fs"
)

// ── Well-known GPT partition type GUIDs ───────────────────────────────────────

// GUID_EFISystem is the GPT type GUID for EFI System Partitions.
var GUID_EFISystem = mustParseGPTGUID("C12A7328-F81F-11D2-BA4B-00A0C93EC93B")

// GUID_BasicData is the GPT type GUID for Microsoft Basic Data partitions.
var GUID_BasicData = mustParseGPTGUID("EBD0A0A2-B9E5-4433-87C0-68B6B72699C7")

// GUID_LinuxData is the GPT type GUID for Linux filesystem data.
var GUID_LinuxData = mustParseGPTGUID("0FC63DAF-8483-4772-8E79-3D69D8477DE4")

// ── BuilderPartition ──────────────────────────────────────────────────────────

// BuilderPartition describes a single partition queued on a Builder.
// StartByte and SizeBytes are set by Builder.Commit.
// UniqueGUID is set by Builder.Commit and holds the partition's unique GPT GUID.
type BuilderPartition struct {
	Index      int
	TypeGUID   [16]byte
	UniqueGUID [16]byte // set by Commit
	Name       string
	StartByte  int64 // set by Commit
	SizeBytes  int64 // set by Commit; pass 0 to fill remaining space
}

// ── Builder ───────────────────────────────────────────────────────────────────

// Builder constructs a new GPT disk image over an *os.File.
//
// Typical usage:
//
//	f, _   := os.Create("disk.img")
//	f.Truncate(8 << 30)
//	img, _ := diskimg.NewBuilder(f)
//	p1     := img.AddPartition(diskimg.GUID_EFISystem,  512<<20)
//	p2     := img.AddPartition(diskimg.GUID_BasicData,  0)       // fills rest
//	img.Commit()
//
//	raw1, _ := img.OpenRaw(p1.Index)
//	mkfs.FAT32(raw1, p1.SizeBytes, mkfs.Options{Label: "EFI"})
//
//	vol, _ := img.Mount(p1.Index)
//	defer vol.Unmount()
type Builder struct {
	f          *os.File
	diskSize   int64
	partitions []*BuilderPartition
	committed  *Image // populated by Commit
	diskGUID   [16]byte // set by Commit
}

// NewBuilder wraps f for GPT construction.
// f must already be sized (e.g. via f.Truncate) to the desired total capacity.
func NewBuilder(f *os.File) (*Builder, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("diskimg.NewBuilder: %w", err)
	}
	if size == 0 {
		return nil, fmt.Errorf("diskimg.NewBuilder: file is empty — call f.Truncate first")
	}
	return &Builder{f: f, diskSize: size}, nil
}

// AddPartition queues a new partition of the given GPT type GUID.
// If sizeBytes is 0 the partition will expand to fill all remaining
// usable space when Commit is called.
// The returned *BuilderPartition pointer is updated in-place by Commit.
func (b *Builder) AddPartition(typeGUID [16]byte, sizeBytes int64) *BuilderPartition {
	p := &BuilderPartition{
		Index:     len(b.partitions) + 1,
		TypeGUID:  typeGUID,
		SizeBytes: sizeBytes,
	}
	b.partitions = append(b.partitions, p)
	return p
}

// DiskGUID returns the disk GUID written into the GPT header.
// Only valid after Commit has been called.
func (b *Builder) DiskGUID() [16]byte { return b.diskGUID }

// Commit writes the protective MBR and GPT structures to the file,
// assigns StartByte/SizeBytes/UniqueGUID on each BuilderPartition, and
// prepares the Builder for OpenRaw / Mount calls.
func (b *Builder) Commit() error {
	const ss = 512

	totalSectors := b.diskSize / ss

	// GPT usable space: LBA 34 through LBA (totalSectors − 34 − 1).
	// (LBA 0 = protective MBR; LBA 1 = primary header; LBA 2–33 = primary
	//  entries; last 33 LBAs = secondary entries + secondary header.)
	firstUsable := int64(34)
	lastUsable := totalSectors - 34 - 1

	// ── Assign partition extents ──────────────────────────────────────────
	cursor := firstUsable
	for _, p := range b.partitions {
		p.StartByte = cursor * ss
		if p.SizeBytes == 0 {
			p.SizeBytes = (lastUsable - cursor + 1) * ss
		}
		sectors := (p.SizeBytes + ss - 1) / ss
		p.SizeBytes = sectors * ss
		if cursor+sectors-1 > lastUsable {
			return fmt.Errorf("diskimg.Builder: partition %d exceeds usable space", p.Index)
		}
		cursor += sectors
	}

	// ── Protective MBR ────────────────────────────────────────────────────
	mbr := make([]byte, ss)
	mbr[0x1BE] = 0x00 // non-bootable
	mbr[0x1BF] = 0x00 // CHS first head
	mbr[0x1C0] = 0x02 // CHS first sector
	mbr[0x1C1] = 0x00 // CHS first cylinder
	mbr[0x1C2] = 0xEE // type: GPT protective
	mbr[0x1C3] = 0xFF
	mbr[0x1C4] = 0xFF
	mbr[0x1C5] = 0xFF
	binary.LittleEndian.PutUint32(mbr[0x1C6:], 1) // LBA start
	lbaCount := uint32(totalSectors - 1)
	if int64(lbaCount) != totalSectors-1 {
		lbaCount = 0xFFFFFFFF // clamp to 32-bit max
	}
	binary.LittleEndian.PutUint32(mbr[0x1CA:], lbaCount)
	mbr[510] = 0x55
	mbr[511] = 0xAA
	if err := b.writeAt(mbr, 0); err != nil {
		return fmt.Errorf("diskimg.Builder: write protective MBR: %w", err)
	}

	// ── Partition entry array (128 entries × 128 bytes = 16 384 bytes) ────
	const (
		numEntries = 128
		entrySize  = 128
	)
	b.diskGUID = builderNewGUID()
	diskGUID := b.diskGUID
	entryBuf := make([]byte, numEntries*entrySize)
	for _, p := range b.partitions {
		off := (p.Index - 1) * entrySize
		copy(entryBuf[off:], p.TypeGUID[:])
		p.UniqueGUID = builderNewGUID()
		copy(entryBuf[off+16:], p.UniqueGUID[:])
		startLBA := uint64(p.StartByte / ss)
		endLBA := startLBA + uint64(p.SizeBytes/ss) - 1
		binary.LittleEndian.PutUint64(entryBuf[off+32:], startLBA)
		binary.LittleEndian.PutUint64(entryBuf[off+40:], endLBA)
		if p.Name != "" {
			copy(entryBuf[off+56:off+entrySize], builderUTF16LE(p.Name, 36))
		}
	}
	partCRC := crc32.ChecksumIEEE(entryBuf)

	// ── Primary GPT header (LBA 1) and entries (LBA 2–33) ─────────────────
	primaryHdr := builderGPTHeader(1, totalSectors-1, firstUsable, lastUsable,
		diskGUID, 2, numEntries, entrySize, partCRC)
	if err := b.writeAt(builderPadSector(primaryHdr, ss), ss); err != nil {
		return fmt.Errorf("diskimg.Builder: write primary GPT header: %w", err)
	}
	if err := b.writeAt(entryBuf, 2*ss); err != nil {
		return fmt.Errorf("diskimg.Builder: write primary partition entries: %w", err)
	}

	// ── Secondary entries (LBA n−33 … n−2) and header (LBA n−1) ──────────
	secondaryEntriesLBA := totalSectors - 1 - 32
	if err := b.writeAt(entryBuf, secondaryEntriesLBA*ss); err != nil {
		return fmt.Errorf("diskimg.Builder: write secondary partition entries: %w", err)
	}
	secondaryHdr := builderGPTHeader(totalSectors-1, 1, firstUsable, lastUsable,
		diskGUID, secondaryEntriesLBA, numEntries, entrySize, partCRC)
	if err := b.writeAt(builderPadSector(secondaryHdr, ss), (totalSectors-1)*ss); err != nil {
		return fmt.Errorf("diskimg.Builder: write secondary GPT header: %w", err)
	}

	// Re-parse the just-written GPT so Mount() can delegate to *Image.
	img, err := Attach(b.f.Name())
	if err != nil {
		return fmt.Errorf("diskimg.Builder.Commit: re-attach: %w", err)
	}
	b.committed = img
	return nil
}

// OpenRaw returns an io.ReadWriteSeeker whose position 0 maps to the first byte
// of partition index. Commit must be called before OpenRaw.
// Pass the returned seeker directly to mkfs.FAT32 or mkfs.ExFAT.
func (b *Builder) OpenRaw(index int) (io.ReadWriteSeeker, error) {
	if b.committed == nil {
		return nil, fmt.Errorf("diskimg.Builder.OpenRaw: call Commit first")
	}
	for _, p := range b.partitions {
		if p.Index == index {
			return &sectionRWS{f: b.f, base: p.StartByte, size: p.SizeBytes}, nil
		}
	}
	return nil, fmt.Errorf("diskimg.Builder.OpenRaw: partition %d not found", index)
}

// Mount mounts a partition by its 1-based index and returns a Volume.
// Commit must be called first. Delegates to the underlying *Image.
func (b *Builder) Mount(index int, opts ...MountOptions) (diskfs.Volume, error) {
	if b.committed == nil {
		return nil, fmt.Errorf("diskimg.Builder.Mount: call Commit first")
	}
	return b.committed.Mount(index, opts...)
}

// Detach flushes and closes the committed image.
// Optionally writes the result to outPath (see Image.Detach).
func (b *Builder) Detach(outPath string) error {
	if b.committed != nil {
		return b.committed.Detach(outPath)
	}
	return b.f.Close()
}

// ── sectionRWS ────────────────────────────────────────────────────────────────

// sectionRWS is an io.ReadWriteSeeker bounded to the byte range [base, base+size)
// within f. Position 0 of the seeker corresponds to byte base of f.
type sectionRWS struct {
	f    *os.File
	base int64
	size int64
	pos  int64
}

func (s *sectionRWS) Read(p []byte) (int, error) {
	if s.pos >= s.size {
		return 0, io.EOF
	}
	if int64(len(p)) > s.size-s.pos {
		p = p[:s.size-s.pos]
	}
	n, err := s.f.ReadAt(p, s.base+s.pos)
	s.pos += int64(n)
	return n, err
}

func (s *sectionRWS) Write(p []byte) (int, error) {
	if s.pos+int64(len(p)) > s.size {
		return 0, fmt.Errorf("sectionRWS: write would exceed partition boundary (%d + %d > %d)",
			s.pos, len(p), s.size)
	}
	n, err := s.f.WriteAt(p, s.base+s.pos)
	s.pos += int64(n)
	return n, err
}

func (s *sectionRWS) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = s.pos + offset
	case io.SeekEnd:
		newPos = s.size + offset
	default:
		return 0, fmt.Errorf("sectionRWS: invalid whence %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("sectionRWS: seek to negative position")
	}
	s.pos = newPos
	return s.pos, nil
}

// ── GPT construction helpers ──────────────────────────────────────────────────

func (b *Builder) writeAt(data []byte, off int64) error {
	_, err := b.f.WriteAt(data, off)
	return err
}

func builderGPTHeader(myLBA, altLBA, firstUsable, lastUsable int64,
	diskGUID [16]byte, entryStartLBA int64,
	numEntries, entrySize int, partCRC uint32) []byte {
	h := make([]byte, 92)
	copy(h[0:8], "EFI PART")
	binary.LittleEndian.PutUint32(h[8:], 0x00010000) // revision 1.0
	binary.LittleEndian.PutUint32(h[12:], 92)         // header size
	// h[16:20] = header CRC32 (filled in below, after computing)
	binary.LittleEndian.PutUint64(h[24:], uint64(myLBA))
	binary.LittleEndian.PutUint64(h[32:], uint64(altLBA))
	binary.LittleEndian.PutUint64(h[40:], uint64(firstUsable))
	binary.LittleEndian.PutUint64(h[48:], uint64(lastUsable))
	copy(h[56:72], diskGUID[:])
	binary.LittleEndian.PutUint64(h[72:], uint64(entryStartLBA))
	binary.LittleEndian.PutUint32(h[80:], uint32(numEntries))
	binary.LittleEndian.PutUint32(h[84:], uint32(entrySize))
	binary.LittleEndian.PutUint32(h[88:], partCRC)
	// Header CRC is over the first 92 bytes with the CRC field zeroed.
	binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(h))
	return h
}

// builderNewGUID returns a random RFC 4122 version-4 UUID.
func builderNewGUID() [16]byte {
	var g [16]byte
	if _, err := rand.Read(g[:]); err != nil {
		panic(fmt.Sprintf("diskimg: crypto/rand: %v", err))
	}
	g[6] = (g[6] & 0x0F) | 0x40 // version 4
	g[8] = (g[8] & 0x3F) | 0x80 // variant 10xx
	return g
}

// mustParseGPTGUID converts a standard GUID string to the mixed-endian
// on-disk representation used by GPT (first three components little-endian,
// last two components big-endian).
func mustParseGPTGUID(s string) [16]byte {
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) != 32 {
		panic("diskimg: invalid GUID length: " + s)
	}
	raw, err := hex.DecodeString(clean)
	if err != nil {
		panic("diskimg: invalid GUID hex: " + err.Error())
	}
	var g [16]byte
	// Component 1 (4 bytes): little-endian
	g[0], g[1], g[2], g[3] = raw[3], raw[2], raw[1], raw[0]
	// Component 2 (2 bytes): little-endian
	g[4], g[5] = raw[5], raw[4]
	// Component 3 (2 bytes): little-endian
	g[6], g[7] = raw[7], raw[6]
	// Components 4+5 (8 bytes): big-endian (verbatim)
	copy(g[8:], raw[8:])
	return g
}

// builderUTF16LE encodes s as UTF-16LE into a buffer of maxRunes × 2 bytes.
func builderUTF16LE(s string, maxRunes int) []byte {
	runes := []rune(s)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	buf := make([]byte, maxRunes*2)
	for i, r := range runes {
		if u := utf16.Encode([]rune{r}); len(u) > 0 {
			binary.LittleEndian.PutUint16(buf[i*2:], u[0])
		}
	}
	return buf
}

// builderPadSector zero-pads or truncates b to exactly size bytes.
func builderPadSector(b []byte, size int) []byte {
	out := make([]byte, size)
	copy(out, b)
	return out
}