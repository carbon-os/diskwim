package diskwim

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// ── XML manifest types ────────────────────────────────────────────────────────

type wimXMLDoc struct {
	TotalBytes int64      `xml:"TOTALBYTES"`
	Images     []xmlImage `xml:"IMAGE"`
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
	root, _, err := p.parseDentry(int64(metaStart))
	if err != nil {
		return nil, fmt.Errorf("diskwim: parse root dentry: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("diskwim: empty metadata resource")
	}
	if err := p.populateChildren(root); err != nil {
		return nil, err
	}

	// WIM format quirk: "Double root". WIMGAPI often wraps the actual root directory
	// inside a dummy root directory that also has an empty name. We unwrap it here
	// so the caller interacts directly with the true filesystem root.
	for len(root.Children) == 1 && root.Children[0].Name == "" && root.Children[0].IsDir {
		root = root.Children[0]
	}

	return root, nil
}

type metaParser struct {
	data []byte
}

func (p *metaParser) parseDentry(off int64) (*Node, int64, error) {
	if off+8 > int64(len(p.data)) {
		return nil, 0, nil
	}

	length := int64(binary.LittleEndian.Uint64(p.data[off:]))
	if length == 0 {
		return nil, 8, nil
	}

	// 102 is the smallest valid fixed dentry (through wStreams at offset 100).
	// Root dentries with no name are commonly written as 102 or 104 bytes.
	if length < 102 || off+length > int64(len(p.data)) {
		return nil, 0, fmt.Errorf("diskwim: dentry at 0x%x has invalid length %d", off, length)
	}
	d := p.data[off:]

	attrs      := binary.LittleEndian.Uint32(d[8:])
	subdirOff  := int64(binary.LittleEndian.Uint64(d[16:]))
	creationTime := filetimeToTime(binary.LittleEndian.Uint64(d[40:]))
	accessTime   := filetimeToTime(binary.LittleEndian.Uint64(d[48:]))
	modTime      := filetimeToTime(binary.LittleEndian.Uint64(d[56:]))

	var hash [20]byte
	copy(hash[:], d[64:84])

	// d[84:88]  = dwReparseTag      (skipped)
	// d[88:92]  = dwReparseReserved (skipped)
	hardLink := int64(binary.LittleEndian.Uint64(d[92:]))

	// Fields at 100, 102, 104 may not be present in very short dentries.
	var streamCount, nameSize int
	if length >= 102 {
		streamCount = int(binary.LittleEndian.Uint16(d[100:]))
	}
	if length >= 106 {
		nameSize = int(binary.LittleEndian.Uint16(d[104:]))
	}

	// Long filename is at offset 106 per the MS spec (FileName[0] field).
	// Short name lives after the long name; we don't need it for tree walking.
	var name string
	if nameSize > 0 && 106+int64(nameSize) <= length {
		name = utf16ToString(d[106 : 106+int64(nameSize)])
	}

	alignedLength := (length + 7) &^ 7
	streamOff := off + alignedLength
	streamHash := hash

	for i := 0; i < streamCount; i++ {
		if streamOff+8 > int64(len(p.data)) {
			break
		}
		sLen := int64(binary.LittleEndian.Uint64(p.data[streamOff:]))
		if sLen < 38 || streamOff+sLen > int64(len(p.data)) {
			break
		}

		var sHash [20]byte
		copy(sHash[:], p.data[streamOff+16:streamOff+36])

		sNameSize := int(binary.LittleEndian.Uint16(p.data[streamOff+36:]))
		var sName string
		if sNameSize > 0 && 38+int64(sNameSize) <= sLen {
			sName = utf16ToString(p.data[streamOff+38 : streamOff+38+int64(sNameSize)])
		}
		if i == 0 && sName == "" {
			streamHash = sHash
		}

		streamOff += (sLen + 7) &^ 7
	}

	consumed := streamOff - off

	isDir := attrs&0x10 != 0
	node := &Node{
		Name:         name,
		Attributes:   attrs,
		Size:         0,
		CreationTime: creationTime,
		AccessTime:   accessTime,
		ModTime:      modTime,
		Hash:         streamHash,
		HardLinkID:   hardLink,
		IsDir:        isDir,
		subdirOffset: subdirOff,
	}
	if isDir {
		node.Hash = [20]byte{}
	}
	return node, consumed, nil
}

// populateChildren recursively fills the Children slice for all directory Nodes.
func (p *metaParser) populateChildren(node *Node) error {
	if !node.IsDir || node.subdirOffset == 0 {
		return nil
	}
	off := node.subdirOffset
	for {
		child, consumed, err := p.parseDentry(off)
		if err != nil {
			return fmt.Errorf("diskwim: parse child at 0x%x: %w", off, err)
		}
		if child == nil {
			break
		}

		off += consumed // already 8-byte aligned; no second realignment needed

		if child.Name == "." || child.Name == ".." {
			continue
		}

		node.Children = append(node.Children, child)
		if err := p.populateChildren(child); err != nil {
			return err
		}
	}
	return nil
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