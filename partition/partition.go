package partition

import "fmt"

// TableType identifies the partition table scheme.
type TableType string

const (
	TableTypeGPT TableType = "gpt"
	TableTypeMBR TableType = "mbr"
	TableTypeRaw TableType = "raw"
)

// Partition represents a single parsed partition from a disk image.
type Partition struct {
	Index      int    // 1-based
	StartLBA   uint64 // start in logical block addresses
	EndLBA     uint64 // end LBA (inclusive)
	StartBytes int64  // byte offset in image
	SizeBytes  int64  // size in bytes

	// MBR fields
	TypeByte byte // e.g. 0x83=Linux, 0x0C=FAT32
	Bootable bool

	// GPT fields
	TypeGUID   string // e.g. "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	UniqueGUID string
	Name       string
}

func (p *Partition) String() string {
	return fmt.Sprintf("Partition %d: start=0x%X size=%d bytes",
		p.Index, p.StartBytes, p.SizeBytes)
}