package diskimg

import (
	"sort"

	"github.com/carbon-os/diskimg/partition"
)

// RegionKind classifies a byte range within the disk image.
type RegionKind int

const (
	RegionBoot      RegionKind = iota // MBR + GRUB + GPT primary header
	RegionPartition                   // filesystem lives here
	RegionGap                         // unallocated space between partitions
	RegionBackup                      // GPT backup header at end of disk
)

// Region is a contiguous byte range in the disk image with a known purpose.
type Region struct {
	Kind           RegionKind
	Start          int64 // inclusive byte offset
	End            int64 // exclusive byte offset
	PartitionIndex int   // 1-based; valid only when Kind == RegionPartition
}

func (r *Region) Size() int64 { return r.End - r.Start }

// buildRegions constructs an ordered slice of Regions from the parsed
// partition list and the total file size.  Gaps between partitions become
// RegionGap entries.  The first 512 bytes (or 34 sectors for GPT) are
// RegionBoot; the last sector is RegionBackup for GPT images.
func buildRegions(parts []*partition.Partition, fileSize int64, isGPT bool) []*Region {
	const sectorSize = 512

	type span struct {
		start, end int64
		pIdx       int // 0 → not a partition
	}

	// Collect partition spans.
	spans := make([]span, len(parts))
	for i, p := range parts {
		spans[i] = span{start: p.StartByte, end: p.StartByte + p.SizeBytes, pIdx: p.Index}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	// Boot region covers LBA 0 through the first partition's start.
	bootEnd := int64(sectorSize) // default for MBR
	if isGPT {
		// GPT: protective MBR (512) + header (512) + 32 entry sectors (16384) = 17408
		bootEnd = 34 * sectorSize
	}
	if len(spans) > 0 && spans[0].start < bootEnd {
		bootEnd = spans[0].start
	}

	var regions []*Region
	regions = append(regions, &Region{Kind: RegionBoot, Start: 0, End: bootEnd})

	cursor := bootEnd
	for _, s := range spans {
		if s.start > cursor {
			regions = append(regions, &Region{Kind: RegionGap, Start: cursor, End: s.start})
		}
		regions = append(regions, &Region{
			Kind:           RegionPartition,
			Start:          s.start,
			End:            s.end,
			PartitionIndex: s.pIdx,
		})
		cursor = s.end
	}

	// GPT backup header occupies the last sector.
	if isGPT && fileSize > 0 {
		backupStart := fileSize - sectorSize
		if cursor < backupStart {
			regions = append(regions, &Region{Kind: RegionGap, Start: cursor, End: backupStart})
		}
		regions = append(regions, &Region{Kind: RegionBackup, Start: backupStart, End: fileSize})
	} else if cursor < fileSize {
		regions = append(regions, &Region{Kind: RegionGap, Start: cursor, End: fileSize})
	}

	return regions
}