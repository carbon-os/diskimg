package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/carbon-os/diskimg"
	"github.com/carbon-os/diskimg/fs"
	"github.com/carbon-os/diskimg/mkfs/exfat"
	"github.com/carbon-os/diskimg/mkfs/ext4"
	"github.com/carbon-os/diskimg/mkfs/fat32"
	"github.com/carbon-os/diskimg/mkfs/ntfs"
)

func main() {
	if len(os.Args) < 3 {
		printUsage()
		os.Exit(1)
	}

	imgPath := os.Args[1]
	command := os.Args[2]

	// --build is handled separately: it creates a new image file and does
	// not call Attach (there is nothing to attach yet).
	if command == "--build" {
		handleBuild(imgPath, os.Args[3:])
		return
	}

	img, err := diskimg.Attach(imgPath)
	if err != nil {
		log.Fatalf("Failed to attach image: %v", err)
	}

	defer func() {
		if err := img.Detach(""); err != nil {
			log.Fatalf("Error during detach: %v", err)
		}
	}()

	switch command {
	case "--info":
		printDiskInfo(img)

	case "--fs":
		if len(os.Args) < 5 {
			fmt.Println("Error: --fs requires a subcommand and an argument (e.g., --fs ls /)")
			printUsage()
			os.Exit(1)
		}

		fsCmd := os.Args[3]
		secondArg := os.Args[4]

		// Parse optional --partition, --subvol, and mkfs-specific flags
		// from the remaining args.
		partitionIndex := 1
		var subvol string
		args := os.Args[5:]
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--partition":
				if i+1 >= len(args) {
					log.Fatal("--partition requires a number")
				}
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n < 1 {
					log.Fatalf("Invalid partition number: %s", args[i+1])
				}
				partitionIndex = n
				args = append(args[:i], args[i+2:]...)
				i--
			case "--subvol":
				if i+1 >= len(args) {
					log.Fatal("--subvol requires a name")
				}
				subvol = args[i+1]
				args = append(args[:i], args[i+2:]...)
				i--
			}
		}

		// mkfs does not need a mounted volume; it writes raw bytes directly
		// onto the partition via OpenRaw-style section access through the image.
		if fsCmd == "mkfs" {
			handleMkfsOnImage(img, secondArg, partitionIndex, args)
			return
		}

		vol, err := img.Mount(partitionIndex, diskimg.MountOptions{Subvol: subvol})
		if err != nil {
			log.Fatalf("Failed to mount partition %d: %v", partitionIndex, err)
		}
		defer func() {
			if err := vol.Unmount(); err != nil {
				log.Fatalf("Error unmounting volume: %v", err)
			}
		}()

		if fsCmd == "subvols" {
			printSubvols(vol)
			return
		}

		handleFSCommand(vol, fsCmd, secondArg, args)

	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

// ── --build ───────────────────────────────────────────────────────────────────

