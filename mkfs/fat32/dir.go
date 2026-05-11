package fat32

import (
	"fmt"
	"io"
	"strings"
)

// PadLabel returns s uppercased and padded/truncated to exactly 11 bytes.
// Exported so callers can validate labels before formatting.
func PadLabel(s string) []byte {
	if s == "" {
		s = "NO NAME"
	}
	b := make([]byte, 11)
	for i := range b {
		b[i] = ' '
	}
	copy(b, strings.ToUpper(s))
	return b
}

// writeRootDir writes the root directory cluster (cluster 2) to w.
func writeRootDir(w io.Writer, ss int, spc uint8, label string) error {
	clusterBytes := int(spc) * ss
	buf := make([]byte, clusterBytes)
	if label != "" {
		writeVolumeLabel(buf, label)
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("fat32: write root directory: %w", err)
	}
	return nil
}

// writeVolumeLabel writes an ATTR_VOLUME_ID entry into the first 32 bytes of buf.
func writeVolumeLabel(buf []byte, label string) {
	if len(buf) < 32 {
		return
	}
	copy(buf[:11], PadLabel(label))
	buf[11] = 0x08 // ATTR_VOLUME_ID
}