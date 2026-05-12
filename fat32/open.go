// Package fat32 implements FAT12/16/32 filesystem read/write support.
package fat32

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	volfs "github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/fs/fstype"
)

// ── constants ────────────────────────────────────────────────────────────────

const (
	fatFREE = uint32(0x00000000)

	attrReadOnly  = uint8(0x01)
	attrHidden    = uint8(0x02)
	attrSystem    = uint8(0x04)
	attrVolumeID  = uint8(0x08)
	attrDirectory = uint8(0x10)
	attrArchive   = uint8(0x20)
	attrLFN       = uint8(0x0F)
)

// ── BPB ──────────────────────────────────────────────────────────────────────

type bpb struct {
	bytesPerSec uint16
	secPerClus  uint8
	rsvdSecCnt  uint16
	numFATs     uint8
	rootEntCnt  uint16 // FAT12/16 only; 0 for FAT32
	fatSz16     uint16
	fatSz32     uint32
	rootClus    uint32 // FAT32 root cluster
	totSec16    uint16
	totSec32    uint32

	// Derived.
	fatType      fstype.Type
	fatSz        uint32 // sectors per FAT copy
	firstDataSec uint32 // first sector of cluster 2
	cntClusters  uint32
	rootDirSec   uint32 // FAT12/16: sector count of fixed root dir
	firstRootDir uint32 // FAT12/16: first root-dir sector; FAT32: = rootClus
	clusterSize  uint32 // bytes per cluster
}

// ── fatDirEntry ───────────────────────────────────────────────────────────────

// fatDirEntry is a fully parsed in-memory directory entry.
// dirCluster is the specific cluster that physically holds the short entry
// (0 = FAT12/16 fixed root dir). dirOffset is the byte offset of the short
// entry within that cluster (or within the fixed root dir buffer for FAT12/16).
type fatDirEntry struct {
	name      string   // LFN if present, otherwise decoded 8.3
	shortName [11]byte // raw 8.3 name bytes
	attr      uint8
	cluster   uint32 // full 32-bit first data cluster
	size      uint32

	wrtDate uint16
	wrtTime uint16
	crtDate uint16
	crtTime uint16

	// Location — used by writeDirEntry.
	dirCluster uint32 // cluster that holds the short entry (0 = fixed root)
	dirOffset  uint32 // byte offset of the short entry within dirCluster
}

func (e *fatDirEntry) isDir() bool { return e.attr&attrDirectory != 0 }

// ── Volume ────────────────────────────────────────────────────────────────────

// Volume implements fs.Volume for a FAT filesystem.
type Volume struct {
	mu    sync.Mutex
	rw    io.WriterAt       // full-image writer — used by flush
	sr    *io.SectionReader // partition-scoped reader
	start int64             // byte offset of partition within the image file
	b     bpb
	ft    fstype.Type
	fat1  []byte           // FAT table 1, cached in memory
	dirty map[uint32][]byte // partition-relative sector number → 512-byte data
}

