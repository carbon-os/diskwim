package diskwim

import (
	"fmt"
	"io"

	"github.com/carbon-os/diskwim/internal/decompress"
)

// readRawResource reads and decompresses a resource described by a rawReshdr.
// partIdx is the zero-based index into w.readers (0 = part 1).
func (w *WIM) readRawResource(res rawReshdr, partIdx int) ([]byte, error) {
	re := &resourceEntry{
		offset:   int64(res.Offset),
		compSize: res.compressedSize(),
		origSize: int64(res.OrigSize),
		flags:    res.flags(),
	}
	return w.readResource(re, partIdx)
}

// readResource reads and decompresses an arbitrary resource entry.
func (w *WIM) readResource(re *resourceEntry, partIdx int) ([]byte, error) {
	if partIdx < 0 || partIdx >= len(w.readers) {
		return nil, fmt.Errorf("diskwim: part %d not loaded", partIdx+1)
	}
	r := w.readers[partIdx]
	out := make([]byte, re.origSize)

	if re.flags&resFlagCompressed == 0 {
		// Uncompressed: read directly.
		if _, err := r.ReadAt(out, re.offset); err != nil {
			return nil, err
		}
		return out, nil
	}

	// Read the entire compressed resource.
	compressed := make([]byte, re.compSize)
	if _, err := r.ReadAt(compressed, re.offset); err != nil {
		return nil, fmt.Errorf("diskwim: read compressed resource at 0x%x: %w", re.offset, err)
	}

	// Solid LZMS: decompress as a single block.
	if re.flags&resFlagSolid != 0 || w.compression == CompressionLZMS {
		if err := decompress.LZMS(compressed, out); err != nil {
			return nil, fmt.Errorf("diskwim: LZMS decompress: %w", err)
		}
		return out, nil
	}

	// Chunked compression (XPRESS or LZX).
	return w.decompressChunked(compressed, re.origSize)
}

// decompressChunked decompresses a chunked XPRESS or LZX resource.
//
// Layout:
//   [chunkTable: (numChunks-1) × 4 bytes (or 8 if origSize > 4 GiB)]
//   [chunk0 data] [chunk1 data] ... [chunkN data]
//
// Each table entry is the cumulative end-offset of the corresponding chunk
// within the chunk data section (entry[i] = start of chunk i+1).
func (w *WIM) decompressChunked(compressed []byte, origSize int64) ([]byte, error) {
	cs := int64(w.chunkSize)
	numChunks := (origSize + cs - 1) / cs
	if numChunks == 0 {
		return nil, nil
	}

	// Determine entry width.
	entryWidth := 4
	if origSize > (1 << 32) {
		entryWidth = 8
	}

	tableSize := int64(numChunks-1) * int64(entryWidth)
	if int64(len(compressed)) < tableSize {
		return nil, fmt.Errorf("diskwim: compressed data too small for chunk table (%d vs %d)",
			len(compressed), tableSize)
	}

	// Parse chunk table: entry[i] = cumulative end-offset of chunk i (from
	// start of chunk data = after the chunk table).
	table := make([]int64, numChunks)
	for i := int64(0); i < numChunks-1; i++ {
		off := i * int64(entryWidth)
		if entryWidth == 4 {
			table[i] = int64(leUint32(compressed[off:]))
		} else {
			table[i] = int64(leUint64(compressed[off:]))
		}
	}
	// The last chunk ends at the end of all compressed data.
	table[numChunks-1] = int64(len(compressed)) - tableSize

	out := make([]byte, origSize)
	chunkData := compressed[tableSize:]

	for i := int64(0); i < numChunks; i++ {
		// Uncompressed range.
		dstStart := i * cs
		dstEnd := dstStart + cs
		if dstEnd > origSize {
			dstEnd = origSize
		}
		dst := out[dstStart:dstEnd]

		// Compressed range.
		var srcStart int64
		if i > 0 {
			srcStart = table[i-1]
		}
		srcEnd := table[i]
		if srcStart < 0 || srcEnd > int64(len(chunkData)) || srcStart > srcEnd {
			return nil, fmt.Errorf("diskwim: chunk %d has invalid bounds [%d, %d]", i, srcStart, srcEnd)
		}
		src := chunkData[srcStart:srcEnd]

		// A chunk whose compressed size equals its uncompressed size is stored raw.
		if int64(len(src)) == int64(len(dst)) {
			copy(dst, src)
			continue
		}

		var err error
		switch w.compression {
		case CompressionXPRESS:
			err = decompress.XPRESS(src, dst)
		case CompressionLZX:
			err = decompress.LZX(src, dst)
		default:
			copy(dst, src)
		}
		if err != nil {
			return nil, fmt.Errorf("diskwim: chunk %d decompress: %w", i, err)
		}
	}
	return out, nil
}

// openResourceByHash opens a streaming reader for the resource identified by
// its SHA-1 hash. The caller must drain the reader before calling it again.
func (w *WIM) openResourceByHash(hash [20]byte) (io.Reader, error) {
	re, ok := w.resources[hash]
	if !ok {
		return nil, fmt.Errorf("diskwim: resource %x not found in lookup table", hash)
	}
	// Determine which part reader to use (part numbers are 1-based).
	partIdx := 0
	if re.partNumber > 1 {
		partIdx = int(re.partNumber) - 1
	}
	data, err := w.readResource(re, partIdx)
	if err != nil {
		return nil, err
	}
	return bytesReader(data), nil
}