package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/carbon-os/diskiso"
	"github.com/carbon-os/diskwim"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "diskwim:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return nil
	}
	inputPath := os.Args[1]

	// Flags
	fs := flag.NewFlagSet("diskwim", flag.ContinueOnError)
	infoFlag := fs.Bool("info", false, "Show WIM metadata")
	lsFlag := fs.String("ls", "", "List a directory path")
	catFlag := fs.String("cat", "", "Print a file to stdout")
	getFlag := fs.String("get", "", "Extract a file (provide source path)")
	getDst := fs.String("dest", "", "Destination path for --get")
	applyFlag := fs.String("apply", "", "Extract full image to directory")
	imageIdx := fs.Int("image", 1, "Image index (1-based)")
	wimInISO := fs.String("wim", "", "Path to WIM inside ISO (default: auto-detect)")

	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	// Resolve the WIM path — either directly or extracted from an ISO.
	wimPath, cleanup, err := resolveWIM(inputPath, *wimInISO)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	w, err := diskwim.Attach(wimPath)
	if err != nil {
		return err
	}
	defer w.Detach()

	switch {
	case *infoFlag:
		return cmdInfo(w, inputPath, wimPath)
	case *lsFlag != "":
		return cmdLS(w, *lsFlag, *imageIdx)
	case *catFlag != "":
		return cmdCat(w, *catFlag, *imageIdx)
	case *getFlag != "":
		dst := *getDst
		if dst == "" && len(fs.Args()) > 0 {
			dst = fs.Args()[0]
		}
		if dst == "" {
			return fmt.Errorf("--get requires a destination (use --dest or a positional argument)")
		}
		return cmdGet(w, *getFlag, dst, *imageIdx)
	case *applyFlag != "":
		return cmdApply(w, *applyFlag, *imageIdx)
	default:
		usage()
	}
	return nil
}

// resolveWIM returns the path to a usable .wim/.esd file.
//
// If inputPath is an ISO, it mounts the ISO, finds the WIM (using wimHint or
// auto-detection), copies it to a temp file, and returns the temp path along
// with a cleanup func that removes it.  If inputPath is already a WIM/ESD the
// path is returned unchanged with a nil cleanup.
func resolveWIM(inputPath, wimHint string) (wimPath string, cleanup func(), err error) {
	lower := strings.ToLower(inputPath)
	if !strings.HasSuffix(lower, ".iso") {
		return inputPath, nil, nil
	}

	disc, err := diskiso.Attach(inputPath)
	if err != nil {
		return "", nil, fmt.Errorf("opening ISO: %w", err)
	}
	defer disc.Detach()

	vol, err := disc.Mount()
	if err != nil {
		return "", nil, fmt.Errorf("mounting ISO: %w", err)
	}

	// Determine which WIM/ESD to use.
	target := wimHint
	if target == "" {
		target, err = detectWIMPath(vol)
		if err != nil {
			return "", nil, err
		}
	}

	fmt.Fprintf(os.Stderr, "ISO: using %s (%s)\n", target, vol.Label())

	src, err := vol.Open(target)
	if err != nil {
		return "", nil, fmt.Errorf("opening %s in ISO: %w", target, err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp("", "diskwim-*.wim")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp file: %w", err)
	}

	if _, err = io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("extracting WIM from ISO: %w", err)
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", nil, err
	}

	return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
}

// detectWIMPath probes well-known locations inside a Windows ISO for a WIM or
// ESD file, in preference order.
func detectWIMPath(vol diskiso.Volume) (string, error) {
	candidates := []string{
		"/sources/install.wim",
		"/sources/install.esd",
		"/sources/boot.wim",
	}
	for _, p := range candidates {
		if _, err := vol.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"no WIM/ESD found in ISO at known locations %v; use --wim to specify the path",
		candidates,
	)
}

// ── commands ──────────────────────────────────────────────────────────────────

func cmdInfo(w *diskwim.WIM, displayPath, wimPath string) error {
	// Show the original input path (ISO or WIM) to the user, but read size
	// from the actual WIM on disk (may be a temp extract).
	fi, _ := os.Stat(wimPath)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	fmt.Printf("File   : %s (%s)\n", displayPath, fmtBytes(size))
	fmt.Printf("Codec  : %s\n", w.Compression())
	fmt.Printf("Images : %d\n\n", len(w.Images()))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INDEX\tEDITION\tNAME\tFILES\tSIZE")
	fmt.Fprintln(tw, "-----\t-------\t----\t-----\t----")
	for _, img := range w.Images() {
		root, err := img.Root()
		var files, sz int64
		if err == nil && root != nil {
			files = root.FileCount()
			sz = root.TotalSize()
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			img.Index(), img.EditionID(), img.Name(),
			fmtInt(files), fmtBytes(sz))
	}
	return tw.Flush()
}

func cmdLS(w *diskwim.WIM, path string, idx int) error {
	img, err := w.Image(idx)
	if err != nil {
		return err
	}
	node, err := img.Lookup(path)
	if err != nil {
		return err
	}
	if !node.IsDir {
		fmt.Printf("%s  (%s)\n", node.Name, fmtBytes(node.Size))
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, child := range node.Children {
		t := "-"
		if child.IsDir {
			t = "d"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t, fmtBytes(child.Size), child.Name)
	}
	return tw.Flush()
}

func cmdCat(w *diskwim.WIM, path string, idx int) error {
	img, err := w.Image(idx)
	if err != nil {
		return err
	}
	r, err := img.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(os.Stdout, r)
	return err
}

func cmdGet(w *diskwim.WIM, src, dst string, idx int) error {
	img, err := w.Image(idx)
	if err != nil {
		return err
	}
	r, err := img.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

// dirVolume is a simple Volume implementation that writes to a local directory.
type dirVolume struct{ root string }

func (v *dirVolume) Create(path string) (io.WriteCloser, error) {
	full := v.root + "/" + strings.TrimPrefix(path, "/")
	return os.Create(full)
}

func (v *dirVolume) Mkdir(path string) error {
	full := v.root + "/" + strings.TrimPrefix(path, "/")
	return os.MkdirAll(full, 0755)
}

func cmdApply(w *diskwim.WIM, dest string, idx int) error {
	img, err := w.Image(idx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	vol := &dirVolume{root: dest}
	fmt.Printf("Extracting image %d (%s) → %s\n", img.Index(), img.Name(), dest)
	var last diskwim.Progress
	err = img.Apply(vol, diskwim.ApplyOptions{
		Progress: func(p diskwim.Progress) {
			last = p
			fmt.Printf("\r  %d / %d files  (%s / %s)   ",
				p.FilesWritten, p.FilesTotal,
				fmtBytes(p.BytesWritten), fmtBytes(p.BytesTotal))
		},
	})
	if last.FilesTotal > 0 {
		fmt.Println()
	}
	return err
}

// ── formatting helpers ────────────────────────────────────────────────────────

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func fmtInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(s)+5)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  diskwim <file.wim|file.esd|file.iso> --info
  diskwim <file.wim|file.esd|file.iso> --ls   <path>  [--image N]
  diskwim <file.wim|file.esd|file.iso> --cat  <path>  [--image N]
  diskwim <file.wim|file.esd|file.iso> --get  <path>  <dest>    [--image N]
  diskwim <file.wim|file.esd|file.iso> --apply <dest-dir>        [--image N]

ISO options:
  --wim <path>   Path to the WIM/ESD inside the ISO
                 (default: auto-detects /sources/install.wim,
                  /sources/install.esd, /sources/boot.wim)`)
}