// Open mounts the FAT partition at [start, start+size) inside rw.
// rw must implement both io.ReaderAt and io.WriterAt (e.g. *os.File).
// ft is the type hint from fstype.Detect; the final type is always re-derived
// from the cluster count per the Microsoft FAT specification.
func Open(rw interface {
	io.ReaderAt
	io.WriterAt
}, start, size int64, ft fstype.Type) (*Volume, error) {
	sr := io.NewSectionReader(rw, start, size)
	v := &Volume{
		rw:    rw,
		sr:    sr,
		start: start,
		ft:    ft,
		dirty: make(map[uint32][]byte),
	}
	if err := v.readBPB(); err != nil {
		return nil, err
	}
	if err := v.readFATTable(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *Volume) Type() fstype.Type { return v.ft }

// ── BPB ──────────────────────────────────────────────────────────────────────

func (v *Volume) readBPB() error {
	buf := make([]byte, 512)
	if _, err := v.sr.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("fat32: read boot sector: %w", err)
	}
	le := binary.LittleEndian
	b := &v.b

	b.bytesPerSec = le.Uint16(buf[11:13])
	b.secPerClus  = buf[13]
	b.rsvdSecCnt  = le.Uint16(buf[14:16])
	b.numFATs     = buf[16]
	b.rootEntCnt  = le.Uint16(buf[17:19])
	b.totSec16    = le.Uint16(buf[19:21])
	b.fatSz16     = le.Uint16(buf[22:24])
	b.fatSz32     = le.Uint32(buf[36:40])
	b.rootClus    = le.Uint32(buf[44:48])
	b.totSec32    = le.Uint32(buf[32:36])

	if b.bytesPerSec == 0 || b.secPerClus == 0 {
		return fmt.Errorf("fat32: invalid BPB: bytesPerSec=%d secPerClus=%d",
			b.bytesPerSec, b.secPerClus)
	}

	if b.fatSz16 != 0 {
		b.fatSz = uint32(b.fatSz16)
	} else {
		b.fatSz = b.fatSz32
	}
	totSec := uint32(b.totSec16)
	if totSec == 0 {
		totSec = b.totSec32
	}

	b.rootDirSec  = (uint32(b.rootEntCnt)*32 + uint32(b.bytesPerSec) - 1) / uint32(b.bytesPerSec)
	b.firstDataSec = uint32(b.rsvdSecCnt) + uint32(b.numFATs)*b.fatSz + b.rootDirSec
	dataSec       := totSec - b.firstDataSec
	b.cntClusters  = dataSec / uint32(b.secPerClus)
	b.clusterSize  = uint32(b.bytesPerSec) * uint32(b.secPerClus)

	switch {
	case b.cntClusters < 4085:
		b.fatType = fstype.FAT12
	case b.cntClusters < 65525:
		b.fatType = fstype.FAT16
	default:
		b.fatType = fstype.FAT32
	}
	v.ft = b.fatType

	if b.fatType == fstype.FAT32 {
		b.firstRootDir = b.rootClus
	} else {
		b.firstRootDir = uint32(b.rsvdSecCnt) + uint32(b.numFATs)*b.fatSz
	}
	return nil
}

// ── FAT table ────────────────────────────────────────────────────────────────

func (v *Volume) readFATTable() error {
	fatStart := int64(v.b.rsvdSecCnt) * int64(v.b.bytesPerSec)
	fatBytes := int64(v.b.fatSz) * int64(v.b.bytesPerSec)
	v.fat1 = make([]byte, fatBytes)
	_, err := v.sr.ReadAt(v.fat1, fatStart)
	return err
}

func (v *Volume) fatEntry(n uint32) uint32 {
	switch v.b.fatType {
	case fstype.FAT12:
		off := n + n/2
		w := binary.LittleEndian.Uint16(v.fat1[off : off+2])
		if n%2 == 0 {
			return uint32(w) & 0xFFF
		}
		return uint32(w) >> 4
	case fstype.FAT16:
		off := n * 2
		return uint32(binary.LittleEndian.Uint16(v.fat1[off : off+2]))
	default: // FAT32
		off := n * 4
		return binary.LittleEndian.Uint32(v.fat1[off:off+4]) & 0x0FFFFFFF
	}
}

func (v *Volume) setFATEntry(n, val uint32) {
	switch v.b.fatType {
	case fstype.FAT12:
		off := n + n/2
		w := binary.LittleEndian.Uint16(v.fat1[off : off+2])
		if n%2 == 0 {
			w = (w & 0xF000) | uint16(val&0xFFF)
		} else {
			w = (w & 0x000F) | uint16((val&0xFFF)<<4)
		}
		binary.LittleEndian.PutUint16(v.fat1[off:off+2], w)
	case fstype.FAT16:
		binary.LittleEndian.PutUint16(v.fat1[n*2:n*2+2], uint16(val))
	default: // FAT32
		existing := binary.LittleEndian.Uint32(v.fat1[n*4 : n*4+4])
		existing = (existing & 0xF0000000) | (val & 0x0FFFFFFF)
		binary.LittleEndian.PutUint32(v.fat1[n*4:n*4+4], existing)
	}
}

func (v *Volume) isEOC(val uint32) bool {
	switch v.b.fatType {
	case fstype.FAT12:
		return val >= 0xFF8
	case fstype.FAT16:
		return val >= 0xFFF8
	default:
		return val >= 0x0FFFFFF8
	}
}

func (v *Volume) eocValue() uint32 {
	switch v.b.fatType {
	case fstype.FAT12:
		return 0xFFF
	case fstype.FAT16:
		return 0xFFFF
	default:
		return 0x0FFFFFFF
	}
}

