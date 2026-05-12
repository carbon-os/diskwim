# diskwim

A pure-Go library for reading and extracting Windows Imaging Format (WIM) and
ESD files without mounting them via the OS. Parses the WIM resource table,
reconstructs directory trees from XML metadata, and extracts files directly
into any `Volume` implementation — including diskimg volumes — using a unified
streaming API.

No `wimlib`, no `DISM`, no administrator rights required.

---

## Features

- **Multi-image support** — list and select images by index, name, or edition
- **Full decompression** — XPRESS, LZX, and LZMS (ESD / solid-compressed)
- **Streaming extraction** — files are decompressed and written in a single
  forward pass; the source WIM is never fully buffered in memory
- **Deduplication-aware** — the WIM hash table is respected; identical files
  share one decompression pass regardless of how many times they appear in the
  tree
- **Volume API integration** — `Apply` writes directly into any `Volume`
  (diskimg ext4, NTFS, FAT32, …) via the same interface used by diskimg
- **Metadata-only mode** — inspect images, list files, and read the XML
  manifest without extracting anything
- **WIM and ESD** — handles both `.wim` (split or single) and `.esd`
  (solid LZMS, used by Windows 10/11 ISOs)

### Compression support

| Codec  | Used in                              | Read | Write |
|--------|--------------------------------------|:----:|:-----:|
| None   | uncompressed WIMs                    | ✓    |       |
| XPRESS | standard `install.wim`, WinPE        | ✓    |       |
| LZX    | older install images, capture WIMs   | ✓    |       |
| LZMS   | `install.esd`, solid-compressed ISOs | ✓    |       |

---

## Installation

```bash
go get github.com/carbon-os/diskwim
```

---

## Library usage

### Opening a WIM or ESD file

```go
wim, err := diskwim.Attach("install.wim")
if err != nil {
    log.Fatal(err)
}
defer wim.Detach()

// Attach also accepts an io.ReaderAt for working directly from an ISO volume
// stream without extracting the file first.
f, err := isoVol.Open("/sources/install.wim")
wim, err = diskwim.AttachReader(f, size)
```

### Listing images

```go
for _, img := range wim.Images() {
    fmt.Printf("[%d] %s — %s (%s, %d files)\n",
        img.Index,
        img.Name,
        img.Description,
        img.EditionID,   // "Professional", "Home", "ServerStandard", …
        img.FileCount,
    )
}
```

```
[1] Windows 11 Home         — Windows 11 Home         (Home,         42631 files)
[2] Windows 11 Pro          — Windows 11 Pro           (Professional, 42884 files)
[3] Windows 11 Pro N        — Windows 11 Pro N         (ProfessionalN,42301 files)
[4] Windows 11 Enterprise   — Windows 11 Enterprise    (Enterprise,   42991 files)
```

### Selecting an image

```go
// By 1-based index (as shown by --info or the XML manifest).
img, err := wim.Image(2)

// By edition ID (case-insensitive).
img, err = wim.ImageByEdition("professional")

// By display name (case-insensitive, partial match OK).
img, err = wim.ImageByName("Windows 11 Pro")
```

### Inspecting the file tree without extracting

```go
root, err := img.Root()

var walk func(n *diskwim.Node, depth int)
walk = func(n *diskwim.Node, depth int) {
    fmt.Printf("%s%s  (%d bytes)\n",
        strings.Repeat("  ", depth), n.Name, n.Size)
    for _, child := range n.Children {
        walk(child, depth+1)
    }
}
walk(root, 0)
```

### Reading a single file

```go
f, err := img.Open(`\Windows\System32\ntoskrnl.exe`)
defer f.Close()

data, err := io.ReadAll(f)
// or stream it:
io.Copy(os.Stdout, f)
```

### Extracting an entire image to a Volume

```go
// vol is any diskimg-mounted Volume (ext4, NTFS, FAT32, …).
vol, err := diskImgHandle.Mount(partitionIndex)
defer vol.Unmount()

err = img.Apply(vol)
```

`Apply` streams all files in a single forward pass over the WIM resource table,
decompressing each resource exactly once even when the same content hash appears
in multiple paths. Progress can be tracked via `ApplyOptions`:

