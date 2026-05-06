// cmd/main.go
package main

import (
	"archive/tar"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/carbon-os/diskimg"
)

const usage = `img2tar — inspect and extract raw disk images

USAGE:
  img2tar info   <image.img>
  img2tar extract <image.img> [flags]
  img2tar rebuild <image.img> [flags]

COMMANDS:
  info      Print partition table and slice map
  extract   Extract a partition to a .tar file (default: partition 1)
  rebuild   Replace a partition with a .tar file and write a new image

FLAGS (extract / rebuild):
  -p  int     Partition number (1-based, default 1)
  -o  string  Output file  (extract: foo.tar | rebuild: new.img)
  -i  string  Input tar file for rebuild
  -v          Verbose: print every file as it is written
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	partNum := fs.Int("p", 1, "partition number (1-based)")
	outFile := fs.String("o", "", "output file")
	inFile  := fs.String("i", "", "input tar file (rebuild only)")
	verbose := fs.Bool("v", false, "verbose output")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	// The first positional arg after the sub-command is always the image path.
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: missing <image.img>")
		fs.Usage()
		os.Exit(1)
	}
	imgPath := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(1)
	}

	switch cmd {
	case "info":
		runInfo(imgPath)
	case "extract":
		runExtract(imgPath, *partNum, *outFile, *verbose)
	case "rebuild":
		runRebuild(imgPath, *partNum, *outFile, *inFile)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

// ── info ─────────────────────────────────────────────────────────────────────

func runInfo(imgPath string) {
	img, err := diskimg.Open(imgPath)
	must("open image", err)
	defer img.Close()

	fmt.Printf("Image : %s\n", imgPath)
	fmt.Printf("Size  : %s (%d bytes)\n", humanBytes(img.Size()), img.Size())
	fmt.Printf("Table : %s\n", img.TableType())
	fmt.Printf("Sector: %d bytes\n\n", img.SectorSize())

	// Partition table
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSTART\tSIZE\tFILESYSTEM\tNAME")
	for _, p := range img.Partitions() {
		ft, _ := img.DetectFilesystem(p.Index)
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			p.Index,
			humanBytes(p.StartBytes),
			humanBytes(p.SizeBytes),
			ft,
			p.Name,
		)
	}
	tw.Flush()

	// Slice map
	fmt.Println()
	tw2 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw2, "KIND\tSTART\tEND\tSIZE")
	for _, sl := range img.Slices() {
		label := sliceLabel(sl)
		fmt.Fprintf(tw2, "%s\t0x%08X\t0x%08X\t%s\n",
			label, sl.Start, sl.End, humanBytes(sl.Size()))
	}
	tw2.Flush()
}

func sliceLabel(sl *diskimg.Slice) string {
	switch sl.Kind {
	case diskimg.SliceKindGap:
		return "gap (boot)"
	case diskimg.SliceKindPartition:
		return fmt.Sprintf("partition %d", sl.PartitionIndex)
	case diskimg.SliceKindBetween:
		return "gap (between)"
	case diskimg.SliceKindTail:
		return "tail (GPT backup)"
	default:
		return "unknown"
	}
}

// ── extract ───────────────────────────────────────────────────────────────────

func runExtract(imgPath string, partNum int, outPath string, verbose bool) {
	if outPath == "" {
		outPath = fmt.Sprintf("partition%d.tar", partNum)
	}

	img, err := diskimg.Open(imgPath)
	must("open image", err)
	defer img.Close()

	f, err := os.Create(outPath)
	must("create tar", err)
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	fmt.Printf("Extracting partition %d → %s\n", partNum, outPath)
	must("extract", img.ExtractPartition(partNum, tw, verbose))
	must("tar flush", tw.Flush())
	fmt.Printf("Done: %s\n", outPath)
}

// ── rebuild ───────────────────────────────────────────────────────────────────

func runRebuild(imgPath string, partNum int, outPath, inPath string) {
	if inPath == "" {
		fmt.Fprintln(os.Stderr, "error: -i <input.tar> is required for rebuild")
		os.Exit(1)
	}
	if outPath == "" {
		outPath = "rebuilt.img"
	}

	img, err := diskimg.Open(imgPath)
	must("open image", err)
	defer img.Close()

	in, err := os.Open(inPath)
	must("open tar", err)
	defer in.Close()

	fmt.Printf("Rebuilding %s (partition %d ← %s) → %s\n",
		imgPath, partNum, inPath, outPath)
	must("rebuild", img.Rebuild(outPath, partNum, in))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func must(label string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", label, err)
		os.Exit(1)
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}