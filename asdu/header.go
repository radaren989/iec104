package asdu

import (
	"encoding/binary"
	"fmt"
)

// Bit layout of the two qualifier octets.
const (
	vsqSequenceBit = 0x80 // SQ: the objects share one base address
	vsqCountMask   = 0x7F

	cotNegativeBit = 0x40 // P/N: the activation was refused
	cotTestBit     = 0x80 // T: this frame is a test, do not act on it
	cotCauseMask   = 0x3F
)

// Header is the fixed part of an ASDU.
type Header struct {
	// Type says what the information objects are.
	Type TypeID

	// Sequence is the SQ bit. When set, the objects share one base address and
	// each subsequent object's address is one higher; when clear, every object
	// carries its own address. Both encodings describe the same data, and a
	// device may use either.
	Sequence bool

	// Count is how many information objects (SQ clear) or elements (SQ set) the
	// ASDU carries. 1..127; zero is malformed.
	Count uint8

	// Cause is why the ASDU was sent.
	Cause Cause

	// Negative is the P/N bit: the station is refusing what was asked. It is
	// only meaningful on a confirmation, and it is the difference between "the
	// breaker is opening" and "the breaker refused to open" — which are the same
	// frame apart from this one bit.
	Negative bool

	// Test is the T bit: this frame is part of a test and must not be acted on.
	Test bool

	// Originator is the originator address, present only when COTOctets is 2.
	// It lets a multi-master system route a confirmation back to whoever asked.
	Originator uint8

	// CommonAddr is the common address of the ASDU — the station address.
	CommonAddr uint16
}

func (h Header) String() string {
	s := fmt.Sprintf("%s casdu=%d cot=%s n=%d", h.Type, h.CommonAddr, h.Cause, h.Count)
	if h.Sequence {
		s += " sq"
	}
	if h.Negative {
		s += " P/N"
	}
	if h.Test {
		s += " test"
	}
	return s
}

// ParseHeader reads the fixed part of an ASDU and returns it with the remaining
// body.
//
// The returned body aliases b. That is safe by construction here: b is the ASDU
// that apci.Decode already copied out of the read buffer, so the caller owns it
// for as long as the frame is being handled.
func (p Params) ParseHeader(b []byte) (Header, []byte, error) {
	if err := p.Validate(); err != nil {
		return Header{}, nil, err
	}
	if len(b) < p.HeaderLen() {
		return Header{}, nil, fmt.Errorf("%w: %d octet(s), need %d for the header",
			ErrTruncated, len(b), p.HeaderLen())
	}

	h := Header{
		Type:     TypeID(b[0]),
		Sequence: b[1]&vsqSequenceBit != 0,
		Count:    b[1] & vsqCountMask,
	}
	if h.Count == 0 {
		return Header{}, nil, fmt.Errorf("%w: %s", ErrObjectCount, h.Type)
	}

	i := 2
	h.Cause = Cause(b[i] & cotCauseMask)
	h.Negative = b[i]&cotNegativeBit != 0
	h.Test = b[i]&cotTestBit != 0
	i++
	if p.COTOctets == 2 {
		h.Originator = b[i]
		i++
	}

	if p.CASDUOctets == 1 {
		h.CommonAddr = uint16(b[i])
		i++
	} else {
		h.CommonAddr = binary.LittleEndian.Uint16(b[i : i+2])
		i += 2
	}

	return h, b[i:], nil
}

// AppendTo renders the header onto dst.
func (h Header) AppendTo(dst []byte, p Params) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return dst, err
	}
	switch {
	case h.Count == 0:
		return dst, fmt.Errorf("%w: %s", ErrObjectCount, h.Type)
	case h.Count > MaxObjects:
		return dst, fmt.Errorf("%w: %d objects, the qualifier holds at most %d",
			ErrObjectCount, h.Count, MaxObjects)
	case h.Cause > cotCauseMask:
		return dst, fmt.Errorf("%w: cause %d does not fit six bits", ErrParams, h.Cause)
	case p.CASDUOctets == 1 && h.CommonAddr > 0xFF:
		return dst, fmt.Errorf("%w: common address %d does not fit one octet", ErrParams, h.CommonAddr)
	case p.COTOctets == 1 && h.Originator != 0:
		// Silently dropping the originator would produce a frame that looks
		// right and routes confirmations to the wrong master.
		return dst, fmt.Errorf("%w: originator address %d cannot be sent with cot_octets=1",
			ErrParams, h.Originator)
	}

	vsq := h.Count
	if h.Sequence {
		vsq |= vsqSequenceBit
	}
	cot := byte(h.Cause)
	if h.Negative {
		cot |= cotNegativeBit
	}
	if h.Test {
		cot |= cotTestBit
	}

	dst = append(dst, byte(h.Type), vsq, cot)
	if p.COTOctets == 2 {
		dst = append(dst, h.Originator)
	}
	if p.CASDUOctets == 1 {
		dst = append(dst, byte(h.CommonAddr))
	} else {
		dst = binary.LittleEndian.AppendUint16(dst, h.CommonAddr)
	}
	return dst, nil
}

