# diskimg

A Go library and CLI for reading and writing disk image files without mounting
them via the OS. Supports GPT and MBR partition tables and provides a unified
filesystem API across ext4, Btrfs, XFS, and FAT variants. Includes a GPT image
builder and low-level filesystem formatters for creating new images from scratch.

---

## Features

- **Partition table parsing** — GPT (with protective MBR) and MBR
- **Filesystem detection** — magic-byte probing at attach time
- **Unified `Volume` API** — mirrors `os.*` so callers need no driver knowledge
- **Btrfs subvolume support** — mount and list named subvolumes
- **GPT image builder** — write a new partition table to any blank file
- **Filesystem formatters** — `fat32.Format`, `exfat.Format`, `ntfs.Format`, and
  `ext4.Format` write directly onto raw partition streams
- **Safe output** — write changes to a new file, leaving the original untouched
- **Low memory footprint** — streaming copies use a fixed 32 KiB buffer;
  allocation tables and bitmaps are written block-by-block regardless of volume size

### Supported filesystems

| Filesystem | Read | Write | Format |
|------------|:----:|:-----:|:------:|
| ext4       | ✓    | ✓     | ✓      |
| Btrfs      | ✓    | ✓     |        |
| XFS        | ✓    | ✓     |        |
| FAT32      | ✓    | ✓     | ✓      |
| FAT16      | ✓    | ✓     |        |
| FAT12      | ✓    | ✓     |        |
| exFAT      |      |       | ✓      |
| NTFS       |      |       | ✓      |

---

## Installation

```bash
go get github.com/carbon-os/diskimg
```

---

## Library usage

### Opening and closing an existing image

```go
img, err := diskimg.Attach("ubuntu.img")
if err != nil {
    log.Fatal(err)
}

// Pass "" to flush in place, or a path to write to a new file
// while leaving the original untouched.
err = img.Detach("")           // in-place
err = img.Detach("output.img") // copy-out
```

### Inspecting partitions and regions

```go
for _, p := range img.Partitions() {
    fmt.Printf("partition %d  start=%d  size=%d  guid=%q  name=%q\n",
        p.Index, p.StartByte, p.SizeBytes, p.TypeGUID, p.Name)
}

// Regions returns the ordered layout of the entire disk —
// boot area, partitions, unallocated gaps, and the GPT backup header.
for _, r := range img.Regions() {
    fmt.Printf("kind=%d  start=%d  end=%d  size=%d\n",
        r.Kind, r.Start, r.End, r.Size())
}
```

### Mounting a partition

```go
// Mount by 1-based index; the Volume must be Unmount()ed before Detach().
vol, err := img.Mount(1)
defer vol.Unmount()

// To mount a specific Btrfs subvolume, pass MountOptions.
vol, err = img.Mount(4, diskimg.MountOptions{Subvol: "root"})
defer vol.Unmount()
```

### Reading from a volume

```go
data, err := vol.ReadFile("/etc/os-release")

f, err := vol.Open("/var/log/syslog")
defer f.Close()
io.Copy(os.Stdout, f)

entries, err := vol.ReadDir("/etc")
for _, e := range entries {
    info, _ := e.Info()
    fmt.Println(e.Name(), info.Mode(), e.IsDir())
}

info, err := vol.Stat("/etc/hostname")    // follows symlinks
info, err  = vol.Lstat("/etc/localtime") // does not follow symlinks

target, err := vol.Readlink("/etc/localtime")

vi, err := vol.StatFS()
fmt.Printf("used %d of %d bytes\n", vi.UsedBytes, vi.TotalBytes)

fmt.Println(vol.Type()) // "ext4", "btrfs", "xfs", ...
```

### Writing to a volume

```go
err = vol.WriteFile("/etc/hostname", []byte("my-host\n"), 0644)

f, err := vol.Create("/etc/myapp/config.yaml")
defer f.Close()
f.Write([]byte("key: value\n"))

f, err = vol.OpenFile("/var/log/app.log", os.O_WRONLY|os.O_APPEND, 0644)
defer f.Close()

err = vol.MkdirAll("/opt/myapp/data/cache", 0755)
err = vol.RemoveAll("/opt/myapp/data")
err = vol.Rename("/etc/myapp/config.new", "/etc/myapp/config.yaml")
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
base, err := img.Mount(4)
defer base.Unmount()

type subvollister interface {
    ListSubvols() ([]string, error)
}
if lister, ok := base.(subvollister); ok {
    names, _ := lister.ListSubvols()
    fmt.Println(names) // [root home]
}

root, err := img.Mount(4, diskimg.MountOptions{Subvol: "root"})
defer root.Unmount()
data, err := root.ReadFile("/etc/passwd")
```

### Patching an existing image

