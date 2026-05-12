package diskwim

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"
)

// WIM is an open WIM or ESD file set.
type WIM struct {
	readers     []io.ReaderAt
	sizes       []int64
	header      rawHeader
	compression CompressionType
	chunkSize   uint32

	// SHA-1 hash → resource entry (deduplicated).
	resources map[[20]byte]*resourceEntry
	
	// All metadata resources (preserves duplicates for image mapping).
	metaResources []*resourceEntry

	// Parsed XML manifest (UTF-8).
	xmlBytes []byte
	xmlDoc   wimXMLDoc

	images    []*Image
	closeFunc func() error
}

// Attach opens a WIM or ESD file by path.
func Attach(path string) (*WIM, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	w, err := AttachReader(f, fi.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	w.closeFunc = f.Close
	return w, nil
}

// AttachReader opens a WIM from any io.ReaderAt.
func AttachReader(r io.ReaderAt, size int64) (*WIM, error) {
	w := &WIM{
		readers:   []io.ReaderAt{r},
		sizes:     []int64{size},
		resources: make(map[[20]byte]*resourceEntry),
	}
	return w, w.init()
}

// AttachSplit opens a split WIM (*.swm) from multiple part paths.
// The first path must be part 1 (which contains the header and XML).
func AttachSplit(paths ...string) (*WIM, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("diskwim: no parts provided")
	}
	var files []*os.File
	readers := make([]io.ReaderAt, 0, len(paths))
	sizes := make([]int64, 0, len(paths))

	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			for _, ff := range files {
				ff.Close()
			}
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			for _, ff := range files {
				ff.Close()
			}
			return nil, err
		}
		files = append(files, f)
		readers = append(readers, f)
		sizes = append(sizes, fi.Size())
	}

	w := &WIM{
		readers:   readers,
		sizes:     sizes,
		resources: make(map[[20]byte]*resourceEntry),
		closeFunc: func() error {
			for _, f := range files {
				f.Close()
			}
			return nil
		},
	}
	if err := w.init(); err != nil {
		w.Detach()
		return nil, err
	}
	return w, nil
}

// Detach closes all underlying file handles.
func (w *WIM) Detach() error {
	if w.closeFunc != nil {
		return w.closeFunc()
	}
	return nil
}

// Compression returns the codec used for file resources in this WIM.
func (w *WIM) Compression() CompressionType { return w.compression }

// XML returns the raw UTF-8 XML manifest.
func (w *WIM) XML() ([]byte, error) { return w.xmlBytes, nil }

// ── internal init ─────────────────────────────────────────────────────────────

func (w *WIM) init() error {
	if err := w.readHeader(); err != nil {
		return err
	}
	if err := w.readLookupTable(); err != nil {
		return err
	}
	if err := w.readXML(); err != nil {
		return err
	}
	return w.buildImages()
}

func (w *WIM) readHeader() error {
	var hdr rawHeader
	if err := readStructAt(w.readers[0], 0, &hdr); err != nil {
		return fmt.Errorf("diskwim: read header: %w", err)
	}
	if string(hdr.Magic[:]) != wimMagic {
		return fmt.Errorf("diskwim: not a WIM file (magic %q)", hdr.Magic)
	}
	switch {
	case hdr.Flags&flagCompressLZMS != 0:
		w.compression = CompressionLZMS
	case hdr.Flags&flagCompressLZX != 0:
		w.compression = CompressionLZX
	case hdr.Flags&flagCompressXPRESS != 0:
		w.compression = CompressionXPRESS
	default:
		w.compression = CompressionNone
	}
	w.header = hdr
	w.chunkSize = hdr.ChunkSize
	if w.chunkSize == 0 {
		w.chunkSize = 32768
	}
	return nil
}

