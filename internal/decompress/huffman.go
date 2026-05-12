// Package decompress provides pure-Go WIM decompressors for XPRESS, LZX, and LZMS.
package decompress

import "fmt"

// maxHuffBits is the maximum Huffman code length we support.
const maxHuffBits = 16

// huffDecoder is a canonical Huffman decoder using a direct-lookup table
// for codes up to fastBits in length and a linear scan for longer codes.
const fastBits = 11

type huffDecoder struct {
	// fast[code >> (16-fastBits)] → packed uint16: high (16-fastBits) bits =
	// symbol, low fastBits bits is unused; we store (sym<<5)|len.
	fast    [1 << fastBits]uint16
	// overflow: sorted by code, used when fast lookup returns len > fastBits
	overflow []huffOverflow
	maxLen   int
}

type huffOverflow struct {
	code uint16
	sym  uint16
	len  uint8
}

// build constructs the decoder from a slice of code lengths (one per symbol).
// Lengths of 0 mean the symbol is not used.
func (h *huffDecoder) build(lengths []uint8) error {
	maxLen := 0
	for _, l := range lengths {
		if int(l) > maxLen {
			maxLen = int(l)
		}
	}
	if maxLen == 0 {
		return nil
	}
	if maxLen > maxHuffBits {
		return fmt.Errorf("huffman: max code length %d exceeds %d", maxLen, maxHuffBits)
	}
	h.maxLen = maxLen

	// Count symbols per length.
	count := [maxHuffBits + 1]int{}
	for _, l := range lengths {
		if l > 0 {
			count[l]++
		}
	}

	// Prevent out-of-bounds panics by validating Kraft inequality (oversubscription)
	var capacity uint32
	for bits := 1; bits <= maxLen; bits++ {
		capacity += uint32(count[bits]) << uint(maxHuffBits-bits)
	}
	if capacity > (1 << maxHuffBits) {
		return fmt.Errorf("huffman: invalid tree (oversubscribed)")
	}

	// First canonical code per length.
	code := [maxHuffBits + 1]uint16{}
	var c uint16
	for bits := 1; bits <= maxLen; bits++ {
		c = (c + uint16(count[bits-1])) << 1
		code[bits] = c
	}

	// Clear fast table.
	for i := range h.fast {
		h.fast[i] = 0xFFFF // invalid
	}
	h.overflow = h.overflow[:0]

	for sym, l := range lengths {
		if l == 0 {
			continue
		}
		cur := code[l]
		code[l]++

		if int(l) <= fastBits {
			// Fill all entries in the fast table that share this prefix.
			base := int(cur) << (fastBits - int(l))
			fill := 1 << (fastBits - int(l))
			entry := uint16(sym)<<5 | uint16(l)
			for i := 0; i < fill; i++ {
				h.fast[base+i] = entry
			}
		} else {
			// Align to 16-bit for overflow.
			fullCode := cur << uint(maxHuffBits-int(l))
			h.overflow = append(h.overflow, huffOverflow{
				code: fullCode,
				sym:  uint16(sym),
				len:  l,
			})
		}
	}
	return nil
}

// ── Bit reader (MSB-first, 16-bit words, little-endian in the byte stream) ───

type bitReader struct {
	src   []byte
	pos   int    // byte position
	buf   uint32 // bit buffer (valid bits are in the top `n` bits)
	n     uint32 // number of valid bits
}

func newBitReader(src []byte) *bitReader {
	br := &bitReader{src: src}
	br.refill()
	return br
}

func (br *bitReader) refill() {
	for br.n <= 16 {
		if br.pos+1 >= len(br.src) {
			// Pad with zero words at end of stream.
			br.buf |= 0 << (32 - br.n - 16)
			br.n += 16
			return
		}
		word := uint32(br.src[br.pos]) | uint32(br.src[br.pos+1])<<8
		br.pos += 2
		// Shift into the top of buf.
		br.buf |= word << (32 - br.n - 16)
		br.n += 16
	}
}

// peek returns the top n bits without consuming them.
func (br *bitReader) peek(n uint32) uint32 {
	return br.buf >> (32 - n)
}

// consume removes n bits from the top of the buffer.
func (br *bitReader) consume(n uint32) {
	br.buf <<= n
	br.n -= n
	if br.n <= 16 {
		br.refill()
	}
}

// readBits reads and returns n bits.
func (br *bitReader) readBits(n uint32) uint32 {
	v := br.peek(n)
	br.consume(n)
	return v
}

// readByte reads 8 bits as a byte.
func (br *bitReader) readByte() byte {
	return byte(br.readBits(8))
}

// decodeHuff decodes one Huffman symbol using the fast-path table.
func (br *bitReader) decodeHuff(h *huffDecoder) (uint16, error) {
	if h.maxLen == 0 {
		return 0, fmt.Errorf("huffman: empty tree")
	}
	idx := br.peek(fastBits)
	e := h.fast[idx]
	if e != 0xFFFF {
		sym := e >> 5
		l := uint32(e & 0x1F)
		br.consume(l)
		return sym, nil
	}
	// Slow path: read up to maxLen bits and scan overflow table.
	full := br.peek(uint32(h.maxLen))
	// Align to 16-bit for comparison.
	aligned := uint16(full << uint(maxHuffBits-h.maxLen))
	for _, oe := range h.overflow {
		mask := uint16(0xFFFF) << uint(maxHuffBits-int(oe.len))
		if aligned&mask == oe.code&mask {
			br.consume(uint32(oe.len))
			return oe.sym, nil
		}
	}
	return 0, fmt.Errorf("huffman: invalid code 0x%04x (maxLen=%d)", aligned, h.maxLen)
}

// ── Length decode helper table (shared by LZX and XPRESS) ───────────────────

// buildHuffFromNibbles builds a huffDecoder from n nibble-packed lengths
// stored in the given bytes (low nibble first).
func buildHuffFromNibbles(data []byte, numSyms int) (*huffDecoder, error) {
	if len(data)*2 < numSyms {
		return nil, fmt.Errorf("huffman: nibble table too small: have %d bytes for %d symbols", len(data), numSyms)
	}
	lengths := make([]uint8, numSyms)
	for i := 0; i < numSyms; i++ {
		b := data[i/2]
		if i%2 == 0 {
			lengths[i] = b & 0xF
		} else {
			lengths[i] = b >> 4
		}
	}
	h := &huffDecoder{}
	return h, h.build(lengths)
}