```go
img, err := diskimg.Attach("base.img")

vol, err := img.Mount(1)
vol.WriteFile("/etc/hostname", []byte("patched-host\n"), 0644)
vol.MkdirAll("/opt/myapp", 0755)
vol.Unmount()

img.Detach("patched.img") // base.img is left untouched
```

---

## Building new images from scratch

`Builder` creates a new GPT disk image over any blank `*os.File`. After writing
the partition table with `Commit`, `OpenRaw` returns a bounded
`io.ReadWriteSeeker` for each partition that can be passed directly to any of
the `mkfs` formatters. Once formatted, `Mount` works exactly like it does on an
image opened with `Attach`.

No OS loop devices, no `hdiutil`, no root required.

### Well-known type GUIDs

| Constant              | GUID                                   | Use                              |
|-----------------------|----------------------------------------|----------------------------------|
| `diskimg.GUID_EFISystem`  | `C12A7328-F81F-11D2-BA4B-00A0C93EC93B` | EFI System Partition             |
| `diskimg.GUID_BasicData`  | `EBD0A0A2-B9E5-4433-87C0-68B6B72699C7` | Microsoft Basic Data / exFAT / NTFS |
| `diskimg.GUID_LinuxData`  | `0FC63DAF-8483-4772-8E79-3D69D8477DE4` | Linux filesystem data (ext4, XFS, …) |

### Builder API

```go
img, err := diskimg.NewBuilder(f)

// sizeBytes = 0 expands the partition to fill all remaining usable space.
p1 := img.AddPartition(diskimg.GUID_EFISystem, 512<<20)
p2 := img.AddPartition(diskimg.GUID_LinuxData, 0)

// Commit writes the protective MBR, GPT header, and partition entries.
// After this call p1.StartByte and p1.SizeBytes are populated.
err = img.Commit()

// OpenRaw returns an io.ReadWriteSeeker whose position 0 is the first byte
// of the partition. Pass it to any mkfs formatter.
raw, err := img.OpenRaw(p1.Index)

vol, err  := img.Mount(p1.Index)
err        = img.Detach("")
```

### `mkfs` formatters

Each formatter lives in its own subpackage and accepts an `io.ReadWriteSeeker`
plus a size and an options struct. All write their required structures — boot
regions, allocation tables, bitmaps, system files and directories — in a single
sequential pass with no intermediate allocations proportional to volume size.

```go
import (
    "github.com/carbon-os/diskimg/mkfs/ext4"
    "github.com/carbon-os/diskimg/mkfs/fat32"
    "github.com/carbon-os/diskimg/mkfs/exfat"
    "github.com/carbon-os/diskimg/mkfs/ntfs"
)

// ext4 — partition must be at least 16 MiB.
err := ext4.Format(rw, sizeBytes, ext4.Options{Label: "ROOT"})

// FAT32 — partition must be at least ~32 MiB (65 536 sectors × 512 bytes).
err  = fat32.Format(rw, sizeBytes, fat32.Options{Label: "BOOT"})

// exFAT — partition must be at least 2 048 sectors.
err  = exfat.Format(rw, sizeBytes, exfat.Options{Label: "DATA"})

// NTFS — fully automated MFT and boot sector layout.
err  = ntfs.Format(rw, sizeBytes, ntfs.Options{Label: "WINDOWS"})
```

All `Options` structs expose these common fields:

| Field        | Type     | Default    | Description                                                |
|--------------|----------|------------|------------------------------------------------------------|
| `Label`      | `string` | —          | Volume label                                               |
| `SectorSize` | `int`    | `512`      | Logical sector size; power of two in [512, 4096] *(FAT32/exFAT/NTFS only)* |

`ext4.Options` also exposes:

| Field         | Type     | Default | Description                                                        |
|---------------|----------|---------|--------------------------------------------------------------------|
| `UUID`        | `[16]byte` | random | Filesystem UUID; a random v4 UUID is generated when zero           |
| `InodeRatio`  | `int64`  | `16384` | One inode is created per this many bytes                           |
| `ReservedPct` | `int`    | `5`     | Percentage of blocks reserved for root                             |

### ext4 feature set

The ext4 formatter writes the same feature set that `mkfs.ext4` enables by
default on modern Linux systems:

