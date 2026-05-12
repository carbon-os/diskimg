// alloc.go
package ntfs

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"unicode/utf16"
)

// ── cluster bitmap ────────────────────────────────────────────────────────────

// allocCluster finds the first free cluster at LCN ≥ 1, marks it used, and
// returns its LCN.
//
// LCN 0 is permanently skipped: it is the partition-relative offset of the
// NTFS boot sector ($Boot) and must never be handed out for file data.  A
// correct mkfs will already have that bit set in $Bitmap, but we defend here
// as well so that a mkfs bug cannot silently corrupt the boot sector.
func (v *ntfsVolume) allocCluster() (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, b := range v.bitmap {
		if b == 0xFF {
			continue
		}
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(bit)) == 0 {
				lcn := int64(i*8 + bit)
				if lcn == 0 {
					// LCN 0 is the boot sector; never allocate it.
					// If mkfs left this bit clear that is a mkfs bug;
					// mark it used now so we never revisit it.
					v.bitmap[i] |= 1 << uint(bit)
					v.dirty = true
					continue
				}
				v.bitmap[i] |= 1 << uint(bit)
				v.dirty = true
				return lcn, nil
			}
		}
	}
	return 0, fmt.Errorf("no free clusters (volume full)")
}

// allocClusters allocates n contiguous-or-scattered clusters and returns a runlist.
func (v *ntfsVolume) allocClusters(n int64) ([]run, error) {
	var runs []run
	remaining := n
	for remaining > 0 {
		lcn, err := v.allocCluster()
		if err != nil {
			return nil, err
		}
		// Extend the last run if contiguous, otherwise start a new one.
		if len(runs) > 0 {
			last := &runs[len(runs)-1]
			if last.lcn+last.length == lcn {
				last.length++
				remaining--
				continue
			}
		}
		runs = append(runs, run{lcn: lcn, length: 1})
		remaining--
	}
	return runs, nil
}

// freeCluster marks a single cluster as free in the bitmap.
func (v *ntfsVolume) freeCluster(lcn int64) {
	if lcn == 0 {
		return // never free the boot sector cluster
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	byteIdx := lcn / 8
	bitIdx := uint(lcn % 8)
	if int(byteIdx) < len(v.bitmap) {
		v.bitmap[byteIdx] &^= 1 << bitIdx
		v.dirty = true
	}
}

// freeRuns frees all clusters referenced by a runlist.
func (v *ntfsVolume) freeRuns(runs []run) {
	for _, r := range runs {
		if r.lcn < 0 {
			continue
		}
		for i := int64(0); i < r.length; i++ {
			v.freeCluster(r.lcn + i)
		}
	}
}

// allocAndWrite allocates clusters for data, writes the data, and returns the runlist.
func (v *ntfsVolume) allocAndWrite(data []byte) ([]run, error) {
	numClusters := (int64(len(data)) + v.clusterSize - 1) / v.clusterSize
	runs, err := v.allocClusters(numClusters)
	if err != nil {
		return nil, err
	}
	// Write the data cluster by cluster across the runs.
	var written int64
	for _, r := range runs {
		for c := int64(0); c < r.length; c++ {
			off := (r.lcn + c) * v.clusterSize
			start := written
			end := written + v.clusterSize
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			chunk := make([]byte, v.clusterSize)
			if start < int64(len(data)) {
				copy(chunk, data[start:end])
			}
			if _, err := v.partWrite(chunk, off); err != nil {
				return nil, fmt.Errorf("write cluster LCN %d: %w", r.lcn+c, err)
			}
			written += v.clusterSize
		}
	}
	return runs, nil
}

// ── MFT record bitmap ─────────────────────────────────────────────────────────

// allocMFTRecord finds the first free MFT record slot, marks it used, and
// returns its record number.
func (v *ntfsVolume) allocMFTRecord() (uint64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, b := range v.mftBitmap {
		if b == 0xFF {
			continue
		}
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(bit)) == 0 {
				n := uint64(i*8 + bit)
				if n < 12 {
					continue // never reuse the 12 system records
				}
				v.mftBitmap[i] |= 1 << uint(bit)
				v.dirty = true
				return n, nil
			}
		}
	}
	// Extend the MFT bitmap by one byte (= 8 new record slots).
	newByte := byte(0x01) // mark first new slot as used
	n := uint64(len(v.mftBitmap) * 8)
	v.mftBitmap = append(v.mftBitmap, newByte)
	v.dirty = true
	return n, nil
}

