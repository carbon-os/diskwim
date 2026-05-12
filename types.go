package diskwim

import (
	"encoding/binary"
	"fmt"
	"io"
)

// CompressionType identifies the compression algorithm used in a WIM.
type CompressionType uint8

const (
	CompressionNone   CompressionType = 0
	CompressionXPRESS CompressionType = 1
	CompressionLZX    CompressionType = 2
	CompressionLZMS   CompressionType = 3 // ESD / solid
)

func (c CompressionType) String() string {
	switch c {
	case CompressionNone:
		return "None"
	case CompressionXPRESS:
		return "XPRESS"
	case CompressionLZX:
		return "LZX"
	case CompressionLZMS:
		return "LZMS"
	default:
		return fmt.Sprintf("CompressionType(%d)", uint8(c))
	}
}

// ── WIM header flags ──────────────────────────────────────────────────────────

const (
	flagCompression    uint32 = 0x00000002
	flagReadOnly       uint32 = 0x00000004
	flagSpanned        uint32 = 0x00000008
	flagResourceOnly   uint32 = 0x00000010
	flagMetadataOnly   uint32 = 0x00000020
	flagCompressXPRESS uint32 = 0x00020000
	flagCompressLZX    uint32 = 0x00040000
	flagCompressLZMS   uint32 = 0x00080000 // solid ESD
)

// ── Resource entry flags ──────────────────────────────────────────────────────

const (
	resFlagFree       uint8 = 0x01
	resFlagMetadata   uint8 = 0x02
	resFlagCompressed uint8 = 0x04
	resFlagSpanned    uint8 = 0x08
	resFlagSolid      uint8 = 0x10
)

// ── On-disk structures (binary.Read compatible) ───────────────────────────────

const wimMagic = "MSWIM\x00\x00\x00"

// rawHeader is the 208-byte WIM file header (WIMHEADER_V1_PACKED).
// Layout: magic(8) + size(4) + version(4) + flags(4) + chunkSize(4) +
//         guid(16) + part(2) + total(2) + imageCount(4) +
//         lookupRes(24) + xmlRes(24) + bootRes(24) + bootIdx(4) +
//         intRes(24) + reserved(60) = 208 bytes.
type rawHeader struct {
	Magic      [8]byte
	Size       uint32
	Version    uint32
	Flags      uint32
	ChunkSize  uint32
	GUID       [16]byte
	PartNumber uint16
	TotalParts uint16
	ImageCount uint32
	LookupRes  rawReshdr
	XMLRes     rawReshdr
	BootRes    rawReshdr
	BootIndex  uint32
	IntRes     rawReshdr
	Reserved   [60]byte
}

// rawReshdr is the 24-byte on-disk resource descriptor (_RESHDR_DISK_SHORT).
// bytes[0:7] = 7-byte LE compressed size; bytes[7] = flags.
type rawReshdr struct {
	Packed   [8]byte // [0:7]=compressedSize LE, [7]=flags
	Offset   uint64
	OrigSize uint64
}

func (r rawReshdr) compressedSize() int64 {
	// The 7 low bytes are the compressed size; the high byte is flags.
	v := binary.LittleEndian.Uint64(r.Packed[:])
	return int64(v & 0x00FFFFFFFFFFFFFF)
}

func (r rawReshdr) flags() uint8 { return r.Packed[7] }

func (r rawReshdr) isEmpty() bool {
	return r.compressedSize() == 0 && r.Offset == 0
}

// rawLookupEntry is the 50-byte on-disk lookup table entry (_RESHDR_DISK).
type rawLookupEntry struct {
	Packed   [8]byte
	Offset   uint64
	OrigSize uint64
	Part     uint16
	RefCount uint32
	Hash     [20]byte
}

func (e rawLookupEntry) compressedSize() int64 {
	v := binary.LittleEndian.Uint64(e.Packed[:])
	return int64(v & 0x00FFFFFFFFFFFFFF)
}

func (e rawLookupEntry) flags() uint8 { return e.Packed[7] }

// ── resourceEntry is the in-memory resource descriptor ───────────────────────

type resourceEntry struct {
	offset       int64
	compSize     int64
	origSize     int64
	flags        uint8
	partNumber   uint16 // which WIM part holds this resource
}

// ── helpers ───────────────────────────────────────────────────────────────────

// readStructAt reads a fixed-size binary struct from r at the given offset.
func readStructAt(r io.ReaderAt, off int64, v interface{}) error {
	size := int64(binary.Size(v))
	buf := make([]byte, size)
	if _, err := r.ReadAt(buf, off); err != nil {
		return err
	}
	return binary.Read(bytesReader(buf), binary.LittleEndian, v)
}

// bytesReader wraps a []byte as an io.Reader.
func bytesReader(b []byte) io.Reader {
	return &sliceReader{b: b}
}

type sliceReader struct {
	b   []byte
	pos int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.pos:])
	s.pos += n
	return n, nil
}

// utf16ToString converts a []byte of UTF-16LE encoded text to a Go string.
func utf16ToString(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	// Strip BOM if present.
	if b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	// Trim null terminator if any.
	for len(u16) > 0 && u16[len(u16)-1] == 0 {
		u16 = u16[:len(u16)-1]
	}
	runes := make([]rune, 0, len(u16))
	for i := 0; i < len(u16); {
		r := rune(u16[i])
		i++
		if r >= 0xD800 && r < 0xDC00 && i < len(u16) {
			low := rune(u16[i])
			if low >= 0xDC00 && low < 0xE000 {
				r = 0x10000 + (r-0xD800)*0x400 + (low - 0xDC00)
				i++
			}
		}
		runes = append(runes, r)
	}
	return string(runes)
}