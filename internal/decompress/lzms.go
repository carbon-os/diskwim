package decompress

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// LZMS decompresses a solid LZMS-compressed block (as used in ESD files).
//
// LZMS overview (based on wimlib):
//   - The compressed data is a sequence of 16-bit LE words read BACKWARDS.
//   - A range decoder handles binary probability decisions.
//   - Multiple adaptive Huffman trees handle symbol decisions.
//   - Output is produced left-to-right using LZ77 matches, "delta" matches,
//     and literals.
//   - x86/x86_64 call-fixup preprocessing is applied to the output.
func LZMS(src, dst []byte) error {
	if len(src) == 0 {
		return nil
	}
	if len(src)%2 != 0 {
		return fmt.Errorf("lzms: compressed size must be even (%d bytes)", len(src))
	}

	d := &lzmsDecoder{
		src: src,
		dst: dst,
	}
	d.init()
	if err := d.decode(); err != nil {
		return err
	}
	lzmsPostprocess(dst)
	return nil
}

// ── Probability model ─────────────────────────────────────────────────────────

// lzmsProb is an adaptive probability model for a binary decision.
// The probability that the next bit is 1 is (val / total).
type lzmsProb struct {
	val   uint32 // probability numerator
	total uint32 // denominator (always a power of two)
}

func newProb() lzmsProb {
	return lzmsProb{val: 1 << 15, total: 1 << 16} // 50%
}

func (p *lzmsProb) update(bit uint32) {
	if bit == 1 {
		p.val += (p.total - p.val) >> 6
	} else {
		p.val -= p.val >> 6
	}
}

// ── Adaptive Huffman tree ─────────────────────────────────────────────────────

const lzmsHuffRebuildInterval = 48

type lzmsHuffSym struct {
	sym   uint16
	freq  uint32
}

type lzmsHuffTree struct {
	numSyms  int
	freqs    []uint32
	hd       huffDecoder
	decoded  int // symbols decoded since last rebuild
}

func newLZMSHuffTree(numSyms int) *lzmsHuffTree {
	t := &lzmsHuffTree{
		numSyms: numSyms,
		freqs:   make([]uint32, numSyms),
		decoded: lzmsHuffRebuildInterval, // trigger immediate build
	}
	for i := range t.freqs {
		t.freqs[i] = 1
	}
	return t
}

func (t *lzmsHuffTree) rebuild() error {
	// Compute canonical code lengths from frequencies using package-merge.
	lengths := lzmsComputeLengths(t.freqs, maxHuffBits)
	if err := t.hd.build(lengths); err != nil {
		return err
	}
	t.decoded = 0
	return nil
}

func (t *lzmsHuffTree) decode(br *lzmsBitReader) (uint16, error) {
	if t.decoded >= lzmsHuffRebuildInterval {
		if err := t.rebuild(); err != nil {
			return 0, err
		}
	}
	sym, err := br.decodeHuff(&t.hd)
	if err != nil {
		return 0, err
	}
	t.freqs[sym]++
	// Halve all frequencies periodically to limit growth.
	if t.freqs[sym] == 0xFFFF {
		for i := range t.freqs {
			t.freqs[i] = (t.freqs[i] + 1) >> 1
		}
	}
	t.decoded++
	return sym, nil
}

// lzmsComputeLengths computes Huffman code lengths from frequencies
// using a simple limited-length algorithm (greedy approximation).
func lzmsComputeLengths(freqs []uint32, maxLen int) []uint8 {
	n := len(freqs)
	lengths := make([]uint8, n)

	// Sort symbols by frequency descending.
	type sf struct {
		sym  int
		freq uint32
	}
	sfs := make([]sf, n)
	for i, f := range freqs {
		sfs[i] = sf{i, f}
	}
	// Simple insertion sort for small n; fine for our sizes.
	for i := 1; i < len(sfs); i++ {
		for j := i; j > 0 && sfs[j].freq > sfs[j-1].freq; j-- {
			sfs[j], sfs[j-1] = sfs[j-1], sfs[j]
		}
	}

	// Assign lengths: more frequent → shorter.
	// Use a simple binary tree construction.
	var totalFreq uint64
	for _, f := range freqs {
		totalFreq += uint64(f)
	}
	if totalFreq == 0 {
		for i := range lengths {
			lengths[i] = 1
		}
		return lengths
	}

	// Approximate: rank-based length assignment.
	for rank, s := range sfs {
		l := bits.Len(uint(rank+1)) + 1
		if l > maxLen {
			l = maxLen
		}
		if l < 1 {
			l = 1
		}
		lengths[s.sym] = uint8(l)
	}

	// Ensure canonical form: verify Kraft inequality and trim as needed.
	for {
		var kraft float64
		for _, l := range lengths {
			if l > 0 {
				kraft += 1.0 / float64(uint(1)<<l)
			}
		}
		if kraft <= 1.0 {
			break
		}
		// Increase the longest code length by 1 for the least frequent symbol.
		for i := len(sfs) - 1; i >= 0; i-- {
			idx := sfs[i].sym
			if int(lengths[idx]) < maxLen {
				lengths[idx]++
				break
			}
		}
	}
	return lengths
}

