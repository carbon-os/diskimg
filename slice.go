package diskimg

// SliceKind classifies a byte range within the image.
type SliceKind int

const (
	SliceKindGap       SliceKind = iota // before first partition (boot code, GPT header)
	SliceKindPartition                  // a filesystem partition
	SliceKindBetween                    // gap between two partitions
	SliceKindTail                       // after last partition (GPT backup header)
)

// Slice is a contiguous byte range inside the disk image.
type Slice struct {
	Kind           SliceKind
	Start          int64 // byte offset, inclusive
	End            int64 // byte offset, exclusive
	PartitionIndex int   // 1-based; only set when Kind == SliceKindPartition
}

// Size returns the byte length of this slice.
func (s *Slice) Size() int64 { return s.End - s.Start }

// buildSlices computes the ordered slice list from the parsed partition table.
// Every byte in the image is covered by exactly one slice.
func (img *Image) buildSlices() {
	img.slices = nil
	cursor := int64(0)

	for i, p := range img.partitions {
		if p.StartBytes > cursor {
			kind := SliceKindGap
			if i > 0 {
				kind = SliceKindBetween
			}
			img.slices = append(img.slices, &Slice{
				Kind:  kind,
				Start: cursor,
				End:   p.StartBytes,
			})
		}
		img.slices = append(img.slices, &Slice{
			Kind:           SliceKindPartition,
			Start:          p.StartBytes,
			End:            p.StartBytes + p.SizeBytes,
			PartitionIndex: i + 1,
		})
		cursor = p.StartBytes + p.SizeBytes
	}

	if cursor < img.size {
		img.slices = append(img.slices, &Slice{
			Kind:  SliceKindTail,
			Start: cursor,
			End:   img.size,
		})
	}
}