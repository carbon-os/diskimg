# diskimg

A Go library and CLI tool for reading and writing disk image files without
mounting them via the OS. Supports GPT and MBR partition tables, and provides
a unified filesystem API across ext4, Btrfs, XFS, and FAT variants. Includes
a GPT image builder and low-level filesystem formatters for creating new images
from scratch.

---

## Features

- **Partition table parsing** — GPT (with protective MBR) and MBR
- **Filesystem detection** — magic-byte probing at attach time; no guessing
- **Unified `Volume` API** — mirrors `os.*` so callers need no driver knowledge
- **Btrfs subvolume support** — mount and list named subvolumes
- **GPT image builder** — write a new partition table to any blank file
- **Filesystem formatters** — `mkfs.FAT32` and `mkfs.ExFAT` write directly onto raw partition streams
- **Safe output** — write changes to a new file, leaving the original untouched
- **Low memory footprint** — streaming copies use a fixed 32 KB buffer; FAT
  tables are written sector-by-sector regardless of volume size

### Supported filesystems

| Filesystem | Read | Write | Format (`mkfs`) |
|------------|------|-------|-----------------|
| ext4       | ✓    | ✓     | —               |
| Btrfs      | ✓    | ✓     | —               |
| XFS        | ✓    | ✓     | —               |
| FAT32      | ✓    | ✓     | ✓               |
| FAT16      | ✓    | ✓     | —               |
| FAT12      | ✓    | ✓     | —               |
| exFAT      | —    | —     | ✓               |

---

## Installation

```bash
go get github.com/carbon-os/diskimg
```

---

## Library usage

### Opening and closing an existing image

```go
// Attach opens the image file and parses its partition table (GPT or MBR).
img, err := diskimg.Attach("ubuntu.img")
if err != nil {
    log.Fatal(err)
}

// Detach unmounts all volumes, flushes all dirty blocks, and closes the file.
// Pass "" to flush in place, or a path to write the result to a new file
// while leaving the original untouched.
err = img.Detach("")           // in-place
err = img.Detach("output.img") // copy-out
```

### Inspecting partitions and regions

```go
for _, p := range img.Partitions() {
    // p.Index     int    — 1-based partition number
    // p.StartByte int64  — byte offset of the first sector
    // p.SizeBytes int64  — total byte length
    // p.TypeGUID  string — GPT type GUID (empty for MBR)
    // p.Name      string — human-readable label (GPT only)
    fmt.Printf("Partition %d  start=%d  size=%d  guid=%q  name=%q\n",
        p.Index, p.StartByte, p.SizeBytes, p.TypeGUID, p.Name)
}

// Regions returns the ordered layout of the entire disk — boot area,
// partitions, unallocated gaps, and the GPT backup header.
for _, r := range img.Regions() {
    // r.Kind           RegionKind — RegionBoot | RegionPartition | RegionGap | RegionBackup
    // r.Start          int64
    // r.End            int64
    // r.PartitionIndex int        — set only for RegionPartition
    // r.Size()         int64      — convenience: r.End - r.Start
    fmt.Printf("kind=%d  start=%d  end=%d  size=%d\n",
        r.Kind, r.Start, r.End, r.Size())
}
```

### Mounting a partition

```go
// Mount mounts a partition by its 1-based index and returns a Volume.
// The Volume must be Unmount()ed before Detach() is called.
vol, err := img.Mount(1)
defer vol.Unmount()

// To mount a specific Btrfs subvolume, pass MountOptions.
vol, err := img.Mount(4, diskimg.MountOptions{Subvol: "root"})
defer vol.Unmount()
```

### Reading from a volume

```go
// ReadFile reads the entire named file and returns its contents.
data, err := vol.ReadFile("/etc/os-release")

// Open opens the named file for streaming reads.
f, err := vol.Open("/var/log/syslog")
defer f.Close()
io.Copy(os.Stdout, f)

// OpenFile opens a file with explicit flags and permissions.
f, err := vol.OpenFile("/etc/hosts", os.O_RDONLY, 0)
defer f.Close()

// ReadDir returns the entries of a directory, matching os.ReadDir.
entries, err := vol.ReadDir("/etc")
for _, e := range entries {
    info, _ := e.Info()
    fmt.Println(e.Name(), info.Mode(), e.IsDir())
}

// Stat follows symlinks; Lstat does not.
info, err := vol.Stat("/etc/hostname")
info, err  = vol.Lstat("/etc/localtime")

// Readlink returns the target of a symbolic link.
target, err := vol.Readlink("/etc/localtime")

// StatFS returns capacity and inode information for the volume.
vi, err := vol.StatFS()
// vi.TotalBytes, vi.FreeBytes, vi.UsedBytes, vi.BlockSize, vi.Inodes, vi.InodesFree
fmt.Printf("used %d of %d bytes\n", vi.UsedBytes, vi.TotalBytes)

// Type returns the detected filesystem type ("ext4", "btrfs", "xfs", etc.).
fmt.Println(vol.Type())
```

