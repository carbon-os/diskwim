package decompress

import (
	"encoding/binary"
	"fmt"
)

// LZX decompresses a single LZX-compressed WIM chunk.
//
// The WIM variant uses a fixed 32 768-byte (2^15) window, which gives
// 30 position slots.  E8/E9 call-fixup pre-processing is NOT applied at
// the chunk level (it is applied per-file by the WIM writer, so the
// decompressor receives already-processed data).
//
// Block types:
//
//	1  VERBATIM     – main tree + length tree
//	2  ALIGNED      – main tree + length tree + 8-symbol aligned-offset tree
//	3  UNCOMPRESSED – raw bytes (aligned to 16-bit boundary)
func LZX(src, dst []byte) error {
	d := &lzxDecoder{
		src: src,
		dst: dst,
	}
	d.initPositionSlots()
	// LZX bit reader is big-endian 16-bit words, MSB first (same as XPRESS).
	d.br = newBitReader(src)
	if err := d.decode(); err != nil {
		return err
	}
	return nil
}

// ── Position slot tables (for window = 32 768 = 2^15) ────────────────────────

// numPositionSlots is the number of position slots for a 32 KB window.
const numPositionSlots = 30

var posSlotBase = [numPositionSlots]int{
	0, 1, 2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64,
	96, 128, 192, 256, 384, 512, 768, 1024, 1536,
	2048, 3072, 4096, 6144, 8192, 12288, 16384, 24576,
}

var posSlotExtraBits = [numPositionSlots]uint8{
	0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5,
	5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11,
	12, 12, 13, 13,
}

// offsetToSlot maps an offset to its position slot index.
func offsetToSlot(offset int) int {
	for i := numPositionSlots - 1; i >= 0; i-- {
		if offset >= posSlotBase[i] {
			return i
		}
	}
	return 0
}

// ── LZX decoder ───────────────────────────────────────────────────────────────

const (
	lzxNumLiterals        = 256
	lzxNumLengthHeaders   = 8
	lzxNumMainSymbols     = lzxNumLiterals + lzxNumLengthHeaders*numPositionSlots
	lzxNumLengthSymbols   = 249
	lzxNumAlignedSymbols  = 8
	lzxMinMatch           = 2
	lzxMaxMatch           = 257
	lzxNumRecentOffsets   = 3
)

type lzxDecoder struct {
	src []byte
	dst []byte
	br  *bitReader

	// Huffman trees (persisted across blocks as deltas).
	mainLens   [lzxNumMainSymbols]uint8
	lenLens    [lzxNumLengthSymbols]uint8
	alignedLens [lzxNumAlignedSymbols]uint8

	mainTree    huffDecoder
	lenTree     huffDecoder
	alignedTree huffDecoder

	// Recent offsets (R0, R1, R2).
	r [lzxNumRecentOffsets]int

	posSlotBase     [numPositionSlots]int
	posSlotExtraBits [numPositionSlots]uint8
}

func (d *lzxDecoder) initPositionSlots() {
	copy(d.posSlotBase[:], posSlotBase[:])
	copy(d.posSlotExtraBits[:], posSlotExtraBits[:])
	d.r[0] = 1
	d.r[1] = 1
	d.r[2] = 1
}

func (d *lzxDecoder) decode() error {
	dstPos := 0
	for dstPos < len(d.dst) {
		// Read 3-bit block type.
		blockType := d.br.readBits(3)
		// Read 24-bit uncompressed block size.
		blockSizeHigh := d.br.readBits(8)
		blockSizeLow := d.br.readBits(16)
		blockSize := int(blockSizeHigh<<16 | blockSizeLow)
		if blockSize == 0 || dstPos+blockSize > len(d.dst) {
			blockSize = len(d.dst) - dstPos
		}

		switch blockType {
		case 1: // VERBATIM
			if err := d.readMainTree(); err != nil {
				return err
			}
			if err := d.readLengthTree(); err != nil {
				return err
			}
			if err := d.decodeSymbols(dstPos, blockSize, false); err != nil {
				return err
			}
		case 2: // ALIGNED OFFSET
			if err := d.readAlignedTree(); err != nil {
				return err
			}
			if err := d.readMainTree(); err != nil {
				return err
			}
			if err := d.readLengthTree(); err != nil {
				return err
			}
			if err := d.decodeSymbols(dstPos, blockSize, true); err != nil {
				return err
			}
		case 3: // UNCOMPRESSED
			// Align the bit reader to a 16-bit boundary.
			if d.br.n%16 != 0 {
				d.br.readBits(d.br.n % 16)
			}
			// Read R0, R1, R2 from raw bytes.
			if d.br.pos+12 > len(d.src) {
				return fmt.Errorf("lzx: uncompressed block: not enough bytes for R values")
			}
			d.r[0] = int(binary.LittleEndian.Uint32(d.src[d.br.pos:]))
			d.r[1] = int(binary.LittleEndian.Uint32(d.src[d.br.pos+4:]))
			d.r[2] = int(binary.LittleEndian.Uint32(d.src[d.br.pos+8:]))
			d.br.pos += 12
			// Copy raw bytes.
			if d.br.pos+blockSize > len(d.src) {
				return fmt.Errorf("lzx: uncompressed block: not enough src bytes")
			}
			copy(d.dst[dstPos:], d.src[d.br.pos:d.br.pos+blockSize])
			d.br.pos += blockSize
			// Re-align to 16-bit.
			if blockSize%2 != 0 {
				d.br.pos++
			}
			// Re-initialise the bit buffer.
			d.br.buf = 0
			d.br.n = 0
			d.br.refill()
		default:
			return fmt.Errorf("lzx: unknown block type %d", blockType)
		}
		dstPos += blockSize
	}
	return nil
}

