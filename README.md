# diskwim

A pure-Go library and CLI tool for reading and extracting Windows Imaging Format (WIM) and ESD files — no wimlib, no DISM, no administrator rights required.

## Features

- Parse WIM and ESD files from any `io.ReaderAt` (file, memory, network stream)
- Split WIM (`.swm`) support
- Full decompression for all WIM compression codecs: **XPRESS**, **LZX**, and **LZMS** (solid ESD)
- Reconstruct directory trees from binary metadata resources
- Extract a single file, a subtree, or a full image
- Hard-link deduplication: each unique content hash is decompressed exactly once per `Apply` call
- No OS mounting, no CGo, no elevated privileges

## Installation

```bash
go get github.com/carbon-os/diskwim
```

To install the CLI tool:

```bash
go install github.com/carbon-os/diskwim/cmd/diskwim@latest
```

## CLI Usage

```
diskwim <file.wim|file.esd> --info
diskwim <file.wim|file.esd> --ls   <path>  [--image N]
diskwim <file.wim|file.esd> --cat  <path>  [--image N]
diskwim <file.wim|file.esd> --get  <path>  <dest>    [--image N]
diskwim <file.wim|file.esd> --apply <dest-dir>        [--image N]
```

### Examples

```bash
# List all images and their metadata
diskwim install.esd --info

# List the contents of a directory in image 3
diskwim install.wim --ls '\Windows\System32' --image 3

# Print a file to stdout
diskwim install.wim --cat '\Windows\System32\drivers\etc\hosts' --image 1

# Extract a single file
diskwim install.wim --get '\Windows\System32\notepad.exe' --dest ./notepad.exe

# Extract a full image to a directory
diskwim install.esd --apply ./extracted --image 3
```

## Library Usage

### Open a WIM and inspect its images

```go
w, err := diskwim.Attach("install.wim")
if err != nil {
    log.Fatal(err)
}
defer w.Detach()

for _, img := range w.Images() {
    fmt.Printf("[%d] %s (%s)\n", img.Index(), img.Name(), img.EditionID())
}
```

### Open a split WIM

```go
w, err := diskwim.AttachSplit("install.swm", "install2.swm", "install3.swm")
if err != nil {
    log.Fatal(err)
}
defer w.Detach()
```

### Open from an `io.ReaderAt`

```go
w, err := diskwim.AttachReader(myReader, size)
```

### Extract a full image

```go
img, err := w.Image(1)
if err != nil {
    log.Fatal(err)
}

err = img.Apply(myVolume, diskwim.ApplyOptions{
    Progress: func(p diskwim.Progress) {
        fmt.Printf("\r%d / %d files", p.FilesWritten, p.FilesTotal)
    },
})
```

### Extract a subtree

```go
err = img.ApplyTree(`\Windows\System32`, myVolume, "./system32")
```

### Read a single file

```go
r, err := img.Open(`\Windows\System32\notepad.exe`)
if err != nil {
    log.Fatal(err)
}
defer r.Close()
io.Copy(os.Stdout, r)
```

### Walk the directory tree

```go
root, err := img.Root()
if err != nil {
    log.Fatal(err)
}

root.Walk(func(path string, node *diskwim.Node) error {
    if !node.IsDir {
        fmt.Printf("%s (%d bytes)\n", path, node.Size)
    }
    return nil
})
```

### Implement a custom Volume

`Apply` and `ApplyTree` write to any `Volume`. Implement it to extract into a disk image, a zip archive, an in-memory filesystem, or anything else:

```go
type Volume interface {
    Create(path string) (io.WriteCloser, error)
    Mkdir(path string) error
}
```

Any implementation of `diskimg.Volume` satisfies this interface automatically.

## Compression Support

| Codec  | Used in              | Notes                                      |
|--------|----------------------|--------------------------------------------|
| None   | Uncompressed WIM     |                                            |
| XPRESS | Standard WIM         | Huffman-coded LZ77, 32 KB chunks           |
| LZX    | High-compression WIM | LZX with aligned-offset and verbatim blocks|
| LZMS   | ESD (solid)          | Range-coded, adaptive Huffman, delta matches, x86 call fixup |

## Node Fields

```go
type Node struct {
    Name         string
    Attributes   uint32    // Windows file attributes
    Size         int64     // Uncompressed size (0 for directories)
    CreationTime time.Time
    AccessTime   time.Time
    ModTime      time.Time
    Hash         [20]byte  // SHA-1 of content; zero for directories
    HardLinkID   int64     // Non-zero if part of a hard link group
    IsDir        bool
    Children     []*Node
}
```

## License

MIT