| Feature              | Flag                    | Description                                             |
|----------------------|-------------------------|---------------------------------------------------------|
| Extent tree inodes   | `INCOMPAT_EXTENTS`      | All inodes use extent trees; no indirect block maps     |
| 64-bit descriptors   | `INCOMPAT_64BIT`        | Group descriptors are 64 bytes; supports volumes > 8 TiB |
| Flexible block groups | `INCOMPAT_FLEX_BG`     | Metadata aggregated in the first group of each flex group (size 16) |
| Directory file type  | `INCOMPAT_FILETYPE`     | Directory entries record the file type                  |
| Journal              | `COMPAT_HAS_JOURNAL`    | Empty JBD2 journal written in inode 8                   |
| Extended attributes  | `COMPAT_EXT_ATTR`       | —                                                       |
| Resize inode         | `COMPAT_RESIZE_INODE`   | Reserved GDT blocks allow online filesystem growth      |
| Sparse superblocks   | `RO_COMPAT_SPARSE_SUPER`| SB backups only in groups 0, 1, and powers of 3, 5, 7  |
| Metadata checksums   | `RO_COMPAT_METADATA_CSUM` | crc32c on superblock, all group descriptors, bitmaps, and inodes |
| Large / huge files   | `RO_COMPAT_LARGE_FILE`, `RO_COMPAT_HUGE_FILE` | Files larger than 2 GiB / 2 TiB  |
| Extra inode size     | `RO_COMPAT_EXTRA_ISIZE` | 28 bytes of extra inode space for nanosecond timestamps |

### Full example — Linux boot image

```go
package main

import (
    "log"
    "os"

    "github.com/carbon-os/diskimg"
    "github.com/carbon-os/diskimg/mkfs/ext4"
    "github.com/carbon-os/diskimg/mkfs/fat32"
)

func main() {
    f, err := os.Create("linux.img")
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()
    if err := f.Truncate(8 << 30); err != nil { // 8 GiB
        log.Fatal(err)
    }

    img, err := diskimg.NewBuilder(f)
    if err != nil {
        log.Fatal(err)
    }
    efi  := img.AddPartition(diskimg.GUID_EFISystem, 512<<20)  // 512 MiB
    root := img.AddPartition(diskimg.GUID_LinuxData, 0)         // fills rest
    if err := img.Commit(); err != nil {
        log.Fatal(err)
    }

    rawEFI, _ := img.OpenRaw(efi.Index)
    if err := fat32.Format(rawEFI, efi.SizeBytes, fat32.Options{Label: "EFI"}); err != nil {
        log.Fatal(err)
    }

    rawRoot, _ := img.OpenRaw(root.Index)
    if err := ext4.Format(rawRoot, root.SizeBytes, ext4.Options{
        Label:       "ROOT",
        InodeRatio:  16384,
        ReservedPct: 5,
    }); err != nil {
        log.Fatal(err)
    }

    vol, err := img.Mount(root.Index)
    if err != nil {
        log.Fatal(err)
    }
    defer vol.Unmount()

    vol.MkdirAll("/etc", 0755)
    vol.MkdirAll("/var/log", 0755)
    vol.WriteFile("/etc/hostname", []byte("myhost\n"), 0644)

    img.Detach("linux.img")
}
```

### Full example — dual-partition Windows image

```go
package main

import (
    "log"
    "os"

    "github.com/carbon-os/diskimg"
    "github.com/carbon-os/diskimg/mkfs/fat32"
    "github.com/carbon-os/diskimg/mkfs/ntfs"
)

func main() {
    f, err := os.Create("windows.img")
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()
    if err := f.Truncate(8 << 30); err != nil {
        log.Fatal(err)
    }

    img, err := diskimg.NewBuilder(f)
    if err != nil {
        log.Fatal(err)
    }
    efi  := img.AddPartition(diskimg.GUID_EFISystem,  512<<20)
    data := img.AddPartition(diskimg.GUID_BasicData,  0)
    if err := img.Commit(); err != nil {
        log.Fatal(err)
    }

    rawEFI, _ := img.OpenRaw(efi.Index)
    if err := fat32.Format(rawEFI, efi.SizeBytes, fat32.Options{Label: "EFI"}); err != nil {
        log.Fatal(err)
    }

    rawData, _ := img.OpenRaw(data.Index)
    if err := ntfs.Format(rawData, data.SizeBytes, ntfs.Options{Label: "WINDOWS"}); err != nil {
        log.Fatal(err)
    }

    img.Detach("windows.img")
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

Print the partition table and region layout of the image.

```
$ diskimg fedora.img --info

=== Partitions ===
Partition 1: Start: 0000001048576, Size:  629145600 bytes | GUID: C12A7328-...
Partition 2: Start: 0000630194176, Size: 1073741824 bytes | GUID: 0FC63DAF-...
  └─ Filesystem: ext4
  └─ UUID:       5ae73877-4510-419e-b15a-44ac2a2df7c6

