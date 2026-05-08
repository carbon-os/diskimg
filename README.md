# diskimg

A Go library and CLI tool for reading and writing disk image files without
mounting them via the OS. Supports GPT and MBR partition tables, and provides
a unified filesystem API across ext4, Btrfs, XFS, and FAT variants.

---

## Features

- **Partition table parsing** — GPT (with protective MBR) and MBR
- **Filesystem detection** — magic-byte probing at attach time; no guessing
- **Unified `Volume` API** — mirrors `os.*` so callers need no driver knowledge
- **Btrfs subvolume support** — mount and list named subvolumes
- **Safe output** — write changes to a new file, leaving the original untouched
- **Low memory footprint** — streaming copies use a fixed 32 KB buffer regardless of image size

### Supported filesystems

| Filesystem | Read | Write |
|------------|------|-------|
| ext4       | ✓    | ✓     |
| Btrfs      | ✓    | ✓     |
| XFS        | ✓    | ✓     |
| FAT32      | ✓    | ✓     |
| FAT16      | ✓    | ✓     |
| FAT12      | ✓    | ✓     |

---

## Installation

### Library

```bash
go get github.com/carbon-os/diskimg
```

### CLI

```bash
go install github.com/carbon-os/diskimg/cmd/diskimg@latest
```

---

## CLI usage

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

## Library usage

### Attach, read, detach

```go
img, err := diskimg.Attach("ubuntu.img")
if err != nil {
    log.Fatal(err)
}
defer img.Detach("") // flush in place; pass a path to write to a new file

vol, err := img.Mount(1) // partition 1
if err != nil {
    log.Fatal(err)
}
defer vol.Unmount()

data, err := vol.ReadFile("/etc/os-release")
```

### Write changes to a new file

```go
img, _ := diskimg.Attach("base.img")

vol, _ := img.Mount(1)
vol.WriteFile("/etc/hostname", []byte("my-host\n"), 0644)
vol.Unmount()

// Original base.img is untouched; changes go to modified.img.
img.Detach("modified.img")
```

### Btrfs subvolumes

```go
img, _ := diskimg.Attach("fedora.img")
defer img.Detach("")

// Mount the default tree to list subvolumes.
base, _ := img.Mount(4)
type subvollister interface {
    ListSubvols() ([]string, error)
}
if lister, ok := base.(subvollister); ok {
    names, _ := lister.ListSubvols()
    fmt.Println(names) // [root home]
}

// Mount a named subvolume.
root, _ := img.Mount(4, diskimg.MountOptions{Subvol: "root"})
defer root.Unmount()

data, _ := root.ReadFile("/etc/passwd")
```

### Inspect partitions and regions

```go
img, _ := diskimg.Attach("disk.img")
defer img.Detach("")

for _, p := range img.Partitions() {
    fmt.Printf("Partition %d  start=%d  size=%d  name=%q\n",
        p.Index, p.StartByte, p.SizeBytes, p.Name)
}

for _, r := range img.Regions() {
    fmt.Printf("Region kind=%d  start=%d  end=%d\n", r.Kind, r.Start, r.End)
}
```

---

## Architecture

```
diskimg/
├── attach.go        # Attach() — open image, parse partition table
├── detach.go        # Detach() — flush and close, optional copy-out
├── mount.go         # Mount() — detect filesystem, return Volume
├── region.go        # buildRegions() — boot / partition / gap / backup map
│
├── partition/
│   ├── partition.go # Partition struct
│   ├── gpt/gpt.go   # GPT parser
│   └── mbr/mbr.go   # MBR parser
│
└── fs/
    ├── volume.go    # Volume interface, File, VolumeInfo
    └── fstype/
        └── detect.go # Magic-byte filesystem detection
```

Each filesystem driver (`ext4`, `btrfs`, `xfs`, `fat32`) lives in its own
package and implements the `fs.Volume` interface. `Mount()` selects the right
driver after `fstype.Detect()` identifies the filesystem from magic bytes —
callers never interact with drivers directly.

---

## License

MIT