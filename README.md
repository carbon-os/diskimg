# diskimg

Pure-Go library for reading, extracting, and rebuilding raw disk images (`.img`)
— no external tools, no root access, no CGo.

```bash
go get github.com/carbon-os/diskimg
```

---

## Overview

`diskimg` gives you a surgical view of a disk image as an ordered list of
**slices** — contiguous byte ranges that together cover every byte of the file.
Slices that fall outside the target partition (boot gap, GPT headers,
inter-partition gaps) are never touched: they are copied verbatim into the
rebuilt image. Only the filesystem contents of the chosen partition are
extracted or replaced.

```
┌──────────────────────────────────────────────────────────────┐
│  gap (boot)  │  partition 1  │  partition 2  │  tail (GPT)  │
│  verbatim ✓  │   extracted   │  verbatim ✓   │  verbatim ✓  │
└──────────────────────────────────────────────────────────────┘
```

This design preserves:

- **GRUB `core.img`** embedded in the boot gap
- Exact partition byte offsets (bootloader block references stay valid)
- GPT primary and backup headers
- Any inter-partition alignment padding

---

## Features

| Area | Details |
|---|---|
| Partition tables | GPT, MBR, raw (whole-image fallback) |
| Filesystems | ext4 (extent tree, htree directories, indirect block map), FAT32 |
| Extraction | Streams filesystem contents to a `tar.Writer` |
| Rebuild | Replaces one partition from an `io.Reader`; all other bytes copied verbatim |
| Dependencies | Zero external binaries; pure Go |

---

## Package Layout

```
diskimg/
├── diskimg.go          # Image open/close, partition detection, ExtractPartition
├── slice.go            # Slice types and buildSlices()
├── rebuild.go          # Rebuild() and copyRange() helpers
│
├── partition/
│   ├── partition.go    # Shared Partition struct and TableType constants
│   ├── gpt/            # GPT parser
│   └── mbr/            # MBR parser
│
├── fstype/             # Filesystem signature detection
├── ext4/               # ext4 superblock, extent tree, htree, extraction
├── fat/                # FAT32 open and extraction
│
└── cmd/
    └── main.go         # img2tar CLI (info / extract / rebuild)
```

---

## Quick Start

### Open an image and inspect it

```go
img, err := diskimg.Open("debian.img")
if err != nil {
    log.Fatal(err)
}
defer img.Close()

fmt.Println("Table :", img.TableType())   // "gpt" | "mbr" | "raw"
fmt.Println("Size  :", img.Size())

for _, p := range img.Partitions() {
    ft, _ := img.DetectFilesystem(p.Index)
    fmt.Printf("  [%d] start=%-12s size=%-12s fs=%s  name=%q\n",
        p.Index,
        humanBytes(p.StartBytes),
        humanBytes(p.SizeBytes),
        ft, p.Name,
    )
}
```

### Extract a partition to a tar archive

```go
f, err := os.Create("rootfs.tar")
if err != nil {
    log.Fatal(err)
}
defer f.Close()

tw := tar.NewWriter(f)
defer tw.Close()

// Extract partition 1 (verbose = true prints every file)
if err := img.ExtractPartition(1, tw, true); err != nil {
    log.Fatal(err)
}
```

### Rebuild an image with a modified partition

```go
newData, err := os.Open("rootfs-modified.tar")
if err != nil {
    log.Fatal(err)
}
defer newData.Close()

// Replace partition 1; every other byte is copied verbatim.
if err := img.Rebuild("debian-patched.img", 1, newData); err != nil {
    log.Fatal(err)
}
```

> **Size contract** — `newPartData` must produce **exactly** as many bytes as
> the original partition. `Rebuild` returns an error if the byte counts differ,
> because writing a different number of bytes would make the partition table
> inconsistent.

---

## Slice Map

Every byte in the image belongs to exactly one slice. The four slice kinds are:

| Kind | Description |
|---|---|
| `SliceKindGap` | Before the first partition — boot code, MBR, GPT primary header |
| `SliceKindPartition` | A filesystem partition |
| `SliceKindBetween` | Gap between two consecutive partitions |
| `SliceKindTail` | After the last partition — GPT backup header |

```go
for _, sl := range img.Slices() {
    fmt.Printf("0x%08X – 0x%08X  %-16s  %s\n",
        sl.Start, sl.End, sl.Kind, humanBytes(sl.Size()))
}
```

`Rebuild` iterates this slice list directly: partition slices receive new data,
everything else is `io.Copy`-ed from the source file at the identical byte
offset.

---

## CLI — `img2tar`

A small reference CLI lives in `cmd/`. Build it with:

```bash
go build -o img2tar ./cmd
```

**Inspect an image**

```bash
./img2tar info debian.img
```
```
Image : debian.img
Size  : 2.0 GiB (2147483648 bytes)
Table : gpt
Sector: 512 bytes

#  START    SIZE     FILESYSTEM  NAME
1  1.0 MiB  512 MiB  vfat        EFI System
2  513 MiB  1.5 GiB  ext4        root

KIND               START       END         SIZE
gap (boot)         0x00000000  0x00100000  1.0 MiB
partition 1        0x00100000  0x20100000  512.0 MiB
partition 2        0x20100000  0x80000000  1.5 GiB
tail (GPT backup)  0x80000000  0x80000200  512 B
```

**Extract partition 2 to a tar file**

```bash
./img2tar extract debian.img -p 2 -o rootfs.tar -v
```

**Rebuild with a modified rootfs**

```bash
./img2tar rebuild debian.img -p 2 -i rootfs-modified.tar -o debian-patched.img
```

| Flag | Default | Meaning |
|---|---|---|
| `-p` | `1` | Partition number (1-based) |
| `-o` | `partitionN.tar` / `rebuilt.img` | Output file |
| `-i` | — | Input tar (rebuild only, required) |
| `-v` | `false` | Print each file during extraction |

---

## Error Handling

All public functions return descriptive, wrapped errors suitable for use with
`errors.Is` / `errors.As`. Every error string is prefixed with the package name:

```
diskimg: open "missing.img": no such file or directory
diskimg: partition 5 out of range (1–2)
diskimg: rebuild: new partition data is 1073741824 bytes, need 536870912
```

---

## Limitations & Roadmap

- Rebuild **does not resize** partitions or rewrite the partition table.
  New data must match the original partition size exactly.
- ext4 write support (rebuilding the filesystem from a tar) is not yet
  implemented; `Rebuild` accepts a pre-built filesystem image via `io.Reader`.
- LUKS / LVM / btrfs / XFS are not yet supported.
- Sparse image formats (QCOW2, VMDK) are not supported; convert to raw first
  with `qemu-img convert -f qcow2 -O raw input.qcow2 output.img`.

---

## Contributing

```bash
git clone https://github.com/carbon-os/diskimg
cd diskimg
go test ./...
```

Please open an issue before sending a pull request for anything beyond a
bug fix — especially for new filesystem or partition-table backends, where
the scope is easy to underestimate.

---

## License

MIT — see [LICENSE](LICENSE).