// Element is one information object: its address and its raw element bytes.
type Element struct {
	IOA IOA

	// Data aliases the body passed to SplitElements. See ParseHeader for why
	// that is safe.
	Data []byte
}

// SplitElements carves the body into information objects, given how many octets
// one element occupies for this type ID.
//
// It handles both encodings and produces the same result from either, which is
// the point: a device may send fifty consecutive measurements as fifty
// addressed objects or as one base address followed by fifty elements, and
// nothing above this line should have to care which.
//
// The length is checked exactly. A body longer than the header promises is
// rejected rather than ignored — trailing octets mean the field widths are
// misconfigured, and that is precisely the failure that otherwise shows up as
// plausible values at the wrong addresses.
func (p Params) SplitElements(h Header, body []byte, elemLen int) ([]Element, error) {
	if elemLen < 0 {
		return nil, fmt.Errorf("%w: negative element length %d", ErrParams, elemLen)
	}
	ioaLen := int(p.IOAOctets)
	count := int(h.Count)

	var want int
	if h.Sequence {
		want = ioaLen + count*elemLen
	} else {
		want = count * (ioaLen + elemLen)
	}
	if len(body) != want {
		return nil, fmt.Errorf("%w: %s with %d object(s) and sq=%v needs %d octet(s), got %d",
			ErrLength, h.Type, count, h.Sequence, want, len(body))
	}

	out := make([]Element, count)

	if h.Sequence {
		base := p.readIOA(body)
		// The implied addresses must fit the configured width. Wrapping round
		// would put the tail of a sequence on top of low-numbered points, which
		// reads as real data.
		if last := base + IOA(count) - 1; last > p.MaxIOA() || last < base {
			return nil, fmt.Errorf("%w: sequence from %d for %d object(s) runs past the %d-octet maximum %d",
				ErrIOARange, base, count, p.IOAOctets, p.MaxIOA())
		}
		rest := body[ioaLen:]
		for i := range count {
			out[i] = Element{IOA: base + IOA(i), Data: rest[i*elemLen : (i+1)*elemLen]}
		}
		return out, nil
	}

	step := ioaLen + elemLen
	for i := range count {
		obj := body[i*step : (i+1)*step]
		out[i] = Element{IOA: p.readIOA(obj), Data: obj[ioaLen:]}
	}
	return out, nil
}

// AppendElements is the inverse of SplitElements: it writes information objects
// in whichever encoding h.Sequence asks for.
//
// With h.Sequence set only the first address is written and the rest must be
// consecutive; elements that are not are a caller bug and are refused rather
// than silently renumbered.
func (p Params) AppendElements(dst []byte, h Header, elems []Element) ([]byte, error) {
	if len(elems) != int(h.Count) {
		return dst, fmt.Errorf("%w: header says %d object(s), got %d", ErrLength, h.Count, len(elems))
	}
	for _, e := range elems {
		if e.IOA > p.MaxIOA() {
			return dst, fmt.Errorf("%w: %d does not fit %d octet(s)", ErrIOARange, e.IOA, p.IOAOctets)
		}
	}

	if h.Sequence {
		for i, e := range elems {
			if want := elems[0].IOA + IOA(i); e.IOA != want {
				return dst, fmt.Errorf("%w: sq=1 needs consecutive addresses; object %d is %d, expected %d",
					ErrIOARange, i, e.IOA, want)
			}
		}
		dst = p.appendIOA(dst, elems[0].IOA)
		for _, e := range elems {
			dst = append(dst, e.Data...)
		}
		return dst, nil
	}

	for _, e := range elems {
		dst = p.appendIOA(dst, e.IOA)
		dst = append(dst, e.Data...)
	}
	return dst, nil
}

// readIOA reads a little-endian address of the configured width. The caller has
// already checked that b is long enough.
func (p Params) readIOA(b []byte) IOA {
	switch p.IOAOctets {
	case 1:
		return IOA(b[0])
	case 2:
		return IOA(binary.LittleEndian.Uint16(b[:2]))
	default:
		// Three octets: there is no Uint24, and the standard is explicit that
		// the low octet comes first.
		return IOA(b[0]) | IOA(b[1])<<8 | IOA(b[2])<<16
	}
}

func (p Params) appendIOA(dst []byte, ioa IOA) []byte {
	switch p.IOAOctets {
	case 1:
		return append(dst, byte(ioa))
	case 2:
		return binary.LittleEndian.AppendUint16(dst, uint16(ioa))
	default:
		return append(dst, byte(ioa), byte(ioa>>8), byte(ioa>>16))
	}
}
