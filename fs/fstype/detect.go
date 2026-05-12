// Package fstype identifies filesystem types from magic bytes.
package fstype

// Type names a filesystem variant.
type Type string

const (
	Ext4    Type = "ext4"
	Btrfs   Type = "btrfs"
	XFS     Type = "xfs"
	NTFS    Type = "ntfs"
	FAT32   Type = "fat32"
	FAT16   Type = "fat16"
	FAT12   Type = "fat12"
	Unknown Type = "unknown"
)

// Detect reads magic bytes via the supplied reader to identify the
// filesystem occupying the partition.  readAt mirrors io.ReaderAt.
func Detect(readAt func(off int64, buf []byte) error) Type {
	// ── Btrfs ────────────────────────────────────────────────────────────────
	// Primary superblock at 0x10000; magic "_BHRfS_M" at +0x40.
	var bm [8]byte
	if readAt(0x10040, bm[:]) == nil && string(bm[:]) == "_BHRfS_M" {
		return Btrfs
	}

	// ── ext4 ─────────────────────────────────────────────────────────────────
	// Superblock starts at byte 1024; magic uint16-LE is at +56 → byte 1080.
	var m [2]byte
	if readAt(1080, m[:]) == nil && m[0] == 0x53 && m[1] == 0xEF {
		return Ext4
	}

	// ── XFS ──────────────────────────────────────────────────────────────────
	// Primary superblock at byte 0; magic "XFSB" (0x58465342), big-endian.
	var xm [4]byte
	if readAt(0, xm[:]) == nil &&
		xm[0] == 0x58 && xm[1] == 0x46 && xm[2] == 0x53 && xm[3] == 0x42 {
		return XFS
	}

	// ── NTFS ─────────────────────────────────────────────────────────────────
	// Boot sector OEM ID field (offset 3, 8 bytes) is "NTFS    " (4 spaces).
	var oem [8]byte
	if readAt(3, oem[:]) == nil && string(oem[:]) == "NTFS    " {
		return NTFS
	}

	// ── FAT family ───────────────────────────────────────────────────────────
	// Boot sector validity: bytes 510-511 must be 55 AA.
	var sig [2]byte
	if readAt(510, sig[:]) != nil || sig[0] != 0x55 || sig[1] != 0xAA {
		return Unknown
	}

	// FAT32 extended BPB has "FAT32   " at offset 82.
	var label [8]byte
	if readAt(82, label[:]) == nil && string(label[:5]) == "FAT32" {
		return FAT32
	}
	// FAT12/16 have "FAT" at offset 54.
	if readAt(54, label[:3]) == nil && string(label[:3]) == "FAT" {
		return FAT16
	}
	return Unknown
}