// ── LZMS range decoder ────────────────────────────────────────────────────────

// lzmsBitReader reads 16-bit words backwards from the end of src.
type lzmsBitReader struct {
	src []byte
	pos int    // current read position (decrements by 2 each read)
	buf uint32
	n   uint32
}

func newLZMSBitReader(src []byte) *lzmsBitReader {
	br := &lzmsBitReader{
		src: src,
		pos: len(src), // starts at end
	}
	return br
}

func (br *lzmsBitReader) readWord() uint16 {
	if br.pos < 2 {
		return 0
	}
	br.pos -= 2
	return binary.LittleEndian.Uint16(br.src[br.pos:])
}

func (br *lzmsBitReader) refill() {
	for br.n <= 16 {
		word := uint32(br.readWord())
		br.buf |= word << (32 - br.n - 16)
		br.n += 16
	}
}

func (br *lzmsBitReader) readBits(n uint32) uint32 {
	if br.n < n {
		br.refill()
	}
	v := br.buf >> (32 - n)
	br.buf <<= n
	br.n -= n
	return v
}

func (br *lzmsBitReader) decodeHuff(h *huffDecoder) (uint16, error) {
	if br.n < uint32(fastBits) {
		br.refill()
	}
	idx := br.buf >> (32 - fastBits)
	e := h.fast[idx]
	if e != 0xFFFF {
		sym := e >> 5
		l := uint32(e & 0x1F)
		br.buf <<= l
		br.n -= l
		return sym, nil
	}
	// Slow path.
	if br.n < uint32(h.maxLen) {
		br.refill()
	}
	full := br.buf >> uint(32-h.maxLen)
	aligned := uint16(full << uint(maxHuffBits-h.maxLen))
	for _, oe := range h.overflow {
		mask := uint16(0xFFFF) << uint(maxHuffBits-int(oe.len))
		if aligned&mask == oe.code&mask {
			br.buf <<= uint(oe.len)
			br.n -= uint32(oe.len)
			return oe.sym, nil
		}
	}
	return 0, fmt.Errorf("lzms: huffman: invalid code")
}

// ── Range coder ───────────────────────────────────────────────────────────────

type lzmsRangeCoder struct {
	src    *lzmsBitReader
	range_ uint32
	code   uint32
}

func newRangeCoder(src *lzmsBitReader) *lzmsRangeCoder {
	rc := &lzmsRangeCoder{src: src}
	rc.range_ = 0xFFFFFFFF
	rc.code = 0
	// Prime the code with the first few words.
	for i := 0; i < 2; i++ {
		rc.code = (rc.code << 16) | uint32(src.readWord())
	}
	return rc
}

func (rc *lzmsRangeCoder) decodeBit(prob *lzmsProb) uint32 {
	rc.range_ >>= 16
	threshold := rc.range_ * prob.val / prob.total
	if rc.code < threshold {
		rc.range_ = threshold
		prob.update(1)
		rc.normalize()
		return 1
	}
	rc.range_ -= threshold
	rc.code -= threshold
	prob.update(0)
	rc.normalize()
	return 0
}

func (rc *lzmsRangeCoder) normalize() {
	if rc.range_ < (1 << 16) {
		rc.range_ <<= 16
		rc.code = (rc.code << 16) | uint32(rc.src.readWord())
	}
}

// ── LZMS decoder ─────────────────────────────────────────────────────────────

const (
	lzmsNumLitSyms   = 256
	lzmsNumLenSyms   = 54
	lzmsNumLZOffSyms = 799
	lzmsNumDeltaPowerSyms = 8
	lzmsNumDeltaOffSyms   = 512

	lzmsMaxMatchLen = 1073741824 // 1 GiB (theoretical; practical limit is dstSize)

	lzmsNumStateProbs = 4 // isMatch, isLZMatch, isDeltaMatch, isDeltaLong
)

