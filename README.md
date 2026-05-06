# diskimg

Pure-Go disk image manipulation. Attach, mount, edit, detach. 
No root. No kernel. No VM. Works natively on Linux, macOS, and Windows[cite: 10].

`diskimg` allows you to programmatically or via CLI interact with disk images (MBR and GPT)[cite: 1]. It parses partition tables, detects filesystems (ext4, FAT32/16/12)[cite: 6], and lets you stream reads and writes directly to partitions without ever loading the full image into memory[cite: 10]. 

## Features

* **Zero Root/Kernel Dependencies:** Does not require loopback devices (`losetup`), `mount`, or elevated privileges[cite: 10].
* **Ultra-Low Memory Footprint:** Reads and writes use a fixed `SectionReader` and 32KB buffers[cite: 4, 10]. A 20GB partition modification takes only kilobytes of RAM[cite: 10].
* **Familiar API:** The `Volume` interface mirrors the standard `os` and `io/fs` packages exactly (`ReadFile`, `OpenFile`, `MkdirAll`, etc.)[cite: 5, 10].
* **Safe Detach:** Changes are journaled and flushed. You can detach in-place or write to a new image, leaving the original perfectly intact[cite: 4, 10].

## Installation
```bash
go get [github.com/carbon-os/diskimg](https://github.com/carbon-os/diskimg)
```

## CLI Usage

The package comes with a built-in CLI for inspecting and modifying images directly.
```bash
# View disk layout, regions, and partition tables
./diskimg debian.img --info

# List files on Partition 1
./diskimg debian.img --fs ls "/"

# Create a directory
./diskimg debian.img --fs mkdir "/opt/myapp"

# Copy a file from your host machine into the disk image
./diskimg debian.img --fs put ./local_app.tar.gz "/opt/myapp/app.tar.gz"
```

## Go Library Usage

The API is designed so anyone who knows standard Go already knows how to use it[cite: 10].
```go
package main

import (
    "fmt"
    "io"
    "log"
    "os"

    "github.com/carbon-os/diskimg"
)

func main() {
    // 1. Attach the image (parses MBR/GPT)
    img, err := diskimg.Attach("debian.img")
    if err != nil {
        log.Fatal(err)
    }
    // Detach saves changes. Provide a path for a new file, or "" for in-place.
    defer img.Detach("out.img")

    // 2. Mount a partition (1-based index)
    vol, err := img.Mount(1)
    if err != nil {
        log.Fatal(err)
    }
    defer vol.Unmount()

    // 3. Interact using standard os.* style methods
    
    // Simple writes
    vol.WriteFile("etc/motd", []byte("Welcome to diskimg!\n"), 0644)
    vol.MkdirAll("opt/myapp", 0755)

    // Stream large files directly without buffering the whole file in memory
    f, _ := vol.Create("opt/myapp/rootfs.tar")
    src, _ := os.Open("rootfs.tar")
    io.Copy(f, src)
    f.Close()

    // Read back data
    data, _ := vol.ReadFile("etc/os-release")
    fmt.Println(string(data))

    // List directories
    entries, _ := vol.ReadDir("etc")
    for _, e := range entries {
        fmt.Println(e.Name())
    }
}
```

## How It Works

`diskimg` operates in four layers to ensure memory safety and zero OS-level dependencies[cite: 10]:

1. **Attach (Container Layer):** Opens the file handle, parses the GPT or MBR partition table, and maps out the disk regions (Boot, Partition, Gap, Backup)[cite: 1, 10]. No data is read into memory; it's just math on offsets[cite: 10].
2. **Mount (Block Layer):** Looks up the requested partition, detects the filesystem (e.g., ext4 via magic bytes at byte 1080)[cite: 6], and builds an `io.SectionReader` over that exact byte range[cite: 3, 10].
3. **Volume (Filesystem Layer):** Reads and writes translate directly into filesystem block allocations and inode updates[cite: 10]. It only reads the blocks it touches.
4. **Detach (Flush Layer):** Walks the region map in order. It unmounts all volumes to flush dirty blocks, then safely copies the regions (including untouched GRUB and GPT backup headers) to the destination using a strict 32 KB memory buffer[cite: 4, 10].

## Memory Profile

Because it relies heavily on standard interfaces and section readers, the memory footprint is exceptionally low[cite: 10]:

| Operation              | Memory footprint |
|------------------------|------------------|
| Attach                 | ~few KB          |
| Mount                  | ~few KB          |
| ReadFile (small)       | file size        |
| Open + stream (large)  | ~32KB buffer     |
| WriteFile (small)      | file size        |
| Create + stream (large)| ~32KB buffer     |
| Detach                 | ~32KB buffer     |
| **20GB partition**     | **never in RAM** |