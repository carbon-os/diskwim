package decompress

import (
	"fmt"
)

// XPRESS decompresses a single XPRESS-Huffman chunk.
//
// Layout of src:
//   [0:256]   nibble-packed Huffman table (512 code lengths)
//   [256:]    Huffman-coded bitstream (16-bit LE words, MSB-first)
//
// A symbol S in [0,255] is a literal byte.
// A symbol S in [256,511] encodes a back-reference:
//
//	matchHeader  = S - 256
//	lenHdr       = matchHeader >> 4      (0..15)
//	offSlot      = matchHeader & 0xF     (0..15)
//	offset       = (1 << offSlot) | readBits(offSlot)
//	length       = lenHdr + 3
//	if lenHdr == 15:
//	    extra   = readBits(8)
//	    length  = 18 + extra
//	    if extra == 255:
//	        length = 270 + readBits(16)
func XPRESS(src, dst []byte) error {
	const tableBytes = 256
	if len(src) < tableBytes {
		return fmt.Errorf("xpress: src too short (%d bytes)", len(src))
	}
	h, err := buildHuffFromNibbles(src[:tableBytes], 512)
	if err != nil {
		return fmt.Errorf("xpress: build huffman: %w", err)
	}

	br := newBitReader(src[tableBytes:])
	dstPos := 0

	for dstPos < len(dst) {
		sym, err := br.decodeHuff(h)
		if err != nil {
			return fmt.Errorf("xpress: decode symbol at dstPos=%d: %w", dstPos, err)
		}

		if sym < 256 {
			// Literal.
			if dstPos >= len(dst) {
				return fmt.Errorf("xpress: output overflow on literal")
			}
			dst[dstPos] = byte(sym)
			dstPos++
			continue
		}

		// Back-reference.
		matchHeader := sym - 256
		lenHdr := matchHeader >> 4
		offSlot := matchHeader & 0xF

		var offsetExtra uint32
		if offSlot > 0 {
			offsetExtra = br.readBits(uint32(offSlot))
		}
		offset := int((1 << offSlot) | offsetExtra)
		if offset == 0 {
			return fmt.Errorf("xpress: zero offset")
		}

		length := int(lenHdr) + 3
		if lenHdr == 15 {
			extra := br.readBits(8)
			length = 18 + int(extra)
			if extra == 255 {
				length = 270 + int(br.readBits(16))
			}
		}

		if dstPos+length > len(dst) {
			return fmt.Errorf("xpress: match extends past output end (pos=%d, len=%d, dstSize=%d)",
				dstPos, length, len(dst))
		}
		if offset > dstPos {
			return fmt.Errorf("xpress: match offset %d exceeds history %d", offset, dstPos)
		}

		copyMatch(dst, dstPos, offset, length)
		dstPos += length
	}
	return nil
}

// copyMatch copies `length` bytes from dst[dstPos-offset] to dst[dstPos:].
// Handles overlapping copies correctly (run-length extension).
func copyMatch(dst []byte, dstPos, offset, length int) {
	src := dstPos - offset
	for i := 0; i < length; i++ {
		dst[dstPos+i] = dst[src+i]
	}
}