package diskwim

import (
	"fmt"
	"strings"
)

// Image represents one deployable image within a WIM file.
type Image struct {
	wim   *WIM
	index int      // 1-based
	meta  *xmlImage // from XML manifest, may be nil

	// Lazy-loaded root node.
	root *Node
}

// Index returns the 1-based image index as stored in the WIM.
func (img *Image) Index() int { return img.index }

// Name returns the display name from the XML manifest.
func (img *Image) Name() string {
	if img.meta != nil {
		return img.meta.Name
	}
	return fmt.Sprintf("Image %d", img.index)
}

// Description returns the description field from the XML manifest.
func (img *Image) Description() string {
	if img.meta != nil {
		return img.meta.Description
	}
	return ""
}

// EditionID returns the Windows edition identifier (e.g. "Professional").
func (img *Image) EditionID() string {
	if img.meta != nil {
		return img.meta.EditionID
	}
	return ""
}

// FileCount returns the declared file count from the XML manifest.
func (img *Image) FileCount() int64 {
	if img.meta != nil {
		return img.meta.FileCount
	}
	return 0
}

// Root returns the root Node of the image's directory tree. The metadata
// resource is parsed on first call and then cached.
func (img *Image) Root() (*Node, error) {
	if img.root != nil {
		return img.root, nil
	}
	// Find the metadata resource for this image.
	// Metadata resources are stored in the lookup table with the METADATA flag
	// and appear in the order they are declared in the XML (image index order).
	metaIdx := 0
	for _, re := range img.wim.resources {
		if re.flags&resFlagMetadata != 0 {
			metaIdx++
			if metaIdx == img.index {
				data, err := img.wim.readResource(re, 0)
				if err != nil {
					return nil, fmt.Errorf("diskwim: image %d: read metadata: %w", img.index, err)
				}
				root, err := parseMeta(data)
				if err != nil {
					return nil, fmt.Errorf("diskwim: image %d: parse metadata: %w", img.index, err)
				}
				img.wim.lookupSizes(root)
				img.root = root
				return root, nil
			}
		}
	}

	// Fallback: use the boot metadata resource for image 1.
	if img.index == 1 && !img.wim.header.BootRes.isEmpty() {
		data, err := img.wim.readRawResource(img.wim.header.BootRes, 0)
		if err != nil {
			return nil, fmt.Errorf("diskwim: image %d: read boot metadata: %w", img.index, err)
		}
		root, err := parseMeta(data)
		if err != nil {
			return nil, fmt.Errorf("diskwim: image %d: parse boot metadata: %w", img.index, err)
		}
		img.wim.lookupSizes(root)
		img.root = root
		return root, nil
	}

	return nil, fmt.Errorf("diskwim: metadata resource for image %d not found", img.index)
}

// Open opens the file or directory at the given WIM path (e.g. `\Windows\System32`).
func (img *Image) Open(path string) (*NodeReader, error) {
	node, err := img.Lookup(path)
	if err != nil {
		return nil, err
	}
	if node.IsDir {
		return nil, fmt.Errorf("diskwim: %s is a directory", path)
	}
	return img.wim.openNode(node)
}

// Lookup walks the directory tree to find the Node at the given path.
func (img *Image) Lookup(path string) (*Node, error) {
	root, err := img.Root()
	if err != nil {
		return nil, err
	}
	// Normalize separators.
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "/")
	if path == "" || path == "." {
		return root, nil
	}
	parts := strings.Split(path, "/")
	cur := root
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		found := false
		for _, child := range cur.Children {
			if strings.EqualFold(child.Name, part) {
				cur = child
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("diskwim: %q not found", path)
		}
	}
	return cur, nil
}