### Writing to a volume

```go
// WriteFile writes data to a file, creating it if it does not exist.
err := vol.WriteFile("/etc/hostname", []byte("my-host\n"), 0644)

// Create creates or truncates a file and returns a writable *File handle.
f, err := vol.Create("/etc/myapp/config.yaml")
defer f.Close()
f.Write([]byte("key: value\n"))

// OpenFile with write flags for full control over creation and truncation.
f, err := vol.OpenFile("/var/log/app.log", os.O_WRONLY|os.O_APPEND, 0644)
defer f.Close()
f.Write([]byte("started\n"))

// MkdirAll creates the directory and any missing parents.
err = vol.MkdirAll("/opt/myapp/data/cache", 0755)

// RemoveAll removes the path and everything under it.
err = vol.RemoveAll("/opt/myapp/data")

// Rename moves a file or directory.
err = vol.Rename("/etc/myapp/config.new", "/etc/myapp/config.yaml")

// Symlink and Link create symbolic and hard links.
err = vol.Symlink("/usr/share/zoneinfo/UTC", "/etc/localtime")
err = vol.Link("/etc/myapp/config.yaml", "/etc/myapp/config.bak")
```

### Updating metadata

```go
err = vol.Chmod("/etc/myapp/secret.key", 0600)
err = vol.Chown("/var/lib/myapp", 1000, 1000)
err = vol.Chtimes("/etc/myapp/config.yaml", time.Now(), time.Now())
```

### Btrfs subvolumes

```go
// Mount the default tree first (no subvol) to enumerate subvolumes.
base, err := img.Mount(4)
defer base.Unmount()

type subvollister interface {
    ListSubvols() ([]string, error)
}
if lister, ok := base.(subvollister); ok {
    names, err := lister.ListSubvols()
    fmt.Println(names) // [root home]
}

// Mount a named subvolume to operate on its tree.
root, err := img.Mount(4, diskimg.MountOptions{Subvol: "root"})
defer root.Unmount()
data, err := root.ReadFile("/etc/passwd")
```

### Patching an existing image and saving to a new file

```go
img, err := diskimg.Attach("base.img")
if err != nil {
    log.Fatal(err)
}

vol, err := img.Mount(1)
if err != nil {
    log.Fatal(err)
}

vol.WriteFile("/etc/hostname", []byte("patched-host\n"), 0644)
vol.MkdirAll("/opt/myapp", 0755)
vol.Unmount()

img.Detach("patched.img") // base.img is left untouched
```

---

## Building new images from scratch

`Builder` creates a new GPT disk image over any blank `*os.File`. After
writing the partition table with `Commit`, `OpenRaw` returns a bounded
`io.ReadWriteSeeker` for each partition that can be passed directly to
`mkfs.FAT32` or `mkfs.ExFAT`. Once formatted, `Mount` works exactly like
it does on an image opened with `Attach`.

No OS loop devices, no `hdiutil`, no root required — every byte is written
directly by the library.

### Well-known type GUIDs

| Constant               | GUID                                   | Use                          |
|------------------------|----------------------------------------|------------------------------|
| `diskimg.GUID_EFISystem`  | `C12A7328-F81F-11D2-BA4B-00A0C93EC93B` | EFI System Partition         |
| `diskimg.GUID_BasicData`  | `EBD0A0A2-B9E5-4433-87C0-68B6B72699C7` | Microsoft Basic Data / exFAT |
| `diskimg.GUID_LinuxData`  | `0FC63DAF-8483-4772-8E79-3D69D8477DE4` | Linux filesystem data        |

### Builder API

