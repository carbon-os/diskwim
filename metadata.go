package diskwim

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// ── XML manifest types ────────────────────────────────────────────────────────

type wimXMLDoc struct {
	TotalBytes int64       `xml:"TOTALBYTES"`
	Images     []xmlImage  `xml:"IMAGE"`
}

type xmlImage struct {
	Index       int    `xml:"INDEX,attr"`
	Name        string `xml:"NAME"`
	Description string `xml:"DESCRIPTION"`
	EditionID   string `xml:"EDITIONID"`
	FileCount   int64  `xml:"FILECOUNT"`
	DirCount    int64  `xml:"DIRCOUNT"`
	TotalBytes  int64  `xml:"TOTALBYTES"`
}

// ── Binary metadata resource ──────────────────────────────────────────────────

// parseMeta decodes a WIM metadata resource (security data + dentry tree)
// and returns the root Node.
func parseMeta(data []byte) (*Node, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("diskwim: metadata resource too small")
	}
	// Security data block.
	secDataSize := int(binary.LittleEndian.Uint32(data[0:4]))
	if secDataSize < 8 || secDataSize > len(data) {
		return nil, fmt.Errorf("diskwim: bad security data size %d", secDataSize)
	}
	// Align security data size to 8-byte boundary.
	metaStart := (secDataSize + 7) &^ 7

	p := &metaParser{data: data}
	root, err := p.parseDentry(int64(metaStart))
	if err != nil {
		return nil, fmt.Errorf("diskwim: parse root dentry: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("diskwim: empty metadata resource")
	}
	if err := p.populateChildren(root); err != nil {
		return nil, err
	}
	return root, nil
}

type metaParser struct {
	data []byte
}

// dentry on-disk layout (variable length, 8-byte aligned):
//   [0:8]   length of this entry (8-byte aligned, 0 = end of directory)
//   [8:4]   file attributes
//   [12:4]  security descriptor index (-1 = none)
//   [16:8]  subdirectory offset (from start of metadata resource)
//   [24:8]  unused
//   [32:8]  unused
//   [40:8]  creation time (FILETIME)
//   [48:8]  last access time (FILETIME)
//   [56:8]  last write time (FILETIME)
//   [64:20] SHA-1 hash
//   [84:4]  reparse tag
//   [88:4]  reparse reserved
//   [92:8]  hard link ID
//   [100:2] number of ADS entries
//   [102:2] short name size
//   [104:2] name size
//   [106:…] short name (UTF-16LE, no null)
//   […:…]   name (UTF-16LE, no null)
//   padding to 8-byte boundary
//   ADS entries …

const dentryFixedSize = 106

func (p *metaParser) parseDentry(off int64) (*Node, error) {
	if off+8 > int64(len(p.data)) {
		return nil, fmt.Errorf("diskwim: dentry offset 0x%x out of range", off)
	}
	length := int64(binary.LittleEndian.Uint64(p.data[off:]))
	if length == 0 {
		return nil, nil // end of directory marker
	}
	if length < dentryFixedSize || off+length > int64(len(p.data)) {
		return nil, fmt.Errorf("diskwim: dentry at 0x%x has invalid length %d", off, length)
	}
	d := p.data[off:]

	attrs := binary.LittleEndian.Uint32(d[8:])
	subdirOff := int64(binary.LittleEndian.Uint64(d[16:]))
	creationTime := filetimeToTime(binary.LittleEndian.Uint64(d[40:]))
	accessTime := filetimeToTime(binary.LittleEndian.Uint64(d[48:]))
	modTime := filetimeToTime(binary.LittleEndian.Uint64(d[56:]))
	var hash [20]byte
	copy(hash[:], d[64:84])
	hardLink := int64(binary.LittleEndian.Uint64(d[92:]))
	adsCount := int(binary.LittleEndian.Uint16(d[100:]))
	shortNameSize := int(binary.LittleEndian.Uint16(d[102:]))
	nameSize := int(binary.LittleEndian.Uint16(d[104:]))

	cursor := int64(dentryFixedSize)
	if cursor+int64(shortNameSize) > length {
		return nil, fmt.Errorf("diskwim: dentry short name out of bounds")
	}
	cursor += int64(shortNameSize)
	// Pad to 2-byte alignment before name.
	if cursor%2 != 0 {
		cursor++
	}
	if cursor+int64(nameSize) > length {
		return nil, fmt.Errorf("diskwim: dentry name out of bounds")
	}
	name := utf16ToString(d[cursor : cursor+int64(nameSize)])
	cursor += int64(nameSize)

	// Align to 8 bytes.
	cursor = (cursor + 7) &^ 7

	// Parse ADS entries to find the default data stream hash.
	streamHash := hash
	for i := 0; i < adsCount && cursor < length; i++ {
		adsHash, _, adsLen, err := p.parseADS(off + cursor)
		if err != nil {
			break
		}
		// ADS with empty name is the unnamed data stream.
		// (We'll use the hash found in the dentry itself for the default stream.)
		_ = adsHash
		cursor += adsLen
	}

	isDir := attrs&0x10 != 0 // FILE_ATTRIBUTE_DIRECTORY
	node := &Node{
		Name:         name,
		Attributes:   attrs,
		Size:         0, // filled in from lookup table
		CreationTime: creationTime,
		AccessTime:   accessTime,
		ModTime:      modTime,
		Hash:         streamHash,
		HardLinkID:   hardLink,
		IsDir:        isDir,
		subdirOffset: subdirOff,
	}
	if isDir {
		node.Hash = [20]byte{} // directories have no data hash
	}
	return node, nil
}