```go
err = img.Apply(vol, diskwim.ApplyOptions{
    Progress: func(p diskwim.Progress) {
        fmt.Printf("\r%d / %d files  (%d MiB / %d MiB)",
            p.FilesWritten, p.FilesTotal,
            p.BytesWritten>>20, p.BytesTotal>>20)
    },
})
```

### Extracting a subtree

```go
// Extract only \Windows\System32 to /System32 on the target volume.
err = img.ApplyTree(`\Windows\System32`, vol, "/System32",
    diskwim.ApplyOptions{})
```

### Reading the raw XML manifest

```go
xml, err := wim.XML()
fmt.Println(string(xml))
```

### Split WIMs

```go
// Windows ISO sources sometimes split large WIMs across .swm files.
wim, err := diskwim.AttachSplit("install.swm",
    "install2.swm",
    "install3.swm",
)
```

---

## CLI

### Installation

```bash
go install github.com/carbon-os/diskwim/cmd/diskwim@latest
```

### Usage

```
diskwim <file.wim|file.esd> --info
diskwim <file.wim|file.esd> --ls   <path>  [--image N]
diskwim <file.wim|file.esd> --cat  <path>  [--image N]
diskwim <file.wim|file.esd> --get  <path>  <dest>    [--image N]
diskwim <file.wim|file.esd> --apply <dest-dir>        [--image N]
```

### `--info` — inspect a WIM

```
$ diskwim install.wim --info

File   : install.wim (4.7 GB)
Codec  : XPRESS
Images : 4

INDEX  EDITION          NAME                    FILES     SIZE
-----  ---------------  ----------------------  --------  -------
1      Home             Windows 11 Home         42,631    16.2 GB
2      Professional     Windows 11 Pro          42,884    16.4 GB
3      ProfessionalN    Windows 11 Pro N        42,301    16.1 GB
4      Enterprise       Windows 11 Enterprise   42,991    16.6 GB
```

### `--ls` — list a directory

```bash
diskwim install.wim --ls '\Windows\System32' --image 2
```

### `--cat` — print a file to stdout

```bash
diskwim install.wim --cat '\Windows\System32\drivers\etc\hosts' --image 2
```

### `--get` — extract a single file

```bash
diskwim install.wim --get '\Windows\System32\ntoskrnl.exe' ./ntoskrnl.exe --image 2
```

### `--apply` — extract a full image to a directory

```bash
# Extract to a local directory (useful for testing without a disk image).
diskwim install.wim --apply /mnt/windows --image 2
```

---

## Integration with diskboot

`diskwim` is designed to slot directly into the `diskboot` pipeline:

