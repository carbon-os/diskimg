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

```bash
go get github.com/carbon-os/diskimg
```

---

## Library usage

### Opening and closing an image

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
// Partitions returns every parsed partition entry.
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

// Open opens the named file for streaming reads. Returns an fs.File.
f, err := vol.Open("/var/log/syslog")
defer f.Close()
io.Copy(os.Stdout, f)

// OpenFile opens a file with explicit flags and permissions.
// Returns a *fs.File that also implements io.Writer and io.Seeker.
f, err := vol.OpenFile("/etc/hosts", os.O_RDONLY, 0)
defer f.Close()

// ReadDir returns the entries of a directory, matching os.ReadDir.
entries, err := vol.ReadDir("/etc")
for _, e := range entries {
    info, _ := e.Info()
    fmt.Println(e.Name(), info.Mode(), e.IsDir())
}

// Stat returns FileInfo for a path, following symlinks (like os.Stat).
info, err := vol.Stat("/etc/hostname")
fmt.Println(info.Size(), info.Mode(), info.ModTime())

// Lstat returns FileInfo without following symlinks (like os.Lstat).
info, err := vol.Lstat("/etc/localtime")

// Readlink returns the target of a symbolic link.
target, err := vol.Readlink("/etc/localtime")

// StatFS returns capacity and inode information for the volume.
vi, err := vol.StatFS()
// vi.TotalBytes int64
// vi.FreeBytes  int64
// vi.UsedBytes  int64
// vi.BlockSize  int64
// vi.Inodes     int64
// vi.InodesFree int64
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

// Mkdir creates a single directory.
err := vol.Mkdir("/etc/myapp", 0755)

// MkdirAll creates the directory and any missing parents (like os.MkdirAll).
err := vol.MkdirAll("/opt/myapp/data/cache", 0755)

// Remove removes a single file or empty directory.
err := vol.Remove("/etc/myapp/old.conf")

// RemoveAll removes the path and everything under it (like os.RemoveAll).
err := vol.RemoveAll("/opt/myapp/data")

// Rename renames (moves) a file or directory.
err := vol.Rename("/etc/myapp/config.new", "/etc/myapp/config.yaml")

// Symlink creates a symbolic link at newname pointing to oldname.
err := vol.Symlink("/usr/share/zoneinfo/UTC", "/etc/localtime")

// Link creates a hard link at newname pointing to oldname.
err := vol.Link("/etc/myapp/config.yaml", "/etc/myapp/config.bak")
```

### Updating metadata

```go
// Chmod changes the permission bits of the named file.
err := vol.Chmod("/etc/myapp/secret.key", 0600)

// Chown changes the user and group ownership of the named file.
err := vol.Chown("/var/lib/myapp", 1000, 1000)

// Chtimes changes the access and modification times of the named file.
err := vol.Chtimes("/etc/myapp/config.yaml", time.Now(), time.Now())
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

// Then mount a named subvolume to operate on its tree.
root, err := img.Mount(4, diskimg.MountOptions{Subvol: "root"})
defer root.Unmount()

data, err := root.ReadFile("/etc/passwd")
```

### Full example — patch a config and save to a new image

```go
img, err := diskimg.Attach("base.img")
if err != nil {
    log.Fatal(err)
}

vol, err := img.Mount(1)
if err != nil {
    log.Fatal(err)
}

if err := vol.WriteFile("/etc/hostname", []byte("patched-host\n"), 0644); err != nil {
    log.Fatal(err)
}
if err := vol.MkdirAll("/opt/myapp", 0755); err != nil {
    log.Fatal(err)
}

vol.Unmount()
img.Detach("patched.img") // base.img is untouched
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