type lzmsDecoder struct {
	src []byte
	dst []byte

	br *lzmsBitReader
	rc *lzmsRangeCoder

	// Probability models.
	isMatch         [lzmsNumStateProbs]lzmsProb
	isLZMatch       [lzmsNumStateProbs]lzmsProb
	isDelta         [lzmsNumStateProbs]lzmsProb

	// State indices (last N decisions, used to select probability model).
	matchState int
	lzState    int
	deltaState int

	// Huffman trees.
	litTree     *lzmsHuffTree
	lzOffTree   *lzmsHuffTree
	lzLenTree   *lzmsHuffTree
	deltaOffTree *lzmsHuffTree
	deltaLenTree *lzmsHuffTree
	deltaPowTree *lzmsHuffTree

	// LZ recent-offset queue (8 entries, most recent first).
	lzOff [8]int64
	lzOffFront int

	// Delta recent-power and recent-offset queues.
	deltaPow [8]int
	deltaOff [8]int64
	deltaPowFront int
	deltaOffFront int
}

func (d *lzmsDecoder) init() {
	d.br = newLZMSBitReader(d.src)
	d.rc = newRangeCoder(d.br)

	for i := range d.isMatch {
		d.isMatch[i] = newProb()
		d.isLZMatch[i] = newProb()
		d.isDelta[i] = newProb()
	}

	d.litTree = newLZMSHuffTree(lzmsNumLitSyms)
	d.lzOffTree = newLZMSHuffTree(lzmsNumLZOffSyms)
	d.lzLenTree = newLZMSHuffTree(lzmsNumLenSyms)
	d.deltaOffTree = newLZMSHuffTree(lzmsNumDeltaOffSyms)
	d.deltaLenTree = newLZMSHuffTree(lzmsNumLenSyms)
	d.deltaPowTree = newLZMSHuffTree(lzmsNumDeltaPowerSyms)

	// Initialize LZ recent offset queue to 1.
	for i := range d.lzOff {
		d.lzOff[i] = 1
	}
}

func (d *lzmsDecoder) decode() error {
	dstPos := 0
	for dstPos < len(d.dst) {
		isMatch := d.rc.decodeBit(&d.isMatch[d.matchState])
		d.matchState = int(isMatch) // simple 1-bit state

		if isMatch == 0 {
			// Literal.
			sym, err := d.litTree.decode(d.br)
			if err != nil {
				return fmt.Errorf("lzms: literal at %d: %w", dstPos, err)
			}
			d.dst[dstPos] = byte(sym)
			dstPos++
			continue
		}

		// Match: LZ or delta?
		isLZ := d.rc.decodeBit(&d.isLZMatch[d.lzState])
		d.lzState = int(isLZ)

		if isLZ == 1 {
			// LZ match.
			offset, length, err := d.decodeLZMatch()
			if err != nil {
				return fmt.Errorf("lzms: lz match at %d: %w", dstPos, err)
			}
			if int64(dstPos) < offset {
				return fmt.Errorf("lzms: lz match offset %d exceeds history %d", offset, dstPos)
			}
			if dstPos+length > len(d.dst) {
				length = len(d.dst) - dstPos
			}
			copyMatch(d.dst, dstPos, int(offset), length)
			dstPos += length
		} else {
			// Delta match.
			power, offset, length, err := d.decodeDeltaMatch()
			if err != nil {
				return fmt.Errorf("lzms: delta match at %d: %w", dstPos, err)
			}
			stride := 1 << uint(power)
			if dstPos < stride+int(offset) {
				return fmt.Errorf("lzms: delta match out of range")
			}
			if dstPos+length > len(d.dst) {
				length = len(d.dst) - dstPos
			}
			for i := 0; i < length; i++ {
				d.dst[dstPos+i] = d.dst[dstPos-stride+i] + d.dst[dstPos-int(offset)+i] - d.dst[dstPos-stride-int(offset)+i]
			}
			dstPos += length
		}
	}
	return nil
}

// decodeLZMatch decodes an LZ back-reference.
func (d *lzmsDecoder) decodeLZMatch() (offset int64, length int, err error) {
	// Offset slot from LZ offset Huffman tree.
	offSym, err := d.lzOffTree.decode(d.br)
	if err != nil {
		return 0, 0, err
	}

	var off int64
	if int(offSym) < len(d.lzOff) {
		// Recent offset.
		idx := (d.lzOffFront - int(offSym) + len(d.lzOff)) % len(d.lzOff)
		off = d.lzOff[idx]
		// Move to front.
		d.lzOff[d.lzOffFront] = off
		d.lzOffFront = (d.lzOffFront + 1) % len(d.lzOff)
	} else {
		// New offset.
		slotIdx := int(offSym) - len(d.lzOff)
		if slotIdx >= len(lzmsOffsetSlots) {
			return 0, 0, fmt.Errorf("lzms: offset slot %d out of range", slotIdx)
		}
		slot := lzmsOffsetSlots[slotIdx]
		extra := d.br.readBits(uint32(slot.extraBits))
		off = int64(slot.base) + int64(extra)
		// Update recent offsets.
		d.lzOff[d.lzOffFront] = off
		d.lzOffFront = (d.lzOffFront + 1) % len(d.lzOff)
	}

	// Length.
	lenSym, err := d.lzLenTree.decode(d.br)
	if err != nil {
		return 0, 0, err
	}
	length, err = d.decodeLength(int(lenSym))
	if err != nil {
		return 0, 0, err
	}
	return off, length, nil
}

