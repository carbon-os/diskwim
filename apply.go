package diskwim

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// Volume is the write target for Apply. Any implementation of diskimg's Volume
// satisfies this interface automatically.
type Volume interface {
	// Create creates or truncates the file at path, returning a writer.
	Create(path string) (io.WriteCloser, error)
	// Mkdir creates the directory at path (and all parents) if it doesn't exist.
	Mkdir(path string) error
}

// ApplyOptions controls the behaviour of Apply and ApplyTree.
type ApplyOptions struct {
	// Progress is called after each file is written.
	Progress func(Progress)
}

// Progress carries extraction progress information.
type Progress struct {
	FilesWritten int64
	FilesTotal   int64
	BytesWritten int64
	BytesTotal   int64
}

// Apply extracts the full image to vol. Files are streamed in a single forward
// pass over the WIM resource table; each unique hash is decompressed once.
func (img *Image) Apply(vol Volume, opts ...ApplyOptions) error {
	root, err := img.Root()
	if err != nil {
		return err
	}
	return img.applyTree(root, vol, "", opts...)
}

// ApplyTree extracts the subtree at wimPath to destDir on vol.
func (img *Image) ApplyTree(wimPath string, vol Volume, destDir string, opts ...ApplyOptions) error {
	node, err := img.Lookup(wimPath)
	if err != nil {
		return err
	}
	return img.applyTree(node, vol, destDir, opts...)
}

func (img *Image) applyTree(root *Node, vol Volume, destDir string, opts ...ApplyOptions) error {
	var opt ApplyOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	// Collect all files and deduplicate by hash.
	type fileTask struct {
		volPath string
		node    *Node
	}
	var tasks []fileTask
	seen := make(map[[20]byte]bool)

	var collect func(node *Node, dir string)
	collect = func(node *Node, dir string) {
		p := path.Join(dir, node.Name)
		if node.IsDir {
			for _, child := range node.Children {
				collect(child, p)
			}
			return
		}
		tasks = append(tasks, fileTask{volPath: p, node: node})
		seen[node.Hash] = false // mark as not yet written
	}

	if root.IsDir {
		if err := vol.Mkdir(destDir); err != nil {
			return err
		}
		for _, child := range root.Children {
			collect(child, destDir)
		}
	} else {
		tasks = append(tasks, fileTask{volPath: path.Join(destDir, root.Name), node: root})
	}

	// Sort tasks by resource file offset (forward pass).
	sort.Slice(tasks, func(i, j int) bool {
		ri := img.wim.resources[tasks[i].node.Hash]
		rj := img.wim.resources[tasks[j].node.Hash]
		if ri == nil || rj == nil {
			return false
		}
		return ri.offset < rj.offset
	})

	// Deduplicated content cache: hash → decompressed data.
	// This avoids re-decompressing the same content for hard-linked files.
	cache := make(map[[20]byte][]byte)

	p := Progress{FilesTotal: int64(len(tasks)), BytesTotal: root.TotalSize()}

	for _, task := range tasks {
		// Ensure parent directories exist.
		dir := path.Dir(task.volPath)
		if dir != "." && dir != "" {
			if err := vol.Mkdir(dir); err != nil {
				return fmt.Errorf("diskwim: mkdir %s: %w", dir, err)
			}
		}

		var data []byte
		var zero [20]byte
		if task.node.Hash != zero {
			var ok bool
			data, ok = cache[task.node.Hash]
			if !ok {
				var err error
				data, err = img.wim.openNodeData(task.node)
				if err != nil {
					return fmt.Errorf("diskwim: open %s: %w", task.volPath, err)
				}
				cache[task.node.Hash] = data
			}
		}

		f, err := vol.Create(task.volPath)
		if err != nil {
			return fmt.Errorf("diskwim: create %s: %w", task.volPath, err)
		}
		if len(data) > 0 {
			if _, err := f.Write(data); err != nil {
				f.Close()
				return fmt.Errorf("diskwim: write %s: %w", task.volPath, err)
			}
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("diskwim: close %s: %w", task.volPath, err)
		}

		p.FilesWritten++
		p.BytesWritten += task.node.Size
		if opt.Progress != nil {
			opt.Progress(p)
		}
	}
	return nil
}

// pathJoin is a Windows-aware path join that always uses forward slashes.
func pathJoin(a, b string) string {
	a = strings.TrimRight(a, "/\\")
	b = strings.TrimLeft(b, "/\\")
	if a == "" {
		return b
	}
	return a + "/" + b
}