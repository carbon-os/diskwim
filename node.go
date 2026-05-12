package diskwim

import (
	"bytes"
	"io"
	"time"
)

// Node represents a file or directory in a WIM image tree.
type Node struct {
	Name         string
	Attributes   uint32 // Windows file attributes
	Size         int64  // uncompressed size (0 for directories)
	CreationTime time.Time
	AccessTime   time.Time
	ModTime      time.Time
	Hash         [20]byte // SHA-1 of uncompressed data; zero for directories
	HardLinkID   int64    // non-zero if this node is in a hard link group
	IsDir        bool
	Children     []*Node // populated for directories

	// Internal: offset into metadata resource where subdirectory list begins.
	subdirOffset int64
}

// NodeReader is an io.ReadCloser over a WIM file's decompressed content.
type NodeReader struct {
	io.Reader
	closer func() error
}

func (r *NodeReader) Close() error {
	if r.closer != nil {
		return r.closer()
	}
	return nil
}

// openNode returns a NodeReader that streams the decompressed content of node.
func (w *WIM) openNode(node *Node) (*NodeReader, error) {
	if node.IsDir {
		return nil, nil
	}
	var zero [20]byte
	if node.Hash == zero {
		// Empty file.
		return &NodeReader{Reader: bytes.NewReader(nil)}, nil
	}
	r, err := w.openResourceByHash(node.Hash)
	if err != nil {
		return nil, err
	}
	return &NodeReader{Reader: r}, nil
}

// Walk calls fn for every node in the subtree rooted at n (pre-order).
func (n *Node) Walk(fn func(path string, node *Node) error) error {
	return walkNode(n, "", fn)
}

func walkNode(n *Node, prefix string, fn func(string, *Node) error) error {
	path := prefix + "/" + n.Name
	if prefix == "" {
		path = n.Name
	}
	if err := fn(path, n); err != nil {
		return err
	}
	for _, child := range n.Children {
		if err := walkNode(child, path, fn); err != nil {
			return err
		}
	}
	return nil
}

// FileCount returns the total number of regular files in this subtree.
func (n *Node) FileCount() int64 {
	if !n.IsDir {
		return 1
	}
	var count int64
	for _, child := range n.Children {
		count += child.FileCount()
	}
	return count
}

// TotalSize returns the sum of uncompressed sizes of all files in this subtree.
func (n *Node) TotalSize() int64 {
	if !n.IsDir {
		return n.Size
	}
	var total int64
	for _, child := range n.Children {
		total += child.TotalSize()
	}
	return total
}

// openData is a convenience used by Apply.
func (w *WIM) openNodeData(node *Node) ([]byte, error) {
	var zero [20]byte
	if node.Hash == zero {
		return nil, nil
	}
	r, err := w.openResourceByHash(node.Hash)
	if err != nil {
		return nil, err
	}
	// NodeReader wraps an in-memory slice, so this is a no-op copy.
	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.Bytes(), nil
}