=== Disk Layout (Regions) ===
[0000000000 - 0000017408] Size:       17408 | Boot (MBR/GPT Header)
[0000017408 - 0001048576] Size:     1031168 | Gap (Unallocated)
[0001048576 - 0630194176] Size:   629145600 | Partition (1)
[0630194176 - 1703936000] Size:  1073741824 | Partition (2)
...
```

### `--fs` subcommands

| Command               | Description                                   |
|-----------------------|-----------------------------------------------|
| `ls <path>`           | List directory contents                       |
| `cat <path>`          | Print file contents to stdout                 |
| `mkdir <path>`        | Create directory (and all parents)            |
| `rm <path>`           | Remove file or directory (recursive)          |
| `put <src> <dst>`     | Copy a host file into the image               |
| `subvols <any>`       | List Btrfs subvolumes on the partition        |

| Flag              | Default | Description                                              |
|-------------------|---------|----------------------------------------------------------|
| `--partition N`   | `1`     | 1-based partition number to operate on                   |
| `--subvol NAME`   |         | Btrfs subvolume to mount (e.g. `root`, `home`)           |

### Examples

```bash
diskimg ubuntu.img  --info
diskimg ubuntu.img  --fs ls /
diskimg fedora.img  --fs ls / --partition 2
diskimg fedora.img  --fs ls /var --partition 2 --subvol root
diskimg fedora.img  --fs cat /etc/os-release --partition 2 --subvol root
diskimg fedora.img  --fs subvols . --partition 2
diskimg ubuntu.img  --fs put ./myfile /etc/myfile --partition 1
diskimg ubuntu.img  --fs mkdir /etc/myapp --partition 1
diskimg ubuntu.img  --fs rm /etc/myapp --partition 1
```

---

## Architecture

```
diskimg/
├── attach.go           — Attach(): open image, parse partition table
├── detach.go           — Detach(): flush and close, optional copy-out
├── mount.go            — Mount(): detect filesystem, return Volume
├── builder.go          — NewBuilder(), AddPartition(), Commit(), OpenRaw()
├── region.go           — buildRegions(): boot / partition / gap / backup map
│
├── fs/
│   ├── volume.go       — Volume interface, File, VolumeInfo
│   └── fstype/
│       └── detect.go   — magic-byte filesystem detection
│
├── partition/
│   ├── partition.go    — Partition struct
│   ├── gpt/gpt.go      — GPT parser
│   └── mbr/mbr.go      — MBR parser
│
└── mkfs/
    ├── ext4/
    │   ├── ext4.go         — Format() entry point, Options
    │   ├── layout.go       — geometry pre-computation, hasSuperBackup
    │   ├── write.go        — single sequential write pass
    │   ├── superblock.go   — 1024-byte superblock encoding, feature flags
    │   ├── gdt.go          — 64-byte group descriptor encoding, checksums
    │   ├── bitmaps.go      — block and inode bitmap construction
    │   ├── inodes.go       — inode table, extent tree encoding
    │   ├── dirs.go         — root and lost+found directory entries
    │   ├── journal.go      — JBD2 journal superblock
    │   └── helpers.go      — crc32c helper
    │
    ├── fat32/
    │   ├── fat32.go        — Format() entry point, Options
    │   ├── bpb.go          — VBR and FSInfo sector construction
    │   ├── fat.go          — FAT table writing, size calculation
    │   ├── dir.go          — root directory cluster, PadLabel
    │   └── helpers.go      — binary helpers, sectorsPerCluster
    │
    ├── exfat/
    │   ├── exfat.go        — Format() entry point, Options, fsLayout
    │   ├── boot.go         — boot region (VBR, extended sectors, checksum)
    │   ├── fat.go          — FAT chain writing
    │   ├── upcase.go       — up-case table construction and checksum
    │   ├── dir.go          — directory entry writers
    │   └── helpers.go      — binary helpers, spcShift
    │
    └── ntfs/
        ├── ntfs.go         — Format() entry point, Options, fsLayout
        ├── boot.go         — boot sector and extended BPB definitions
        ├── mft.go          — MFT record builder, attribute construction
        ├── records.go      — system file definitions ($MFT, $Boot, $Volume, …)
        └── helpers.go      — binary helpers, sectorsPerCluster, upcase table
```

### How the pieces fit together

`Attach` and `NewBuilder` are the two entry points. `Attach` parses an existing
image; `NewBuilder` creates a fresh one. Both produce an `*Image` whose `Mount`
method uses `fstype.Detect` to pick the right driver and return a `Volume`.

The `mkfs` subpackages are entirely independent of the rest of the library —
their only dependency is the standard library (plus `github.com/google/uuid` in
`mkfs/ext4` for UUID generation). Each formatter receives an `io.ReadWriteSeeker`
and writes binary structures in one sequential pass. `Builder.OpenRaw` supplies
that seeker as a bounded window into the underlying `*os.File`, so the formatter
writes bytes directly to the correct offset in the image without any OS
involvement and without holding partition data in RAM.

The three layers have clean separation: `diskimg` owns GPT geometry, `mkfs` owns
filesystem structure, and the caller owns orchestration. Each layer can be
tested, replaced, or extended without touching the others.

---

## License

MIT