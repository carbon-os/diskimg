package diskimg

import (
	"sort"

	"github.com/carbon-os/diskimg/partition"
)

type RegionKind int

const (
	RegionBoot      RegionKind = iota
	RegionPartition
	RegionGap
	RegionBackup
)

type Region struct {
	Kind           RegionKind
	Start          int64
	End            int64
	PartitionIndex int
}

func (r *Region) Size() int64 { return r.End - r.Start }

func buildRegions(parts []*partition.Partition, fileSize int64, isGPT bool) []*Region {
	const sectorSize = 512

	type span struct {
		start, end int64
		pIdx       int
	}

	spans := make([]span, len(parts))
	for i, p := range parts {
		spans[i] = span{start: p.StartByte, end: p.StartByte + p.SizeBytes, pIdx: p.Index}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	bootEnd := int64(sectorSize)
	if isGPT {
		bootEnd = 34 * sectorSize
	}
	if len(spans) > 0 && spans[0].start < bootEnd {
		bootEnd = spans[0].start
	}

	var regions []*Region

	// Only emit a boot region if it has non-zero size (whole-disk images
	// have their first partition at byte 0, leaving no room for a boot region).
	if bootEnd > 0 {
		regions = append(regions, &Region{Kind: RegionBoot, Start: 0, End: bootEnd})
	}

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