// handleBuild creates a new GPT image, partitions it, and optionally formats
// each partition in a single pass.
//
// Flags:
//
//	--size <N>                     Total image size (e.g. 8G, 512M, 1073741824)
//	--add-part <type>:<size>[:<fstype>[:<label>]]
//	                               Add a partition. Repeatable; order matters.
//	                               type:  efi | linux | data
//	                               size:  human (512M, 2G) or 0 to fill rest
//	                               fstype: fat32 | ext4 | exfat | ntfs  (optional)
//	                               label: volume label                   (optional)
//	--sector-size  <N>             Logical sector size for all formatters (default 512)
//	--inode-ratio  <N>             ext4 inode ratio                       (default 16384)
//	--reserved-pct <N>             ext4 reserved block percentage         (default 5)
func handleBuild(imgPath string, rawArgs []string) {
	var (
		totalSize   int64
		partSpecs   []string
		sectorSize  = 512
		inodeRatio  = int64(16384)
		reservedPct = 5
	)

	for i := 0; i < len(rawArgs); i++ {
		switch rawArgs[i] {
		case "--size":
			if i+1 >= len(rawArgs) {
				log.Fatal("--size requires a value")
			}
			sz, err := parseSize(rawArgs[i+1])
			if err != nil {
				log.Fatalf("--size: %v", err)
			}
			totalSize = sz
			i++
		case "--add-part":
			if i+1 >= len(rawArgs) {
				log.Fatal("--add-part requires a value")
			}
			partSpecs = append(partSpecs, rawArgs[i+1])
			i++
		case "--sector-size":
			if i+1 >= len(rawArgs) {
				log.Fatal("--sector-size requires a value")
			}
			n, err := strconv.Atoi(rawArgs[i+1])
			if err != nil {
				log.Fatalf("--sector-size: %v", err)
			}
			sectorSize = n
			i++
		case "--inode-ratio":
			if i+1 >= len(rawArgs) {
				log.Fatal("--inode-ratio requires a value")
			}
			n, err := strconv.ParseInt(rawArgs[i+1], 10, 64)
			if err != nil {
				log.Fatalf("--inode-ratio: %v", err)
			}
			inodeRatio = n
			i++
		case "--reserved-pct":
			if i+1 >= len(rawArgs) {
				log.Fatal("--reserved-pct requires a value")
			}
			n, err := strconv.Atoi(rawArgs[i+1])
			if err != nil {
				log.Fatalf("--reserved-pct: %v", err)
			}
			reservedPct = n
			i++
		default:
			log.Fatalf("Unknown --build flag: %s", rawArgs[i])
		}
	}

	if totalSize == 0 {
		log.Fatal("--build requires --size")
	}
	if len(partSpecs) == 0 {
		log.Fatal("--build requires at least one --add-part")
	}

	// Parse partition specs before touching the file so we fail fast on
	// bad input.
	type partDef struct {
		guid   [16]byte
		size   int64
		fstype string
		label  string
	}
	defs := make([]partDef, 0, len(partSpecs))
	for _, spec := range partSpecs {
		fields := strings.SplitN(spec, ":", 4)
		if len(fields) < 2 {
			log.Fatalf("--add-part %q: expected at least <type>:<size>", spec)
		}
		var d partDef
		switch strings.ToLower(fields[0]) {
		case "efi":
			d.guid = diskimg.GUID_EFISystem
		case "linux":
			d.guid = diskimg.GUID_LinuxData
		case "data", "basic":
			d.guid = diskimg.GUID_BasicData
		default:
			log.Fatalf("--add-part %q: unknown partition type %q (efi | linux | data)", spec, fields[0])
		}
		sz, err := parseSize(fields[1])
		if err != nil {
			log.Fatalf("--add-part %q: bad size: %v", spec, err)
		}
		d.size = sz
		if len(fields) >= 3 {
			d.fstype = strings.ToLower(fields[2])
		}
		if len(fields) >= 4 {
			d.label = fields[3]
		}
		defs = append(defs, d)
	}

	// Create and size the image file.
	f, err := os.Create(imgPath)
	if err != nil {
		log.Fatalf("create %s: %v", imgPath, err)
	}
	defer f.Close()
	if err := f.Truncate(totalSize); err != nil {
		log.Fatalf("truncate %s: %v", imgPath, err)
	}

	builder, err := diskimg.NewBuilder(f)
	if err != nil {
		log.Fatalf("NewBuilder: %v", err)
	}

	bparts := make([]*diskimg.BuilderPartition, len(defs))
	for i, d := range defs {
		bparts[i] = builder.AddPartition(d.guid, d.size)
	}

	if err := builder.Commit(); err != nil {
		log.Fatalf("Commit: %v", err)
	}

	// Format each partition that has an fstype specified.
	for i, d := range defs {
		if d.fstype == "" {
			fmt.Printf("Partition %d (%s): no formatter requested, skipping.\n",
				bparts[i].Index, partTypeName(bparts[i].TypeGUID))
			continue
		}
		raw, err := builder.OpenRaw(bparts[i].Index)
		if err != nil {
			log.Fatalf("OpenRaw partition %d: %v", bparts[i].Index, err)
		}
		size := bparts[i].SizeBytes
		if err := runFormatter(raw, size, d.fstype, d.label, sectorSize, inodeRatio, reservedPct); err != nil {
			log.Fatalf("format partition %d as %s: %v", bparts[i].Index, d.fstype, err)
		}
		fmt.Printf("Partition %d: formatted as %s  label=%q  size=%s\n",
			bparts[i].Index, d.fstype, d.label, humanSize(size))
	}

	if err := builder.Detach(""); err != nil {
		log.Fatalf("Detach: %v", err)
	}

	fmt.Printf("\nBuilt %s (%s)\n", imgPath, humanSize(totalSize))
}

// ── --fs mkfs ─────────────────────────────────────────────────────────────────

