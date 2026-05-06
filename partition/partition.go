// Package partition represents a parsed partition-table entry.
package partition

// Partition holds the location of a single partition within a disk image.
type Partition struct {
	Index     int    // 1-based partition number
	StartByte int64  // byte offset of first sector of partition
	SizeBytes int64  // total byte length of partition
	TypeGUID  string // GPT partition type GUID (empty for MBR partitions)
	Name      string // human-readable label (GPT only)
}