// readTreeDeltas reads a Huffman tree definition encoded as delta lengths.
// If first is true, the previous lengths are all 0 (first block).
func (d *lzxDecoder) readDeltaTree(prev []uint8, numSyms int) error {
	// A 20-symbol "pre-tree" encodes the deltas.
	preTreeLens := make([]uint8, 20)
	for i := range preTreeLens {
		preTreeLens[i] = uint8(d.br.readBits(4))
	}
	var preTree huffDecoder
	if err := preTree.build(preTreeLens); err != nil {
		return fmt.Errorf("lzx: pre-tree: %w", err)
	}

	i := 0
	for i < numSyms {
		sym, err := d.br.decodeHuff(&preTree)
		if err != nil {
			return fmt.Errorf("lzx: pre-tree decode at sym %d: %w", i, err)
		}
		switch {
		case sym <= 16:
			// Delta from previous length (mod 17).
			prev[i] = uint8((17 + int(prev[i]) - int(sym)) % 17)
			i++
		case sym == 17:
			// Run of 0-length (small).
			runLen := int(d.br.readBits(4)) + 4
			for j := 0; j < runLen && i < numSyms; j++ {
				prev[i] = 0
				i++
			}
		case sym == 18:
			// Run of 0-length (large).
			runLen := int(d.br.readBits(5)) + 20
			for j := 0; j < runLen && i < numSyms; j++ {
				prev[i] = 0
				i++
			}
		case sym == 19:
			// Run of same length.
			runLen := int(d.br.readBits(1)) + 4
			nextSym, err2 := d.br.decodeHuff(&preTree)
			if err2 != nil {
				return fmt.Errorf("lzx: pre-tree run decode: %w", err2)
			}
			l := uint8((17 + int(prev[i]) - int(nextSym)) % 17)
			for j := 0; j < runLen && i < numSyms; j++ {
				prev[i] = l
				i++
			}
		default:
			return fmt.Errorf("lzx: invalid pre-tree symbol %d", sym)
		}
	}
	return nil
}

func (d *lzxDecoder) readMainTree() error {
	if err := d.readDeltaTree(d.mainLens[:], lzxNumMainSymbols); err != nil {
		return fmt.Errorf("lzx: main tree: %w", err)
	}
	return d.mainTree.build(d.mainLens[:])
}

func (d *lzxDecoder) readLengthTree() error {
	if err := d.readDeltaTree(d.lenLens[:], lzxNumLengthSymbols); err != nil {
		return fmt.Errorf("lzx: length tree: %w", err)
	}
	return d.lenTree.build(d.lenLens[:])
}

func (d *lzxDecoder) readAlignedTree() error {
	for i := range d.alignedLens {
		d.alignedLens[i] = uint8(d.br.readBits(3))
	}
	return d.alignedTree.build(d.alignedLens[:])
}

func (d *lzxDecoder) decodeSymbols(dstPos, blockSize int, aligned bool) error {
	end := dstPos + blockSize
	for dstPos < end {
		sym, err := d.br.decodeHuff(&d.mainTree)
		if err != nil {
			return fmt.Errorf("lzx: main tree decode: %w", err)
		}

		if int(sym) < lzxNumLiterals {
			// Literal.
			d.dst[dstPos] = byte(sym)
			dstPos++
			continue
		}

		// Match.
		matchSym := int(sym) - lzxNumLiterals
		lenHeader := matchSym % lzxNumLengthHeaders
		posSlot := matchSym / lzxNumLengthHeaders

		// Decode match length.
		matchLen := lenHeader + lzxMinMatch
		if lenHeader == lzxNumLengthHeaders-1 {
			lenSym, err2 := d.br.decodeHuff(&d.lenTree)
			if err2 != nil {
				return fmt.Errorf("lzx: length tree decode: %w", err2)
			}
			matchLen = int(lenSym) + lzxNumLengthHeaders + lzxMinMatch - 1
		}

		// Decode match offset.
		var matchOffset int
		switch posSlot {
		case 0:
			matchOffset = d.r[0]
		case 1:
			matchOffset = d.r[1]
			d.r[1] = d.r[0]
			d.r[0] = matchOffset
		case 2:
			matchOffset = d.r[2]
			d.r[2] = d.r[1]
			d.r[1] = d.r[0]
			d.r[0] = matchOffset
		default:
			extra := int(d.posSlotExtraBits[posSlot])
			var footprint int
			if aligned && extra >= 3 {
				// Read (extra-3) verbatim bits then 3 bits from aligned tree.
				if extra > 3 {
					hi := int(d.br.readBits(uint32(extra - 3)))
					footprint = hi << 3
				}
				alignSym, err2 := d.br.decodeHuff(&d.alignedTree)
				if err2 != nil {
					return fmt.Errorf("lzx: aligned offset decode: %w", err2)
				}
				footprint |= int(alignSym)
			} else {
				footprint = int(d.br.readBits(uint32(extra)))
			}
			matchOffset = d.posSlotBase[posSlot] + footprint
			// Update recent offsets (verbatim match).
			d.r[2] = d.r[1]
			d.r[1] = d.r[0]
			d.r[0] = matchOffset
		}

		if matchOffset <= 0 || dstPos-matchOffset < 0 {
			return fmt.Errorf("lzx: invalid match offset %d at dstPos=%d", matchOffset, dstPos)
		}
		if dstPos+matchLen > len(d.dst) {
			return fmt.Errorf("lzx: match extends past output")
		}
		copyMatch(d.dst, dstPos, matchOffset, matchLen)
		dstPos += matchLen
	}
	return nil
}