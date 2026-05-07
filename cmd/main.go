package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/carbon-os/diskimg"
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

		// Parse optional --partition N from remaining args.
		partitionIndex := 1
		args := os.Args[5:]
		for i := 0; i < len(args); i++ {
			if args[i] == "--partition" && i+1 < len(args) {
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n < 1 {
					log.Fatalf("Invalid partition number: %s", args[i+1])
				}
				partitionIndex = n
				// Remove --partition N from args so put's host/dest aren't affected.
				args = append(args[:i], args[i+2:]...)
				break
			}
		}

		vol, err := img.Mount(partitionIndex)
		if err != nil {
			log.Fatalf("Failed to mount partition %d: %v", partitionIndex, err)
		}

		defer func() {
			if err := vol.Unmount(); err != nil {
				log.Fatalf("Error unmounting volume: %v", err)
			}
		}()

		handleFSCommand(vol, fsCmd, targetPath, args)

	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

// handleFSCommand mirrors standard os.* operations through the unified Volume interface.
// extraArgs holds any arguments beyond <path> (e.g. the host src for "put").
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
		hostPath := path       // os.Args[4] is the host src for "put"
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
		fmt.Printf("Partition %d: Start: %010d, Size: %-10d bytes%s\n", p.Index, p.StartByte, p.SizeBytes, guidStr)
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
		fmt.Printf("[%010d - %010d] Size: %-10d | %s\n", r.Start, r.End, r.Size(), kindStr)
	}
}

func printUsage() {
	fmt.Println(`Usage:
  diskimg <image.img> --info
      Shows partition tables and region layout.

  diskimg <image.img> --fs <command> <path> [--partition N]
      Executes filesystem commands on the specified partition (default: 1).

  Available --fs commands:
      ls <path>                        List directory contents
      mkdir <path>                     Create directory and parents
      cat <path>                       Print file contents to stdout
      rm <path>                        Remove file or directory (recursive)
      put <host_src> <img_dest>        Stream host file into the image

  Examples:
      diskimg ubuntu.img --fs ls /
      diskimg ubuntu.img --fs ls /boot --partition 16
      diskimg ubuntu.img --fs cat /etc/os-release --partition 1
      diskimg ubuntu.img --fs put ./myfile /etc/myfile --partition 1`)
}