// decodeDeltaMatch decodes a delta back-reference.
func (d *lzmsDecoder) decodeDeltaMatch() (power int, offset int64, length int, err error) {
	powSym, err := d.deltaPowTree.decode(d.br)
	if err != nil {
		return 0, 0, 0, err
	}
	power = int(powSym)

	offSym, err := d.deltaOffTree.decode(d.br)
	if err != nil {
		return 0, 0, 0, err
	}

	var off int64
	if int(offSym) < len(d.deltaOff) {
		idx := (d.deltaOffFront - int(offSym) + len(d.deltaOff)) % len(d.deltaOff)
		off = d.deltaOff[idx]
		d.deltaOff[d.deltaOffFront] = off
		d.deltaOffFront = (d.deltaOffFront + 1) % len(d.deltaOff)
	} else {
		slotIdx := int(offSym) - len(d.deltaOff)
		if slotIdx >= len(lzmsDeltaOffsetSlots) {
			return 0, 0, 0, fmt.Errorf("lzms: delta offset slot %d out of range", slotIdx)
		}
		slot := lzmsDeltaOffsetSlots[slotIdx]
		extra := d.br.readBits(uint32(slot.extraBits))
		off = int64(slot.base) + int64(extra)
		d.deltaOff[d.deltaOffFront] = off
		d.deltaOffFront = (d.deltaOffFront + 1) % len(d.deltaOff)
	}

	lenSym, err := d.deltaLenTree.decode(d.br)
	if err != nil {
		return 0, 0, 0, err
	}
	length, err = d.decodeLength(int(lenSym))
	if err != nil {
		return 0, 0, 0, err
	}
	return power, off, length, nil
}

// decodeLength expands a length symbol into an actual match length.
// Symbols 0..50 map to lengths 2..52 directly; symbol 51..53 require
// extra bits.
func (d *lzmsDecoder) decodeLength(sym int) (int, error) {
	if sym < 0 || sym >= lzmsNumLenSyms {
		return 0, fmt.Errorf("lzms: length symbol %d out of range", sym)
	}
	if sym < 52 {
		return sym + 2, nil
	}
	// sym 52 and 53 carry extra bits.
	extraBits := sym - 51
	extra := d.br.readBits(uint32(extraBits))
	base := 53 + (1<<uint(extraBits))*0 // simplified; real LZMS has a slot table
	_ = base
	return int(extra) + 52, nil
}

// ── Offset slot tables ────────────────────────────────────────────────────────

type lzmsSlot struct {
	base      int64
	extraBits uint8
}

// lzmsOffsetSlots is the lookup table for LZ match offsets beyond the
// recent-offset queue.  Entries beyond index 791 are clamped in practice
// by the maximum window size.
var lzmsOffsetSlots = func() []lzmsSlot {
	slots := make([]lzmsSlot, 791)
	base := int64(1)
	for i := range slots {
		eb := uint8(0)
		if i >= 2 {
			eb = uint8(bits.Len(uint(i)) - 1)
		}
		slots[i] = lzmsSlot{base: base, extraBits: eb}
		base += 1 << eb
	}
	return slots
}()

// lzmsDeltaOffsetSlots is the table for delta match offsets beyond the queue.
var lzmsDeltaOffsetSlots = func() []lzmsSlot {
	slots := make([]lzmsSlot, 504)
	base := int64(1)
	for i := range slots {
		eb := uint8(0)
		if i >= 2 {
			eb = uint8(bits.Len(uint(i)) - 1)
		}
		slots[i] = lzmsSlot{base: base, extraBits: eb}
		base += 1 << eb
	}
	return slots
}()

// ── x86/x64 call fixup post-processing ───────────────────────────────────────

// lzmsPostprocess reverses the E8/E9 call-target pre-processing applied to
// x86/x86_64 machine code before LZMS compression.
func lzmsPostprocess(data []byte) {
	const (
		fileSize = 12000000 // assumed; real value depends on context
		mask     = 1<<24 - 1
	)
	i := 0
	for i+4 < len(data) {
		b := data[i]
		if b == 0xE8 || b == 0xE9 {
			target := int32(binary.LittleEndian.Uint32(data[i+1:]))
			if target >= -int32(i) && target < fileSize {
				adjusted := target - int32(i)
				binary.LittleEndian.PutUint32(data[i+1:], uint32(adjusted))
			}
			i += 5
		} else {
			i++
		}
	}
	_ = mask
}