// freeMFTSlot marks MFT record n as free in the bitmap.
func freeMFTSlot(bitmap []byte, n uint64) {
	byteIdx := n / 8
	bitIdx := uint(n % 8)
	if int(byteIdx) < len(bitmap) {
		bitmap[byteIdx] &^= 1 << bitIdx
	}
}

// ── directory index management ────────────────────────────────────────────────

// addDirEntry inserts a $FILE_NAME index entry into the directory at dirMFTNum.
// This implementation handles the resident-only (small directory) case directly.
// Large directories with $INDEX_ALLOCATION are handled by appending to a new
// index block.
func (v *ntfsVolume) addDirEntry(dirMFTNum uint64, name string, childMFTNum uint64, isDir bool) error {
	dirRec, err := v.getRecord(dirMFTNum)
	if err != nil {
		return err
	}
	dirRec = cloneRecord(dirRec)

	idxRootOff := findAttrOffset(dirRec, attrINDEX_ROOT, "$I30")
	if idxRootOff < 0 {
		idxRootOff = findAttrOffset(dirRec, attrINDEX_ROOT, "")
	}
	if idxRootOff < 0 {
		return fmt.Errorf("directory %d has no $INDEX_ROOT", dirMFTNum)
	}

	valOff := idxRootOff + int(binary.LittleEndian.Uint16(dirRec[idxRootOff+0x14:]))
	valLen := int(binary.LittleEndian.Uint32(dirRec[idxRootOff+0x10:]))
	if valOff+valLen > len(dirRec) {
		return fmt.Errorf("$INDEX_ROOT value out of bounds")
	}

	// Build index entry for the new file.
	fa := uint32(faArchive)
	if isDir {
		fa = faDirectory
	}
	fnKey := buildFileNameAttr(name, dirMFTNum, fa, isDir)
	entry := buildIndexEntry(childMFTNum, fnKey)

	// Insert before the last-entry sentinel in the resident index.
	idxHdr := dirRec[valOff+16 : valOff+valLen]
	newHdr, err := insertIndexEntry(idxHdr, entry)
	if err != nil {
		return err
	}

	// Rebuild the $INDEX_ROOT attribute value.
	newVal := make([]byte, 16+len(newHdr))
	copy(newVal, dirRec[valOff:valOff+16]) // copy the first 16 bytes (attr type, collation, …)
	copy(newVal[16:], newHdr)

	// Replace the $INDEX_ROOT attribute.
	dirRec = removeAttr(dirRec, attrINDEX_ROOT, "$I30")
	if findAttrOffset(dirRec, attrINDEX_ROOT, "$I30") < 0 {
		dirRec = removeAttr(dirRec, attrINDEX_ROOT, "")
	}
	off := nextAttrOffset(dirRec, int(binary.LittleEndian.Uint16(dirRec[0x14:])))
	dirRec = appendNamedResidentAttrAt(dirRec, off, attrINDEX_ROOT, "$I30", newVal)
	return v.putRecord(dirMFTNum, dirRec)
}

// removeDirEntry removes the index entry for name from its parent directory.
func (v *ntfsVolume) removeDirEntry(filePath string) error {
	parentPath := path.Dir(filePath)
	baseName := path.Base(filePath)
	parentNum, dirRec, err := v.lookupPath(parentPath)
	if err != nil {
		return err
	}
	dirRec = cloneRecord(dirRec)

	idxRootOff := findAttrOffset(dirRec, attrINDEX_ROOT, "$I30")
	if idxRootOff < 0 {
		idxRootOff = findAttrOffset(dirRec, attrINDEX_ROOT, "")
	}
	if idxRootOff < 0 {
		return nil // no index; nothing to remove
	}

	valOff := idxRootOff + int(binary.LittleEndian.Uint16(dirRec[idxRootOff+0x14:]))
	valLen := int(binary.LittleEndian.Uint32(dirRec[idxRootOff+0x10:]))
	if valOff+valLen > len(dirRec) {
		return nil
	}

	idxHdr := dirRec[valOff+16 : valOff+valLen]
	newHdr := deleteIndexEntry(idxHdr, baseName)

	newVal := make([]byte, 16+len(newHdr))
	copy(newVal, dirRec[valOff:valOff+16])
	copy(newVal[16:], newHdr)

	dirRec = removeAttr(dirRec, attrINDEX_ROOT, "$I30")
	if findAttrOffset(dirRec, attrINDEX_ROOT, "$I30") < 0 {
		dirRec = removeAttr(dirRec, attrINDEX_ROOT, "")
	}
	off := nextAttrOffset(dirRec, int(binary.LittleEndian.Uint16(dirRec[0x14:])))
	dirRec = appendNamedResidentAttrAt(dirRec, off, attrINDEX_ROOT, "$I30", newVal)
	return v.putRecord(parentNum, dirRec)
}

