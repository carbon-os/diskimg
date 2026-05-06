package fat

import (
	"archive/tar"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Extract walks the FAT32 filesystem and writes all entries to tw.
func Extract(fs *FS, tw *tar.Writer, verbose bool) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "./",
		Mode:     0755,
	}); err != nil {
		return err
	}
	return fs.walkDir(tw, fs.rootCluster, "", verbose)
}

// dirEntry is a parsed 32-byte FAT32 directory entry.
type dirEntry struct {
	name     string
	isDir    bool
	cluster  uint32
	size     uint32
	modTime  time.Time
}

func (fs *FS) walkDir(tw *tar.Writer, firstCluster uint32, prefix string, verbose bool) error {
	chain, err := fs.readClusterChain(firstCluster)
	if err != nil {
		return err
	}

	var entries []dirEntry
	for _, cl := range chain {
		data := make([]byte, fs.clusterSize)
		if _, err := fs.r.ReadAt(data, fs.clusterOffset(cl)); err != nil {
			return fmt.Errorf("fat32: read dir cluster %d: %w", cl, err)
		}
		entries = append(entries, parseDirCluster(data)...)
	}

	for _, e := range entries {
		relPath := prefix + e.name
		tarName := "./" + relPath

		if e.isDir {
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     tarName + "/",
				Mode:     0755,
				ModTime:  e.modTime,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if verbose {
				fmt.Printf("  d  %s/\n", relPath)
			}
			if err := fs.walkDir(tw, e.cluster, relPath+"/", verbose); err != nil {
				return err
			}
		} else {
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     tarName,
				Size:     int64(e.size),
				Mode:     0644,
				ModTime:  e.modTime,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if err := fs.writeFileData(tw, e.cluster, int64(e.size)); err != nil {
				fmt.Fprintf(os.Stderr, "  [warn] fat32 copy %q: %v\n", relPath, err)
			} else if verbose {
				fmt.Printf("  f  %s (%d bytes)\n", relPath, e.size)
			}
		}
	}
	return nil
}

func (fs *FS) writeFileData(w io.Writer, firstCluster uint32, size int64) error {
	chain, err := fs.readClusterChain(firstCluster)
	if err != nil {
		return err
	}
	remaining := size
	for _, cl := range chain {
		if remaining <= 0 {
			break
		}
		data := make([]byte, fs.clusterSize)
		if _, err := fs.r.ReadAt(data, fs.clusterOffset(cl)); err != nil {
			return err
		}
		write := int64(len(data))
		if write > remaining {
			write = remaining
		}
		if _, err := w.Write(data[:write]); err != nil {
			return err
		}
		remaining -= write
	}
	return nil
}

// parseDirCluster parses 32-byte directory entries from one cluster of data.
// Skips deleted entries (first byte 0xE5), LFN entries (attr=0x0F), and . / ..
func parseDirCluster(data []byte) []dirEntry {
	var out []dirEntry
	for i := 0; i+32 <= len(data); i += 32 {
		e := data[i : i+32]
		if e[0] == 0x00 {
			break // end of directory
		}
		if e[0] == 0xE5 {
			continue // deleted
		}
		attr := e[11]
		if attr == 0x0F {
			continue // LFN entry
		}
		if attr&0x08 != 0 {
			continue // volume label
		}

		name := parseSFN(e[0:11])
		if name == "." || name == ".." || name == "" {
			continue
		}

		clusterHi := binary.LittleEndian.Uint16(e[20:22])
		clusterLo := binary.LittleEndian.Uint16(e[26:28])
		cluster := uint32(clusterHi)<<16 | uint32(clusterLo)
		size := binary.LittleEndian.Uint32(e[28:32])

		out = append(out, dirEntry{
			name:    name,
			isDir:   attr&0x10 != 0,
			cluster: cluster,
			size:    size,
			modTime: fatTime(e[22:24], e[24:26]),
		})
	}
	return out
}

// parseSFN converts an 8.3 short filename to a standard string.
func parseSFN(b []byte) string {
	base := strings.TrimRight(string(b[0:8]), " ")
	ext  := strings.TrimRight(string(b[8:11]), " ")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// fatTime decodes FAT date and time fields into a time.Time.
func fatTime(timeB, dateB []byte) time.Time {
	t := binary.LittleEndian.Uint16(timeB)
	d := binary.LittleEndian.Uint16(dateB)
	sec  := int(t&0x1F) * 2
	min  := int((t >> 5) & 0x3F)
	hour := int(t >> 11)
	day  := int(d & 0x1F)
	mon  := int((d >> 5) & 0x0F)
	year := int(d>>9) + 1980
	return time.Date(year, time.Month(mon), day, hour, min, sec, 0, time.UTC)
}