```go
package main

import (
    "log"
    "os"

    "github.com/carbon-os/diskboot"
    "github.com/carbon-os/diskboot/bcd"
    "github.com/carbon-os/diskimg"
    "github.com/carbon-os/diskimg/mkfs/fat32"
    "github.com/carbon-os/diskimg/mkfs/ntfs"
    "github.com/carbon-os/diskiso"
    "github.com/carbon-os/diskwim"
)

func main() {
    // ── source ISO ────────────────────────────────────────────────────────────
    disc, err := diskiso.Attach("windows11.iso")
    if err != nil {
        log.Fatal(err)
    }
    defer disc.Detach()

    isoVol, err := disc.Mount()
    if err != nil {
        log.Fatal(err)
    }

    // ── open install.wim directly from the ISO (no extraction needed) ─────────
    wimFile, err := isoVol.Open("/sources/install.wim")
    if err != nil {
        log.Fatal(err)
    }
    defer wimFile.Close()

    wimStat, _ := isoVol.Stat("/sources/install.wim")
    wim, err := diskwim.AttachReader(wimFile, wimStat.Size())
    if err != nil {
        log.Fatal(err)
    }
    defer wim.Detach()

    winImage, err := wim.ImageByEdition("professional")
    if err != nil {
        log.Fatal(err)
    }

    // ── target disk image ─────────────────────────────────────────────────────
    f, err := os.Create("windows.img")
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()
    if err := f.Truncate(64 << 30); err != nil {
        log.Fatal(err)
    }

    img, err := diskimg.NewBuilder(f)
    if err != nil {
        log.Fatal(err)
    }
    efiPart := img.AddPartition(diskimg.GUID_EFISystem, 512<<20)
    winPart := img.AddPartition(diskimg.GUID_BasicData, 0)
    if err := img.Commit(); err != nil {
        log.Fatal(err)
    }

    // ── format ────────────────────────────────────────────────────────────────
    rawEFI, _ := img.OpenRaw(efiPart.Index)
    if err := fat32.Format(rawEFI, efiPart.SizeBytes, fat32.Options{Label: "EFI"}); err != nil {
        log.Fatal(err)
    }
    rawWin, _ := img.OpenRaw(winPart.Index)
    if err := ntfs.Format(rawWin, winPart.SizeBytes, ntfs.Options{Label: "WINDOWS"}); err != nil {
        log.Fatal(err)
    }

    // ── mount ─────────────────────────────────────────────────────────────────
    efiVol, err := img.Mount(efiPart.Index)
    if err != nil {
        log.Fatal(err)
    }
    defer efiVol.Unmount()

    winVol, err := img.Mount(winPart.Index)
    if err != nil {
        log.Fatal(err)
    }
    defer winVol.Unmount()

    // ── install boot environment ──────────────────────────────────────────────
    err = diskboot.InstallWindowsBoot(efiVol, isoVol, diskboot.Options{
        Options: bcd.Options{
            BootMgrDevice: bcd.PartitionDevice(img.DiskGUID(), efiPart.UniqueGUID),
            WindowsDevice: bcd.PartitionDevice(img.DiskGUID(), winPart.UniqueGUID),
            Description:   "Windows 11 Pro",
            Timeout:       10,
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    // ── apply Windows image ───────────────────────────────────────────────────
    err = winImage.Apply(winVol, diskwim.ApplyOptions{
        Progress: func(p diskwim.Progress) {
            log.Printf("extracting: %d / %d files  (%d MiB written)",
                p.FilesWritten, p.FilesTotal, p.BytesWritten>>20)
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    // ── write image ───────────────────────────────────────────────────────────
    if err := img.Detach("windows.img"); err != nil {
        log.Fatal(err)
    }

    log.Println("done — windows.img is ready to boot")
}
```

---

## Architecture

```
diskwim/
├── attach.go           — Attach(), AttachReader(), AttachSplit(), Detach()
├── image.go            — Image: index selection, metadata, Root(), Apply()
├── node.go             — Node: file/directory tree, Open(), stat fields
├── resource.go         — resource table parsing, hash-keyed lookup
├── metadata.go         — XML manifest parser → Node tree
├── apply.go            — Apply(), ApplyTree(), ApplyOptions, Progress
│
└── internal/
    └── decompress/
        ├── xpress.go   — XPRESS decompressor (chunk-based LZ + Huffman)
        ├── lzx.go      — LZX decompressor (sliding window, E8 transform)
        └── lzms.go     — LZMS decompressor (solid, range-coded, used in ESD)
```

### How the pieces fit together

`Attach` opens the file, validates the WIM signature, reads the resource table
and the XML manifest, and returns a `*WIM`. Each `Image` lazily parses its
metadata resource the first time `Root()` or `Apply()` is called.

The resource table maps SHA-1 hashes to compressed byte ranges in the file.
Every file in every image is stored by hash, so identical content across images
or across paths is stored once. `Apply` sorts all resources needed by an image
by their file offset, then makes a single forward pass over the WIM — each
resource is decompressed once, regardless of how many directory entries point
to it, and streamed directly to the target `Volume` without buffering the
decompressed output.

The three decompressors in `internal/decompress` are pure Go with no CGo. Each
exposes a single `Decompress(src, dst []byte) error` function. LZMS is the most
complex: it uses a range coder with multiple probability models and is applied
to the entire image resource as a solid block, which requires holding the full
decompressed resource in memory before individual files can be located within
it. XPRESS and LZX operate chunk-by-chunk and can stream.

---

## License

MIT