// buildIndexEntry constructs a binary $I30 index entry for a $FILE_NAME key.
func buildIndexEntry(mftNum uint64, fnKey []byte) []byte {
	keyLen := len(fnKey)
	entLen := (16 + keyLen + 7) &^ 7 // 16-byte header + key, aligned to 8
	entry := make([]byte, entLen)
	binary.LittleEndian.PutUint64(entry[0:], mftNum|uint64(1)<<48)
	binary.LittleEndian.PutUint16(entry[8:], uint16(entLen))
	binary.LittleEndian.PutUint16(entry[10:], uint16(keyLen))
	copy(entry[16:], fnKey)
	return entry
}

// insertIndexEntry inserts a new index entry before the last-entry sentinel.
func insertIndexEntry(hdr []byte, entry []byte) ([]byte, error) {
	// Find the last-entry sentinel.
	entriesOff := int(binary.LittleEndian.Uint32(hdr[0:]))
	indexLen := int(binary.LittleEndian.Uint32(hdr[4:]))
	if entriesOff > len(hdr) || indexLen > len(hdr) {
		return nil, fmt.Errorf("index header corrupt")
	}

	data := hdr[entriesOff:indexLen]
	sentinelOff := -1
	off := 0
	for off+16 <= len(data) {
		flags := binary.LittleEndian.Uint32(data[off+12:])
		if flags&idxFlagLastEntry != 0 {
			sentinelOff = entriesOff + off
			break
		}
		entLen := int(binary.LittleEndian.Uint16(data[off+8:]))
		if entLen <= 0 {
			break
		}
		off += entLen
	}
	if sentinelOff < 0 {
		return nil, fmt.Errorf("index has no last-entry sentinel")
	}

	// Insert new entry at sentinelOff.
	newHdr := make([]byte, len(hdr)+len(entry))
	copy(newHdr, hdr[:sentinelOff])
	copy(newHdr[sentinelOff:], entry)
	copy(newHdr[sentinelOff+len(entry):], hdr[sentinelOff:])

	// Update INDEX_HEADER lengths.
	newIndexLen := indexLen + len(entry)
	binary.LittleEndian.PutUint32(newHdr[4:], uint32(newIndexLen))
	binary.LittleEndian.PutUint32(newHdr[8:], uint32(newIndexLen))
	return newHdr, nil
}

// deleteIndexEntry removes the entry with the given name from an INDEX_HEADER body.
func deleteIndexEntry(hdr []byte, name string) []byte {
	entriesOff := int(binary.LittleEndian.Uint32(hdr[0:]))
	indexLen := int(binary.LittleEndian.Uint32(hdr[4:]))
	if entriesOff > len(hdr) || indexLen > len(hdr) {
		return hdr
	}
	data := hdr[entriesOff:indexLen]
	off := 0
	for off+16 <= len(data) {
		flags := binary.LittleEndian.Uint32(data[off+12:])
		if flags&idxFlagLastEntry != 0 {
			break
		}
		entLen := int(binary.LittleEndian.Uint16(data[off+8:]))
		keyLen := int(binary.LittleEndian.Uint16(data[off+10:]))
		if entLen <= 0 {
			break
		}
		if keyLen >= 66 {
			key := data[off+16 : off+16+keyLen]
			nl := int(key[64])
			if 66+nl*2 <= len(key) {
				u16 := make([]uint16, nl)
				for i := range u16 {
					u16[i] = binary.LittleEndian.Uint16(key[66+i*2:])
				}
				if strings.EqualFold(string(utf16.Decode(u16)), name) {
					absOff := entriesOff + off
					newHdr := make([]byte, len(hdr)-entLen)
					copy(newHdr, hdr[:absOff])
					copy(newHdr[absOff:], hdr[absOff+entLen:])
					newIdxLen := indexLen - entLen
					binary.LittleEndian.PutUint32(newHdr[4:], uint32(newIdxLen))
					binary.LittleEndian.PutUint32(newHdr[8:], uint32(newIdxLen))
					return newHdr
				}
			}
		}
		off += entLen
	}
	return hdr
}