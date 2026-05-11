package ntfs

import "time"

// rootDirRef is the NTFS file reference for the root directory (record 5, seq 1).
const rootDirRef = uint64(5) | uint64(1)<<48

// buildMFT builds all numSysRecords MFT file records and returns the raw bytes.
func buildMFT(l *fsLayout, now time.Time, label string) []byte {
	buf := make([]byte, numSysRecords*mftRecordSize)

	builders := [numSysRecords]func(*fsLayout, time.Time) *mftRecord{
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec0(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec1(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec2(l, t) },
		nil, // record 3 needs label — handled separately below
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec4(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec5(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec6(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec7(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec8(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec9(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec10(l, t) },
		func(l *fsLayout, t time.Time) *mftRecord { return buildRec11(l, t) },
	}

	for i := 0; i < numSysRecords; i++ {
		var rec *mftRecord
		switch {
		case i == 3:
			rec = buildRec3(l, now, label)
		case i < len(builders) && builders[i] != nil:
			rec = builders[i](l, now)
		default:
			rec = buildFreeRecord(uint32(i))
		}
		copy(buf[i*mftRecordSize:], rec.finalize())
	}
	return buf
}

// ── Record 0: $MFT ───────────────────────────────────────────────────────────

func buildRec0(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(0, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		l.mftClusters*l.clusterSize, int64(numSysRecords*mftRecordSize),
		faHidden|faSystem, "$MFT"))

	r.appendNonResident(attrData,
		l.mftLCN, l.mftClusters, l.clusterSize,
		int64(numSysRecords*mftRecordSize))

	// $BITMAP: which MFT records are in use (records 0–11; 12–23 = free).
	// 24 records → 3 bytes: 0xFF 0x0F 0x00
	r.appendResident(attrBitmap, []byte{0xFF, 0x0F, 0x00})

	return r
}

// ── Record 1: $MFTMirr ───────────────────────────────────────────────────────

func buildRec1(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(1, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	dataSize := int64(4 * mftRecordSize) // mirror holds first 4 MFT records
	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		l.mftMirrClusters*l.clusterSize, dataSize, faHidden|faSystem, "$MFTMirr"))

	r.appendNonResident(attrData,
		l.mftMirrLCN, l.mftMirrClusters, l.clusterSize, dataSize)

	return r
}

// ── Record 2: $LogFile ───────────────────────────────────────────────────────

func buildRec2(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(2, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		l.logFileClusters*l.clusterSize, l.logFileBytes,
		faHidden|faSystem, "$LogFile"))

	r.appendNonResident(attrData,
		l.logFileLCN, l.logFileClusters, l.clusterSize, l.logFileBytes)

	return r
}

// ── Record 3: $Volume ────────────────────────────────────────────────────────

func buildRec3(l *fsLayout, t time.Time, label string) *mftRecord {
	r := newMFTRecord(3, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		0, 0, faHidden|faSystem, "$Volume"))

	// $VOLUME_NAME — may be empty if no label supplied.
	if label != "" {
		r.appendResident(attrVolumeName, toUTF16LE(label))
	}

	// $VOLUME_INFORMATION — NTFS version 3.1.
	r.appendResident(attrVolumeInfo, buildVolumeInfoAttr())

	return r
}

// ── Record 4: $AttrDef ───────────────────────────────────────────────────────

func buildRec4(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(4, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	dataSize := int64(attrDefCount * attrDefEntrySize)
	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		l.attrDefClusters*l.clusterSize, dataSize, faHidden|faSystem, "$AttrDef"))

	r.appendNonResident(attrData,
		l.attrDefLCN, l.attrDefClusters, l.clusterSize, dataSize)

	return r
}

// ── Record 5: . (root directory) ─────────────────────────────────────────────

func buildRec5(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(5, 1, 1, mftInUse|mftDir)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faArchive))

	// Root directory is its own parent.
	selfRef := fileRef(5, 1)
	r.appendResident(attrFileName,
		buildFileName(selfRef, t, 0, 0, faArchive, "."))

	// $INDEX_ROOT for $I30 (filename index), empty directory.
	indexAllocSize := l.clusterSize
	if indexAllocSize < 4096 {
		indexAllocSize = 4096
	}
	var clustersPerIdx uint8
	if l.clusterSize >= 4096 {
		clustersPerIdx = 1
	} else {
		clustersPerIdx = uint8(4096 / l.clusterSize)
	}
	r.appendResident(attrIndexRoot, buildIndexRoot(indexAllocSize, clustersPerIdx))

	return r
}

// ── Record 6: $Bitmap ────────────────────────────────────────────────────────

func buildRec6(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(6, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		l.bitmapClusters*l.clusterSize, l.bitmapBytes,
		faHidden|faSystem, "$Bitmap"))

	r.appendNonResident(attrData,
		l.bitmapLCN, l.bitmapClusters, l.clusterSize, l.bitmapBytes)

	return r
}

// ── Record 7: $Boot ──────────────────────────────────────────────────────────

func buildRec7(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(7, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	bootDataSize := l.bootClusters * l.clusterSize
	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		bootDataSize, bootDataSize, faHidden|faSystem, "$Boot"))

	// $DATA points to LCN 0 (the start of the volume).
	r.appendNonResident(attrData, 0, l.bootClusters, l.clusterSize, bootDataSize)

	return r
}

// ── Record 8: $BadClus ───────────────────────────────────────────────────────

func buildRec8(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(8, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		0, 0, faHidden|faSystem, "$BadClus"))

	// Empty unnamed data stream
	r.appendResident(attrData, nil)

	return r
}

// ── Record 9: $Secure ────────────────────────────────────────────────────────

func buildRec9(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(9, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		0, 0, faHidden|faSystem, "$Secure"))

	// For a basic format without advanced security descriptors, leaving it minimal.
	r.appendResident(attrData, nil)

	return r
}

// ── Record 10: $UpCase ───────────────────────────────────────────────────────

func buildRec10(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(10, 1, 1, mftInUse)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	dataSize := int64(131072) // 128KB upcase table
	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		l.upcaseClusters*l.clusterSize, dataSize, faHidden|faSystem, "$UpCase"))

	r.appendNonResident(attrData,
		l.upcaseLCN, l.upcaseClusters, l.clusterSize, dataSize)

	return r
}

// ── Record 11: $Extend ───────────────────────────────────────────────────────

func buildRec11(l *fsLayout, t time.Time) *mftRecord {
	r := newMFTRecord(11, 1, 1, mftInUse|mftDir)

	r.appendResident(attrStandardInfo, buildStdInfo(t, faHidden|faSystem))

	r.appendResident(attrFileName, buildFileName(rootDirRef, t,
		0, 0, faHidden|faSystem, "$Extend"))

	// Minimal index root for directory
	indexAllocSize := l.clusterSize
	if indexAllocSize < 4096 {
		indexAllocSize = 4096
	}
	var clustersPerIdx uint8
	if l.clusterSize >= 4096 {
		clustersPerIdx = 1
	} else {
		clustersPerIdx = uint8(4096 / l.clusterSize)
	}
	r.appendResident(attrIndexRoot, buildIndexRoot(indexAllocSize, clustersPerIdx))

	return r
}

// ── Free Records (12-23) ─────────────────────────────────────────────────────

func buildFreeRecord(recNum uint32) *mftRecord {
	// Flags = 0 means not in use. Sequence number initialized to 0.
	return newMFTRecord(recNum, 0, 0, 0)
}