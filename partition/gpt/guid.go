package gpt

import (
	"encoding/binary"
	"fmt"
)

// GUID represents a 16-byte GUID in its mixed-endian on-disk form.
// The first three fields are stored little-endian; the last two big-endian.
//
// On-disk layout for "C12A7328-F81F-11D2-BA4B-00A0C93EC93B":
//   28 73 2A C1  1F F8  D2 11  BA 4B  00 A0 C9 3E C9 3B
type GUID [16]byte

// String formats the GUID as the canonical "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX" string.
func (g GUID) String() string {
	a := binary.LittleEndian.Uint32(g[0:4])
	b := binary.LittleEndian.Uint16(g[4:6])
	c := binary.LittleEndian.Uint16(g[6:8])
	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X",
		a, b, c,
		binary.BigEndian.Uint16(g[8:10]),
		g[10:16])
}

// IsZero reports whether all bytes are zero (unused GPT entry).
func (g GUID) IsZero() bool {
	for _, b := range g {
		if b != 0 {
			return false
		}
	}
	return true
}

// Well-known partition type GUIDs.
const (
	GUIDLinuxData  = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	GUIDLinuxSwap  = "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F"
	GUIDEFISystem  = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	GUIDBIOSBoot   = "21686148-6449-6E6F-744E-656564454649"
	GUIDLinuxHome  = "933AC7E1-2EB4-4F13-B844-0E14E2AEF915"
)