```go
// NewBuilder wraps a pre-sized *os.File for GPT construction.
img, err := diskimg.NewBuilder(f)

// AddPartition queues a partition of the given type GUID.
// Pass sizeBytes = 0 to expand the partition to fill all remaining usable space.
// The returned *BuilderPartition pointer is updated in-place by Commit.
p1 := img.AddPartition(diskimg.GUID_EFISystem, 512<<20) // 512 MB
p2 := img.AddPartition(diskimg.GUID_BasicData, 0)       // rest of disk

// Commit writes the protective MBR, GPT header, and partition entries.
// After this call, p1.StartByte and p1.SizeBytes are set.
err = img.Commit()

// OpenRaw returns an io.ReadWriteSeeker whose position 0 is the first byte of
// the partition. Pass it to mkfs.FAT32 or mkfs.ExFAT.
raw, err := img.OpenRaw(p1.Index)

// Mount works identically to Image.Mount — available after Commit.
vol, err := img.Mount(p1.Index)

// Detach flushes and closes the image (optional outPath for copy-out).
err = img.Detach("")
```

### `mkfs` package

```go
import "github.com/carbon-os/diskimg/mkfs"

type mkfs.Options struct {
    Label      string // volume label
    SectorSize int    // defaults to 512; must be a power of two in [512, 4096]
}

// FAT32 formats the stream as FAT32. The partition must be at least ~32 MB
// (65 536 sectors at 512 bytes/sector).
err := mkfs.FAT32(rw, sizeBytes, mkfs.Options{Label: "BOOT"})

// ExFAT formats the stream as exFAT. The partition must be at least 2 048 sectors.
err := mkfs.ExFAT(rw, sizeBytes, mkfs.Options{Label: "DATA"})
```

Both formatters write all required structures — VBR, FSInfo / boot checksum
sectors, backup boot region, FAT, allocation bitmap, up-case table, and root
directory — in a single sequential pass.

### Full example — build a Windows-style dual-partition USB image

```go
package main

import (
    "io"
    "log"
    "os"

    "github.com/carbon-os/diskimg"
    "github.com/carbon-os/diskimg/mkfs"
)

func main() {
    // 1. Create a raw blank 8 GB image.
    f, err := os.Create("windows-unattended.img")
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()
    if err := f.Truncate(8 << 30); err != nil {
        log.Fatal(err)
    }

    // 2. Initialise the GPT and declare partitions.
    img, err := diskimg.NewBuilder(f)
    if err != nil {
        log.Fatal(err)
    }
    efi  := img.AddPartition(diskimg.GUID_EFISystem,  512<<20) // 512 MB EFI
    data := img.AddPartition(diskimg.GUID_BasicData,  0)       // rest for Windows files
    if err := img.Commit(); err != nil {
        log.Fatal(err)
    }

    // 3. Format both partitions in userspace — no loop device, no root.
    rawEFI, _ := img.OpenRaw(efi.Index)
    if err := mkfs.FAT32(rawEFI, efi.SizeBytes, mkfs.Options{Label: "EFI"}); err != nil {
        log.Fatal(err)
    }
    rawData, _ := img.OpenRaw(data.Index)
    if err := mkfs.ExFAT(rawData, data.SizeBytes, mkfs.Options{Label: "WINDOWS"}); err != nil {
        log.Fatal(err)
    }

    // 4. Mount the freshly formatted volumes and write files.
    volEFI, err := img.Mount(efi.Index)
    if err != nil {
        log.Fatal(err)
    }
    defer volEFI.Unmount()

    volData, err := img.Mount(data.Index)
    if err != nil {
        log.Fatal(err)
    }
    defer volData.Unmount()

    // 5. Populate the EFI partition.
    volEFI.MkdirAll("/efi/boot", 0755)

    // 6. Populate the data partition from an ISO image.
    iso, _ := diskimg.Attach("Win11.iso")
    isoVol, _ := iso.Mount(1)
    defer isoVol.Unmount()

    entries, _ := isoVol.ReadDir("/sources")
    for _, e := range entries {
        src, _  := isoVol.Open("/sources/" + e.Name())
        dst, _  := volData.Create("/sources/" + e.Name())
        io.Copy(dst, src)
        dst.Close()
        src.Close()
    }

    // 7. Inject the answer file.
    volData.WriteFile("/autounattend.xml", []byte("<unattend>...</unattend>"), 0644)

    // 8. Flush everything; the original file is updated in place.
    img.Detach("")
}
```

---

## CLI

