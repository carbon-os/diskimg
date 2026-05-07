package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/carbon-os/diskimg"
	"github.com/carbon-os/diskimg/btrfs"
	"github.com/carbon-os/diskimg/fs"
)

func main() {
	if len(os.Args) < 3 {
		printUsage()
		os.Exit(1)
	}

	imgPath := os.Args[1]
	command := os.Args[2]

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
			fmt.Println("Error: --fs requires a subcommand and a path (e.g., --fs ls /)")
			printUsage()
			os.Exit(1)
		}

		fsCmd := os.Args[3]
		targetPath := os.Args[4]

		// Parse optional --partition N and --subvol NAME from remaining args.
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

		vol, err := img.Mount(partitionIndex, diskimg.MountOptions{Subvol: subvol})
		if err != nil {
			log.Fatalf("Failed to mount partition %d: %v", partitionIndex, err)
		}

		defer func() {
			if err := vol.Unmount(); err != nil {
				log.Fatalf("Error unmounting volume: %v", err)
			}
		}()

		// Special case: list available subvolumes on a Btrfs partition.
		if fsCmd == "subvols" {
			printSubvols(vol)
			return
		}

		handleFSCommand(vol, fsCmd, targetPath, args)

	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

// printSubvols lists Btrfs subvolumes if the volume supports it.
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

// handleFSCommand mirrors standard os.* operations through the unified Volume interface.
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
			log.Fatal("put requires a source file from the host: --fs put <host_src> <img_dest>")
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
		fmt.Printf("Copied %s to %s on the disk image.\n", hostPath, imgDest)

	default:
		log.Fatalf("Unknown fs subcommand: %s", cmd)
	}
}

func printDiskInfo(img *diskimg.Image) {
	fmt.Println("=== Partitions ===")
	partitions := img.Partitions()
	if len(partitions) == 0 {
		fmt.Println("No partitions found.")
	}
	for _, p := range partitions {
		guidStr := ""
		if p.TypeGUID != "" {
			guidStr = fmt.Sprintf(" | GUID: %s", p.TypeGUID)
		}
		fmt.Printf("Partition %d: Start: %010d, Size: %-10d bytes%s\n",
			p.Index, p.StartByte, p.SizeBytes, guidStr)
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
		fmt.Printf("[%010d - %010d] Size: %-10d | %s\n",
			r.Start, r.End, r.Size(), kindStr)
	}
}

func printUsage() {
	fmt.Println(`Usage:
  diskimg <image.img> --info
      Shows partition tables and region layout.

  diskimg <image.img> --fs <command> <path> [--partition N] [--subvol NAME]
      Executes filesystem commands on the specified partition (default: 1).
      --subvol selects a Btrfs subvolume (e.g. "root" or "home").

  Available --fs commands:
      ls <path>                        List directory contents
      mkdir <path>                     Create directory and parents
      cat <path>                       Print file contents to stdout
      rm <path>                        Remove file or directory (recursive)
      put <host_src> <img_dest>        Stream host file into the image
      subvols <any>                    List Btrfs subvolumes on the partition

  Examples:
      diskimg fedora.img --fs ls / --partition 4
      diskimg fedora.img --fs ls / --partition 4 --subvol root
      diskimg fedora.img --fs ls /var --partition 4 --subvol root
      diskimg fedora.img --fs subvols . --partition 4
      diskimg fedora.img --fs cat /etc/os-release --partition 4 --subvol root
      diskimg ubuntu.img --fs put ./myfile /etc/myfile --partition 1`)
}