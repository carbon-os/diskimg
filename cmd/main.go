package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"text/tabwriter"

	"github.com/carbon-os/diskimg"
)

func main() {
	if len(os.Args) < 3 {
		printUsage()
		os.Exit(1)
	}

	imgPath := os.Args[1]
	command := os.Args[2]

	// Attach opens the named disk image and parses its partition table[cite: 1].
	// No data is read into memory; it only builds the region map[cite: 10].
	img, err := diskimg.Attach(imgPath)
	if err != nil {
		log.Fatalf("Failed to attach image: %v", err)
	}

	// Ensure we detach and flush any changes in-place before exiting[cite: 4, 10].
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

		// Mount partition 1 by default. This detects the filesystem (e.g., Ext4, FAT32) 
		// and returns a Volume backed by a SectionReader[cite: 3, 10].
		vol, err := img.Mount(1)
		if err != nil {
			log.Fatalf("Failed to mount partition 1: %v", err)
		}
		
		// Unmount flushes the journal, syncs dirty blocks, and releases the volume[cite: 5, 10].
		defer func() {
			if err := vol.Unmount(); err != nil {
				log.Fatalf("Error unmounting volume: %v", err)
			}
		}()

		handleFSCommand(vol, fsCmd, targetPath)

	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

// handleFSCommand mirrors standard os.* operations through the unified Volume interface[cite: 5, 10].
func handleFSCommand(vol diskimg.Volume, cmd, path string) {
	switch cmd {
	case "ls":
		// ReadDir reads the directory named by name and returns sorted entries[cite: 5, 10].
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
		// MkdirAll creates a directory and all parents as needed[cite: 5, 10].
		if err := vol.MkdirAll(path, 0755); err != nil {
			log.Fatalf("mkdir failed: %v", err)
		}
		fmt.Printf("Created directory: %s\n", path)

	case "cat":
		// ReadFile reads the named file and returns its contents[cite: 5, 10].
		// For larger files, you would use vol.Open() to stream it[cite: 10].
		data, err := vol.ReadFile(path)
		if err != nil {
			log.Fatalf("cat failed: %v", err)
		}
		os.Stdout.Write(data)

	case "rm":
		// RemoveAll removes a path and all children, like rm -rf[cite: 5, 10].
		if err := vol.RemoveAll(path); err != nil {
			log.Fatalf("rm failed: %v", err)
		}
		fmt.Printf("Removed: %s\n", path)
		
	case "put":
		// Example: ./diskimg yourimg.img --fs put hostfile.txt /destfile.txt
		if len(os.Args) < 6 {
			log.Fatal("put requires a source file from the host: --fs put <host_src> <img_dest>")
		}
		hostPath := os.Args[4]
		imgDest := os.Args[5]
		
		src, err := os.Open(hostPath)
		if err != nil {
			log.Fatalf("Failed to open host file: %v", err)
		}
		defer src.Close()
		
		// Create creates or truncates the named file, returning a handle for streaming[cite: 5, 10].
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
	// Partitions returns the parsed partition list[cite: 1].
	partitions := img.Partitions()
	if len(partitions) == 0 {
		fmt.Println("No partitions found.")
	}
	for _, p := range partitions {
		// Uses the Partition struct from the partition package[cite: 7].
		guidStr := ""
		if p.TypeGUID != "" {
			guidStr = fmt.Sprintf(" | GUID: %s", p.TypeGUID)
		}
		fmt.Printf("Partition %d: Start: %010d, Size: %-10d bytes%s\n", p.Index, p.StartByte, p.SizeBytes, guidStr)
	}

	fmt.Println("\n=== Disk Layout (Regions) ===")
	// Regions returns the ordered region map[cite: 1].
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
		// Uses the Region struct from the region package[cite: 2].
		fmt.Printf("[%010d - %010d] Size: %-10d | %s\n", r.Start, r.End, r.Size(), kindStr)
	}
}

func printUsage() {
	fmt.Println(`Usage:
  diskimg <image.img> --info
      Shows partition tables and region layout.

  diskimg <image.img> --fs <command> <path> [args...]
      Executes filesystem commands on Partition 1.

  Available --fs commands:
      ls <path>           List directory contents
      mkdir <path>        Create directory and parents
      cat <path>          Print file contents to stdout
      rm <path>           Remove file or directory (recursive)
      put <src> <dest>    Stream host file into the image partition`)
}