func (w *WIM) readLookupTable() error {
	res := w.header.LookupRes
	if res.isEmpty() {
		return nil
	}
	data, err := w.readRawResource(res, 0 /* part 1 */)
	if err != nil {
		return fmt.Errorf("diskwim: read lookup table: %w", err)
	}

	const entrySize = 50
	if len(data)%entrySize != 0 {
		return fmt.Errorf("diskwim: lookup table size %d not a multiple of %d", len(data), entrySize)
	}
	n := len(data) / entrySize
	for i := 0; i < n; i++ {
		var e rawLookupEntry
		if err := readRawLookupEntry(data[i*entrySize:], &e); err != nil {
			return fmt.Errorf("diskwim: lookup entry %d: %w", i, err)
		}
		re := &resourceEntry{
			offset:     int64(e.Offset),
			compSize:   e.compressedSize(),
			origSize:   int64(e.OrigSize),
			flags:      e.flags(),
			partNumber: e.Part,
		}
		w.resources[e.Hash] = re
		
		// Retain a raw list of all metadata resources to avoid map deduplication
		if re.flags&resFlagMetadata != 0 {
			w.metaResources = append(w.metaResources, re)
		}
	}
	return nil
}

func readRawLookupEntry(b []byte, e *rawLookupEntry) error {
	if len(b) < 50 {
		return fmt.Errorf("short entry: %d bytes", len(b))
	}
	copy(e.Packed[:], b[0:8])
	e.Offset = leUint64(b[8:])
	e.OrigSize = leUint64(b[16:])
	e.Part = leUint16(b[24:])
	e.RefCount = leUint32(b[26:])
	copy(e.Hash[:], b[30:50])
	return nil
}

func (w *WIM) readXML() error {
	res := w.header.XMLRes
	if res.isEmpty() {
		return nil
	}
	raw, err := w.readRawResource(res, 0)
	if err != nil {
		return fmt.Errorf("diskwim: read XML data: %w", err)
	}
	// XML data starts with a UTF-16LE BOM (0xFF 0xFE).
	w.xmlBytes = []byte(utf16ToString(raw))

	// Parse the manifest for image metadata.
	dec := xml.NewDecoder(bytes.NewReader(w.xmlBytes))
	if err := dec.Decode(&w.xmlDoc); err != nil {
		// Non-fatal: some WIM files have incomplete XML.
		return nil
	}
	return nil
}

func (w *WIM) buildImages() error {
	w.images = make([]*Image, int(w.header.ImageCount))
	for i := range w.images {
		img := &Image{
			wim:   w,
			index: i + 1, // 1-based
		}
		// Attach XML metadata if available.
		for j := range w.xmlDoc.Images {
			if w.xmlDoc.Images[j].Index == i+1 {
				img.meta = &w.xmlDoc.Images[j]
				break
			}
		}
		w.images[i] = img
	}
	return nil
}

// ── Image selection ───────────────────────────────────────────────────────────

// Images returns all images contained in the WIM.
func (w *WIM) Images() []*Image { return w.images }

// Image returns the image at the given 1-based index.
func (w *WIM) Image(index int) (*Image, error) {
	if index < 1 || index > len(w.images) {
		return nil, fmt.Errorf("diskwim: image index %d out of range [1, %d]", index, len(w.images))
	}
	return w.images[index-1], nil
}

// ImageByEdition returns the first image whose EditionID matches (case-insensitive).
func (w *WIM) ImageByEdition(edition string) (*Image, error) {
	edition = strings.ToLower(edition)
	for _, img := range w.images {
		if img.meta != nil && strings.ToLower(img.meta.EditionID) == edition {
			return img, nil
		}
	}
	return nil, fmt.Errorf("diskwim: no image with edition %q", edition)
}

// ImageByName returns the first image whose name contains needle (case-insensitive).
func (w *WIM) ImageByName(name string) (*Image, error) {
	name = strings.ToLower(name)
	for _, img := range w.images {
		if img.meta != nil && strings.Contains(strings.ToLower(img.meta.Name), name) {
			return img, nil
		}
	}
	return nil, fmt.Errorf("diskwim: no image matching name %q", name)
}

// ── LE read helpers ───────────────────────────────────────────────────────────

func leUint16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func leUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func leUint64(b []byte) uint64 {
	return uint64(leUint32(b)) | uint64(leUint32(b[4:]))<<32
}