// handleMkfsOnImage formats an existing partition inside an already-attached
// image without going through the Volume layer (raw bytes, no FS driver needed).
//
// Usage:
//
//	diskimg image.img --fs mkfs <fstype> --partition N
//	                   [--label L] [--sector-size N]
//	                   [--inode-ratio N] [--reserved-pct N]
func handleMkfsOnImage(img *diskimg.Image, fstype string, partIndex int, args []string) {
	var (
		label       string
		sectorSize  = 512
		inodeRatio  = int64(16384)
		reservedPct = 5
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--label":
			if i+1 >= len(args) {
				log.Fatal("--label requires a value")
			}
			label = args[i+1]
			i++
		case "--sector-size":
			if i+1 >= len(args) {
				log.Fatal("--sector-size requires a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				log.Fatalf("--sector-size: %v", err)
			}
			sectorSize = n
			i++
		case "--inode-ratio":
			if i+1 >= len(args) {
				log.Fatal("--inode-ratio requires a value")
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				log.Fatalf("--inode-ratio: %v", err)
			}
			inodeRatio = n
			i++
		case "--reserved-pct":
			if i+1 >= len(args) {
				log.Fatal("--reserved-pct requires a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				log.Fatalf("--reserved-pct: %v", err)
			}
			reservedPct = n
			i++
		default:
			log.Fatalf("Unknown mkfs flag: %s", args[i])
		}
	}

	// Obtain a bounded io.ReadWriteSeeker for the raw partition bytes.
	// We reach into the image's region list the same way Builder.OpenRaw does:
	// find the partition region, then wrap the underlying file.
	rws, size, err := openRawPartition(img, partIndex)
	if err != nil {
		log.Fatalf("mkfs: %v", err)
	}

	if err := runFormatter(rws, size, strings.ToLower(fstype), label, sectorSize, inodeRatio, reservedPct); err != nil {
		log.Fatalf("mkfs %s partition %d: %v", fstype, partIndex, err)
	}

	fmt.Printf("Partition %d formatted as %s  label=%q  size=%s\n",
		partIndex, fstype, label, humanSize(size))
}

// openRawPartition returns an io.ReadWriteSeeker that covers the raw bytes of
// partition partIndex inside img, plus the partition's size in bytes.
// It mirrors what Builder.OpenRaw does but works on an *Image obtained via Attach.
func openRawPartition(img *diskimg.Image, partIndex int) (io.ReadWriteSeeker, int64, error) {
	for _, p := range img.Partitions() {
		if p.Index == partIndex {
			f, err := img.RawFile()
			if err != nil {
				return nil, 0, fmt.Errorf("openRawPartition: %w", err)
			}
			rws := &sectionRWS{f: f, base: p.StartByte, size: p.SizeBytes}
			return rws, p.SizeBytes, nil
		}
	}
	return nil, 0, fmt.Errorf("partition %d not found", partIndex)
}

// ── formatter dispatch ────────────────────────────────────────────────────────

func runFormatter(
	rw io.ReadWriteSeeker,
	size int64,
	fstype, label string,
	sectorSize int,
	inodeRatio int64,
	reservedPct int,
) error {
	switch fstype {
	case "fat32":
		return fat32.Format(rw, size, fat32.Options{
			Label:      label,
			SectorSize: sectorSize,
		})
	case "ext4":
		return ext4.Format(rw, size, ext4.Options{
			Label:       label,
			InodeRatio:  inodeRatio,
			ReservedPct: reservedPct,
		})
	case "exfat":
		return exfat.Format(rw, size, exfat.Options{
			Label:      label,
			SectorSize: sectorSize,
		})
	case "ntfs":
		return ntfs.Format(rw, size, ntfs.Options{
			Label:      label,
			SectorSize: sectorSize,
		})
	default:
		return fmt.Errorf("unsupported filesystem %q (fat32 | ext4 | exfat | ntfs)", fstype)
	}
}

// ── sectionRWS ────────────────────────────────────────────────────────────────

// sectionRWS is a bounded io.ReadWriteSeeker over an *os.File.
// Position 0 of the seeker maps to byte base of f.
type sectionRWS struct {
	f    *os.File
	base int64
	size int64
	pos  int64
}

func (s *sectionRWS) Read(p []byte) (int, error) {
	if s.pos >= s.size {
		return 0, io.EOF
	}
	if int64(len(p)) > s.size-s.pos {
		p = p[:s.size-s.pos]
	}
	n, err := s.f.ReadAt(p, s.base+s.pos)
	s.pos += int64(n)
	return n, err
}

func (s *sectionRWS) Write(p []byte) (int, error) {
	if s.pos+int64(len(p)) > s.size {
		return 0, fmt.Errorf("sectionRWS: write would exceed partition boundary (%d + %d > %d)",
			s.pos, len(p), s.size)
	}
	n, err := s.f.WriteAt(p, s.base+s.pos)
	s.pos += int64(n)
	return n, err
}

func (s *sectionRWS) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = s.pos + offset
	case io.SeekEnd:
		newPos = s.size + offset
	default:
		return 0, fmt.Errorf("sectionRWS: invalid whence %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("sectionRWS: seek to negative position")
	}
	s.pos = newPos
	return s.pos, nil
}