// ── cluster I/O ───────────────────────────────────────────────────────────────

// clusterByteOff returns the byte offset from partition start of cluster n.
func (v *Volume) clusterByteOff(n uint32) int64 {
	sec := int64(v.b.firstDataSec) + int64(n-2)*int64(v.b.secPerClus)
	return sec * int64(v.b.bytesPerSec)
}

func (v *Volume) readCluster(n uint32) ([]byte, error) {
	if n < 2 {
		return nil, fmt.Errorf("fat32: invalid cluster %d", n)
	}
	buf := make([]byte, v.b.clusterSize)
	if _, err := v.sr.ReadAt(buf, v.clusterByteOff(n)); err != nil {
		return nil, fmt.Errorf("fat32: read cluster %d: %w", n, err)
	}
	secSize  := int(v.b.bytesPerSec)
	firstSec := uint32(v.b.firstDataSec) + (n-2)*uint32(v.b.secPerClus)
	for i := 0; i < int(v.b.secPerClus); i++ {
		if data, ok := v.dirty[firstSec+uint32(i)]; ok {
			copy(buf[i*secSize:], data)
		}
	}
	return buf, nil
}

// writeCluster stages all sectors of cluster n into the dirty map.
func (v *Volume) writeCluster(n uint32, data []byte) {
	secSize := int(v.b.bytesPerSec)
	firstSec := uint32(v.b.firstDataSec) + (n-2)*uint32(v.b.secPerClus)
	for i := 0; i < int(v.b.secPerClus); i++ {
		buf := make([]byte, secSize)
		copy(buf, data[i*secSize:(i+1)*secSize])
		v.dirty[firstSec+uint32(i)] = buf
	}
}

// ── StatFS ────────────────────────────────────────────────────────────────────

func (v *Volume) StatFS() (volfs.VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	var free uint32
	for n := uint32(2); n < v.b.cntClusters+2; n++ {
		if v.fatEntry(n) == fatFREE {
			free++
		}
	}
	total := v.b.cntClusters
	cs := int64(v.b.clusterSize)
	return volfs.VolumeInfo{
		TotalBytes: int64(total) * cs,
		FreeBytes:  int64(free) * cs,
		UsedBytes:  int64(total-free) * cs,
		BlockSize:  int64(v.b.bytesPerSec),
	}, nil
}

// ── date/time ─────────────────────────────────────────────────────────────────

func fatDate(t time.Time) uint16 {
	y := t.Year() - 1980
	if y < 0 {
		y = 0
	}
	if y > 127 {
		y = 127
	}
	return uint16(y<<9) | uint16(t.Month())<<5 | uint16(t.Day())
}

func fatTime(t time.Time) uint16 {
	return uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
}

func fatToTime(d, t uint16) time.Time {
	year  := int(d>>9) + 1980
	month := time.Month((d >> 5) & 0xF)
	day   := int(d & 0x1F)
	hour  := int(t >> 11)
	min   := int((t >> 5) & 0x3F)
	sec   := int(t&0x1F) * 2
	if month < 1 {
		month = 1
	}
	if day < 1 {
		day = 1
	}
	return time.Date(year, month, day, hour, min, sec, 0, time.UTC)
}

// ── name helpers ──────────────────────────────────────────────────────────────

// decodeShortName converts an 11-byte FAT name to a display string.
func decodeShortName(raw [11]byte) string {
	name := strings.TrimRight(string(raw[:8]), " ")
	ext  := strings.TrimRight(string(raw[8:11]), " ")
	if ext == "" {
		return name
	}
	return name + "." + ext
}

// toShortName converts a filename to an 8.3 uppercase short name.
// Returns (name, true) on success or (zeroed, false) if the name doesn't fit.
func toShortName(name string) ([11]byte, bool) {
	var out [11]byte
	for i := range out {
		out[i] = ' '
	}
	dot := strings.LastIndex(name, ".")
	var base, ext string
	if dot < 0 {
		base = name
	} else {
		base = name[:dot]
		ext  = name[dot+1:]
	}
	if len(base) > 8 || len(ext) > 3 {
		return out, false
	}
	copy(out[:8], strings.ToUpper(base))
	copy(out[8:11], strings.ToUpper(ext))
	return out, true
}