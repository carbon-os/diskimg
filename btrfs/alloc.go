package btrfs

import (
	"hash/crc32"
)

// ── block allocator ───────────────────────────────────────────────────────────

// allocDataBlock allocates a contiguous region of sectorSize bytes for file
// data, extending the last chunk mapping if needed.
// Returns the logical (== physical for single-device) byte number.
func (v *Volume) allocDataBlock(size uint64) uint64 {
	logical := v.allocPtr
	ss := uint64(v.sb.sectorSize)
	if ss == 0 {
		ss = 4096
	}
	// Round up to sector boundary.
	aligned := (size + ss - 1) / ss * ss
	v.allocPtr += aligned

	// Extend chunk map.
	if len(v.chunks) > 0 {
		last := &v.chunks[len(v.chunks)-1]
		if logical == last.logStart+last.length {
			last.length += aligned
			return logical
		}
	}
	v.chunks = append(v.chunks, chunkMap{
		logStart:  logical,
		length:    aligned,
		physStart: logical,
	})
	return logical
}

// ── objectID allocator ────────────────────────────────────────────────────────

func (v *Volume) allocObjID() uint64 {
	id := v.nextObjID
	v.nextObjID++
	return id
}

// ── name hash ─────────────────────────────────────────────────────────────────

// nameHash returns the Btrfs directory name hash: CRC32c(seed=~1, name).
func nameHash(name string) uint64 {
	h := crc32.Update(nameHashSeed, crc32.MakeTable(crc32.Castagnoli), []byte(name))
	return uint64(h)
}

// ── DIR_INDEX sequence ────────────────────────────────────────────────────────

// nextDirSeq returns (and increments) the next DIR_INDEX sequence for a dir.
func (v *Volume) nextDirSeq(dirObjID uint64) uint64 {
	seq := v.dirSeq[dirObjID]
	v.dirSeq[dirObjID] = seq + 1
	return seq
}

// initDirSeq scans the DIR_INDEX items for dirObjID and seeds dirSeq
// with max(offset)+1 so new entries don't collide.
func (v *Volume) initDirSeq(dirObjID uint64) {
	if _, ok := v.dirSeq[dirObjID]; ok {
		return
	}
	var maxSeq uint64
	_ = v.walkTree(v.fsTreeRoot, func(k btrfsKey, _ []byte) error {
		if k.objectID == dirObjID && k.itemType == typeDirIndex {
			if k.offset >= maxSeq {
				maxSeq = k.offset + 1
			}
		}
		return nil
	})
	v.dirSeq[dirObjID] = maxSeq
}