### Installation

```bash
go install github.com/carbon-os/diskimg/cmd/diskimg@latest
```

### Usage

```
diskimg <image.img> --info
diskimg <image.img> --fs <command> <path> [--partition N] [--subvol NAME]
```

### `--info`

Print the partition table and full region layout of the image.

```
$ diskimg fedora.img --info

=== Partitions ===
Partition 1: Start: 0000001048576, Size: 629145600  bytes | GUID: C12A7328-...
Partition 2: Start: 0000630194176, Size: 1073741824 bytes | GUID: 0FC63DAF-...
  └─ Filesystem: btrfs
  └─ Subvolumes: root, home
...

=== Disk Layout (Regions) ===
[0000000000 - 0000017408] Size: 17408      | Boot (MBR/GPT Header)
[0000017408 - 0001048576] Size: 1031168    | Gap (Unallocated)
[0001048576 - 0630194176] Size: 629145600  | Partition (1)
...
```

### `--fs` subcommands

| Command | Description |
|---------|-------------|
| `ls <path>` | List directory contents |
| `cat <path>` | Print file contents to stdout |
| `mkdir <path>` | Create directory (and all parents) |
| `rm <path>` | Remove file or directory (recursive) |
| `put <host_src> <img_dest>` | Copy a host file into the image |
| `subvols <any>` | List Btrfs subvolumes on the partition |

**Flags**

| Flag | Default | Description |
|------|---------|-------------|
| `--partition N` | `1` | 1-based partition number to operate on |
| `--subvol NAME` | *(none)* | Btrfs subvolume to mount (e.g. `root`, `home`) |

### Examples

```bash
# Inspect an image
diskimg ubuntu.img --info

# List the root of partition 1
diskimg ubuntu.img --fs ls /

# List a directory on a specific partition
diskimg fedora.img --fs ls / --partition 4

# List a directory inside a Btrfs subvolume
diskimg fedora.img --fs ls /var --partition 4 --subvol root

# Print a file
diskimg fedora.img --fs cat /etc/os-release --partition 4 --subvol root

# List available Btrfs subvolumes
diskimg fedora.img --fs subvols . --partition 4

# Copy a host file into the image
diskimg ubuntu.img --fs put ./myfile /etc/myfile --partition 1

# Create a directory
diskimg ubuntu.img --fs mkdir /etc/myapp --partition 1

# Remove a file or directory
diskimg ubuntu.img --fs rm /etc/myapp --partition 1
```

---

## Architecture

```
diskimg/
├── attach.go        # Attach() — open image, parse partition table
├── detach.go        # Detach() — flush and close, optional copy-out
├── mount.go         # Mount() — detect filesystem, return Volume
├── builder.go       # NewBuilder(), AddPartition(), Commit(), OpenRaw(), Mount()
├── region.go        # buildRegions() — boot / partition / gap / backup map
│
├── partition/
│   ├── partition.go # Partition struct
│   ├── gpt/gpt.go   # GPT parser
│   └── mbr/mbr.go   # MBR parser
│
├── mkfs/
│   ├── mkfs.go      # Options, shared binary helpers
│   ├── fat32.go     # FAT32() — VBR, FSInfo, FAT tables, root directory
│   └── exfat.go     # ExFAT() — boot region, FAT, bitmap, upcase table, root directory
│
└── fs/
    ├── volume.go    # Volume interface, File, VolumeInfo
    └── fstype/
        └── detect.go # Magic-byte filesystem detection
```

### How the pieces fit together

`Attach` and `NewBuilder` are the two entry points. `Attach` parses an
existing image; `NewBuilder` creates a fresh one. Both ultimately produce
an `*Image` whose `Mount` method uses `fstype.Detect` to pick the right
driver and return a `Volume`.

`mkfs` is completely independent of the rest of the library. Its only
dependency is the standard library. It receives an `io.ReadWriteSeeker`
and writes binary structures in one sequential pass. `Builder.OpenRaw`
supplies that seeker as a mathematically bounded window into the underlying
`*os.File`, so `mkfs` writes bytes directly to the correct offset in the
image without any OS involvement and without holding the partition data in
RAM.

The three layers therefore have clean separation: `diskimg` owns GPT
geometry, `mkfs` owns filesystem structure, and the caller owns
orchestration. Each layer can be tested, replaced, or extended without
touching the others.

---

## License

MIT