// parseADS parses an alternate data stream entry at the given offset.
// Returns (hash, name, byteLength, error).
func (p *metaParser) parseADS(off int64) ([20]byte, string, int64, error) {
	d := p.data[off:]
	if len(d) < 38 {
		return [20]byte{}, "", 0, fmt.Errorf("diskwim: ADS entry too short")
	}
	length := int64(binary.LittleEndian.Uint64(d[0:]))
	if length < 38 || off+length > int64(len(p.data)) {
		return [20]byte{}, "", 0, fmt.Errorf("diskwim: ADS entry length %d invalid", length)
	}
	var hash [20]byte
	copy(hash[:], d[16:36])
	nameSize := int(binary.LittleEndian.Uint16(d[36:]))
	var name string
	if nameSize > 0 && 38+nameSize <= int(length) {
		name = utf16ToString(d[38 : 38+nameSize])
	}
	// ADS entries are padded to 8-byte boundary.
	aligned := (length + 7) &^ 7
	return hash, name, aligned, nil
}

// populateChildren recursively fills the Children slice for all directory Nodes.
func (p *metaParser) populateChildren(node *Node) error {
	if !node.IsDir || node.subdirOffset == 0 {
		return nil
	}
	off := node.subdirOffset
	for {
		child, err := p.parseDentry(off)
		if err != nil {
			return fmt.Errorf("diskwim: parse child at 0x%x: %w", off, err)
		}
		if child == nil {
			break
		}
		// Skip dot and dotdot.
		if child.Name == "." || child.Name == ".." {
			off += p.dentryTotalLength(off)
			continue
		}
		node.Children = append(node.Children, child)
		if err := p.populateChildren(child); err != nil {
			return err
		}
		off += p.dentryTotalLength(off)
	}
	return nil
}

// dentryTotalLength returns the 8-byte-aligned total length of the dentry at off.
func (p *metaParser) dentryTotalLength(off int64) int64 {
	if off+8 > int64(len(p.data)) {
		return 8
	}
	l := int64(binary.LittleEndian.Uint64(p.data[off:]))
	if l < 8 {
		return 8
	}
	return (l + 7) &^ 7
}

// filetimeToTime converts a Windows FILETIME (100-ns ticks since 1601-01-01)
// to a time.Time in UTC.
func filetimeToTime(ft uint64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	const epochDiff = 116444736000000000 // 100-ns ticks between 1601 and 1970
	ns := int64((ft - epochDiff) * 100)
	return time.Unix(0, ns).UTC()
}

// lookupSizes walks a Node tree and fills in Size from the WIM's resource table.
func (w *WIM) lookupSizes(node *Node) {
	if !node.IsDir {
		var zero [20]byte
		if node.Hash != zero {
			if re, ok := w.resources[node.Hash]; ok {
				node.Size = re.origSize
			}
		}
	}
	for _, child := range node.Children {
		w.lookupSizes(child)
	}
}

// ── Image XML helpers ─────────────────────────────────────────────────────────

func normalizePath(p string) string {
	// Convert Windows-style backslash paths to forward-slash for internal use.
	return strings.ReplaceAll(p, "\\", "/")
}