// ── existing fs subcommands ───────────────────────────────────────────────────

func printSubvols(vol fs.Volume) {
	type subvollister interface {
		ListSubvols() ([]string, error)
	}
	lister, ok := vol.(subvollister)
	if !ok {
		log.Fatal("subvols: not a Btrfs volume (or subvol already selected)")
	}
	names, err := lister.ListSubvols()
	if err != nil {
		log.Fatalf("subvols: %v", err)
	}
	if len(names) == 0 {
		fmt.Println("No subvolumes found.")
		return
	}
	for _, n := range names {
		fmt.Println(n)
	}
}

func handleFSCommand(vol fs.Volume, cmd, path string, extraArgs []string) {
	switch cmd {
	case "ls":
		entries, err := vol.ReadDir(path)
		if err != nil {
			log.Fatalf("ls failed: %v", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "MODE\tNAME\tIS_DIR")
		for _, e := range entries {
			info, _ := e.Info()
			fmt.Fprintf(w, "%s\t%s\t%v\n", info.Mode(), e.Name(), e.IsDir())
		}
		w.Flush()

	case "mkdir":
		if err := vol.MkdirAll(path, 0755); err != nil {
			log.Fatalf("mkdir failed: %v", err)
		}
		fmt.Printf("Created directory: %s\n", path)

	case "cat":
		data, err := vol.ReadFile(path)
		if err != nil {
			log.Fatalf("cat failed: %v", err)
		}
		os.Stdout.Write(data)

	case "rm":
		if err := vol.RemoveAll(path); err != nil {
			log.Fatalf("rm failed: %v", err)
		}
		fmt.Printf("Removed: %s\n", path)

	case "put":
		if len(extraArgs) < 1 {
			log.Fatal("put requires a destination path: --fs put <host_src> <img_dest>")
		}
		hostPath := path
		imgDest := extraArgs[0]

		src, err := os.Open(hostPath)
		if err != nil {
			log.Fatalf("Failed to open host file: %v", err)
		}
		defer src.Close()

		dst, err := vol.Create(imgDest)
		if err != nil {
			log.Fatalf("Failed to create file on image: %v", err)
		}
		defer dst.Close()

		if _, err := io.Copy(dst, src); err != nil {
			log.Fatalf("Failed to copy data: %v", err)
		}
		fmt.Printf("Copied %s → %s on image.\n", hostPath, imgDest)

	default:
		log.Fatalf("Unknown fs subcommand: %s", cmd)
	}
}

// ── --info ────────────────────────────────────────────────────────────────────

func printDiskInfo(img *diskimg.Image) {
	fmt.Println("=== Partitions ===")
	partitions := img.Partitions()
	if len(partitions) == 0 {
		fmt.Println("No partitions found.")
	}

	type subvollister interface {
		ListSubvols() ([]string, error)
	}

	for _, p := range partitions {
		guidStr := ""
		if p.TypeGUID != "" {
			guidStr = fmt.Sprintf(" | GUID: %s", p.TypeGUID)
		}
		fmt.Printf("Partition %d: Start: %010d, Size: %-10d bytes  (%s)%s\n",
			p.Index, p.StartByte, p.SizeBytes, humanSize(p.SizeBytes), guidStr)

		vol, err := img.Mount(p.Index)
		if err == nil {
			fmt.Printf("  └─ Filesystem: %s\n", vol.Type())
			if vi, err := vol.StatFS(); err == nil {
				fmt.Printf("  └─ Used:       %s / %s\n",
					humanSize(vi.UsedBytes), humanSize(vi.TotalBytes))
			}
			if lister, ok := vol.(subvollister); ok {
				subs, subErr := lister.ListSubvols()
				if subErr == nil && len(subs) > 0 {
					fmt.Printf("  └─ Subvolumes: %s\n", strings.Join(subs, ", "))
				}
			}
			vol.Unmount()
		} else {
			fmt.Printf("  └─ Filesystem: unknown/unsupported\n")
		}
	}

	fmt.Println("\n=== Disk Layout (Regions) ===")
	for _, r := range img.Regions() {
		kindStr := "Unknown"
		switch r.Kind {
		case diskimg.RegionBoot:
			kindStr = "Boot (MBR/GPT Header)"
		case diskimg.RegionPartition:
			kindStr = fmt.Sprintf("Partition (%d)", r.PartitionIndex)
		case diskimg.RegionGap:
			kindStr = "Gap (Unallocated)"
		case diskimg.RegionBackup:
			kindStr = "Backup (GPT Header)"
		}
		fmt.Printf("[%010d - %010d] Size: %-12d (%s) | %s\n",
			r.Start, r.End, r.Size(), humanSize(r.Size()), kindStr)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// parseSize understands plain byte counts and IEC suffixes: K, M, G, T.
// "0" is returned as 0 (fill-rest sentinel for AddPartition).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if len(s) == 0 {
		return 0, fmt.Errorf("empty size string")
	}
	multiplier := int64(1)
	switch strings.ToUpper(string(s[len(s)-1])) {
	case "K":
		multiplier = 1 << 10
		s = s[:len(s)-1]
	case "M":
		multiplier = 1 << 20
		s = s[:len(s)-1]
	case "G":
		multiplier = 1 << 30
		s = s[:len(s)-1]
	case "T":
		multiplier = 1 << 40
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q as a size", s)
	}
	return n * multiplier, nil
}

// humanSize formats a byte count as a compact human-readable string.
func humanSize(b int64) string {
	const (
		_  = iota
		KB = 1 << (10 * iota)
		MB
		GB
		TB
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TiB", float64(b)/TB)
	case b >= GB:
		return fmt.Sprintf("%.1f GiB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.1f MiB", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.1f KiB", float64(b)/KB)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// partTypeName returns a human-friendly label for a well-known type GUID.
func partTypeName(guid [16]byte) string {
	switch guid {
	case diskimg.GUID_EFISystem:
		return "EFI System"
	case diskimg.GUID_LinuxData:
		return "Linux Data"
	case diskimg.GUID_BasicData:
		return "Basic Data"
	default:
		return "Unknown"
	}
}

// ── usage ─────────────────────────────────────────────────────────────────────

func printUsage() {
	fmt.Print(`Usage:
  diskimg <image.img> --info
      Show partition table and region layout.

  diskimg <new.img> --build --size <N> --add-part <type>:<size>[:<fstype>[:<label>]] ...
      Create a new GPT image, partition it, and optionally format each partition.
      --size          Total image size (e.g. 8G, 512M, 1073741824)
      --add-part      Repeatable; fields separated by ':':
                        type:    efi | linux | data
                        size:    e.g. 512M, 2G, or 0 to fill remaining space
                        fstype:  fat32 | ext4 | exfat | ntfs  (optional)
                        label:   volume label                  (optional)
      --sector-size   Logical sector size for formatters (default: 512)
      --inode-ratio   ext4 inode ratio                  (default: 16384)
      --reserved-pct  ext4 reserved block percentage    (default: 5)

  diskimg <image.img> --fs <command> <arg> [--partition N] [--subvol NAME]
      Run a filesystem command on a mounted partition (default partition: 1).

      Commands:
        ls     <path>               List directory
        cat    <path>               Print file to stdout
        mkdir  <path>               Create directory (with parents)
        rm     <path>               Remove file or directory (recursive)
        put    <host_src> <dst>     Copy host file into image
        subvols <any>               List Btrfs subvolumes
        mkfs   <fstype>             Format partition in place
                                    fstype: fat32 | ext4 | exfat | ntfs
                                    Extra flags: --label L  --sector-size N
                                                 --inode-ratio N  --reserved-pct N

Examples:
  # Inspect
  diskimg fedora.img --info

  # Create a Linux boot image (EFI + root)
  diskimg linux.img --build --size 8G \
    --add-part efi:512M:fat32:EFI \
    --add-part linux:0:ext4:ROOT

  # Create a Windows image
  diskimg windows.img --build --size 8G \
    --add-part efi:512M:fat32:EFI \
    --add-part data:0:ntfs:WINDOWS

  # Create a blank image then format later
  diskimg blank.img --build --size 4G \
    --add-part linux:512M \
    --add-part linux:0
  diskimg blank.img --fs mkfs ext4 --partition 1 --label BOOT
  diskimg blank.img --fs mkfs ext4 --partition 2 --label ROOT --inode-ratio 8192

  # Read / write
  diskimg ubuntu.img  --fs ls /
  diskimg fedora.img  --fs ls /var --partition 2 --subvol root
  diskimg fedora.img  --fs cat /etc/os-release --partition 2 --subvol root
  diskimg ubuntu.img  --fs put ./myfile /etc/myfile --partition 1
  diskimg ubuntu.img  --fs mkdir /etc/myapp --partition 1
  diskimg ubuntu.img  --fs rm /etc/myapp --partition 1
  diskimg fedora.img  --fs subvols . --partition 2
`)
}