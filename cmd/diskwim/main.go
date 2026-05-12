package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

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
	wimPath := os.Args[1]

	// Flags
	fs := flag.NewFlagSet("diskwim", flag.ContinueOnError)
	infoFlag := fs.Bool("info", false, "Show WIM metadata")
	lsFlag := fs.String("ls", "", "List a directory path")
	catFlag := fs.String("cat", "", "Print a file to stdout")
	getFlag := fs.String("get", "", "Extract a file (provide source path)")
	getDst := fs.String("dest", "", "Destination path for --get")
	applyFlag := fs.String("apply", "", "Extract full image to directory")
	imageIdx := fs.Int("image", 1, "Image index (1-based)")

	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	w, err := diskwim.Attach(wimPath)
	if err != nil {
		return err
	}
	defer w.Detach()

	switch {
	case *infoFlag:
		return cmdInfo(w, wimPath)
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

func cmdInfo(w *diskwim.WIM, path string) error {
	fi, _ := os.Stat(path)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	fmt.Printf("File   : %s (%s)\n", path, fmtBytes(size))
	fmt.Printf("Codec  : %s\n", w.Compression())
	fmt.Printf("Images : %d\n\n", len(w.Images()))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INDEX\tEDITION\tNAME\tFILES\tSIZE")
	fmt.Fprintln(tw, "-----\t-------\t----\t-----\t----")
	for _, img := range w.Images() {
		root, err := img.Root()
		var files int64
		var sz int64
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
  diskwim <file.wim|file.esd> --info
  diskwim <file.wim|file.esd> --ls   <path>  [--image N]
  diskwim <file.wim|file.esd> --cat  <path>  [--image N]
  diskwim <file.wim|file.esd> --get  <path>  <dest>    [--image N]
  diskwim <file.wim|file.esd> --apply <dest-dir>        [--image N]`)
}