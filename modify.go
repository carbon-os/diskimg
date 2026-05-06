package diskimg

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/carbon-os/diskimg/ext4/mkfs"
)

// ModifyPartition extracts partition partNum, lets fn inject or override
// entries, then rebuilds the partition as a fresh ext4 image at outPath.
//
// fn receives a *tar.Writer it can write to freely:
//   - Writing a path that already exists replaces the original file.
//   - Writing a path named ".wh.<basename>" removes <basename> from the image.
//
// The partition size is preserved exactly; only the filesystem contents change.
// Only ext4 source partitions are supported.
func (img *Image) ModifyPartition(partNum int, outPath string, fn func(*tar.Writer) error) error {
	p, err := img.partition(partNum)
	if err != nil {
		return err
	}

	// Extract the existing partition.
	var baseBuf bytes.Buffer
	baseTW := tar.NewWriter(&baseBuf)
	if err := img.ExtractPartition(partNum, baseTW, false); err != nil {
		return fmt.Errorf("diskimg: ModifyPartition: extract: %w", err)
	}
	baseTW.Close()

	// Collect the caller's patch entries.
	var patchBuf bytes.Buffer
	patchTW := tar.NewWriter(&patchBuf)
	if err := fn(patchTW); err != nil {
		return fmt.Errorf("diskimg: ModifyPartition: patch fn: %w", err)
	}
	patchTW.Close()

	// Merge base + patch.
	merged, err := mergeTars(bytes.NewReader(baseBuf.Bytes()), bytes.NewReader(patchBuf.Bytes()))
	if err != nil {
		return fmt.Errorf("diskimg: ModifyPartition: merge: %w", err)
	}

	// Build a fresh ext4 image the same size as the original partition.
	tr := tar.NewReader(merged)
	newPart, err := mkfs.Build(tr, p.SizeBytes)
	if err != nil {
		return fmt.Errorf("diskimg: ModifyPartition: mkfs.Build: %w", err)
	}

	return img.Rebuild(outPath, partNum, newPart)
}

// Remove writes a whiteout entry for filePath into tw.  When tw is later
// passed to ModifyPartition the corresponding file is omitted from the rebuilt
// partition.
//
// Example:
//
//	img.ModifyPartition(1, "new.img", func(tw *tar.Writer) error {
//	    diskimg.Remove(tw, "etc/motd")
//	    return nil
//	})
func Remove(tw *tar.Writer, filePath string) error {
	dir, base := path.Split(path.Clean(filePath))
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     dir + ".wh." + base,
		Size:     0,
	})
}

// mergeTars merges a base and a patch tar stream into a single tar stream.
//
// Rules:
//   - Patch entries for a path override the base entry for the same path.
//   - A patch entry named ".wh.<name>" removes "<name>" from the base.
//   - The output is sorted so parent directories always precede their children.
func mergeTars(base, patch io.Reader) (io.Reader, error) {
	type ent struct {
		hdr  *tar.Header
		data []byte
	}

	// Preserve insertion order for de-dup while building the merged set.
	var order []string
	entries := map[string]ent{}

	load := func(r io.Reader, applyWhiteouts bool) error {
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			p := normPath(hdr.Name)

			// Process whiteout entries from the patch layer.
			if applyWhiteouts {
				base := path.Base(p)
				if strings.HasPrefix(base, ".wh.") {
					victim := normPath(path.Join(path.Dir(p), strings.TrimPrefix(base, ".wh.")))
					delete(entries, victim)
					continue
				}
			}

			var data []byte
			if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
				data = make([]byte, hdr.Size)
				if _, err := io.ReadFull(tr, data); err != nil {
					return fmt.Errorf("mergeTars: read %q: %w", hdr.Name, err)
				}
			}
			if _, exists := entries[p]; !exists {
				order = append(order, p)
			}
			entries[p] = ent{hdr: hdr, data: data}
		}
		return nil
	}

	if err := load(base, false); err != nil {
		return nil, err
	}
	if err := load(patch, true); err != nil {
		return nil, err
	}

	// Deduplicate order, dropping entries removed by whiteouts.
	seen := map[string]bool{}
	paths := order[:0]
	for _, p := range order {
		e, alive := entries[p]
		if !seen[p] && alive && e.hdr != nil {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	// Sort: root ("") first, then lexicographic — parents naturally precede children.
	sort.Slice(paths, func(i, j int) bool {
		a, b := paths[i], paths[j]
		if a == "" {
			return true
		}
		if b == "" {
			return false
		}
		return a < b
	})

	// Write the merged tar.
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, p := range paths {
		e := entries[p]
		if err := tw.WriteHeader(e.hdr); err != nil {
			return nil, err
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				return nil, err
			}
		}
	}
	tw.Close()

	return bytes.NewReader(out.Bytes()), nil
}

// normPath canonicalises a tar path: strips leading "./" or "/", collapses
// ".." components, and returns "" for the root entry.
func normPath(s string) string {
	s = path.Clean(s)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	if s == "." {
		return ""
	}
	return s
}