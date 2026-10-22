package asdu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrUnsupportedType means this gateway has no mapping for the type.
//
// It is NOT a protocol violation and must not close the connection. Study
// Reference §12: log and skip. A station is entitled to send types outside what
// we implement, and dropping the link over one would take every other point on
// that RTU down with it — the failure would be enormously larger than the cause.
var ErrUnsupportedType = errors.New("unsupported ASDU type")

// ValueKind says which of Point's three value fields carries the reading. It
// mirrors the contract's oneof, so the mapping to PointValue is mechanical.
type ValueKind uint8

const (
	KindBool  ValueKind = iota // Point.Bool
	KindFloat                  // Point.Float
	KindInt                    // Point.Int
)

func (k ValueKind) String() string {
	switch k {
	case KindBool:
		return "bool"
	case KindFloat:
		return "float"
	case KindInt:
		return "int"
	default:
		return fmt.Sprintf("ValueKind(%d)", uint8(k))
	}
}

// normalisedFullScale is what a normalised value is divided by. A normalised
// value is a signed 16-bit fraction of full scale, so the range is
// -1 .. +1-2^-15 — it never quite reaches +1, which is why the divisor is 32768
// and not 32767.
const normalisedFullScale = 32768

// Point is one information object, decoded.
//
// The three scalar fields mirror the contract's oneof and Kind says which one is
// live. The fields after Q carry what the contract's shape cannot: a double
// point's two indeterminate states, a tap changer in motion, a counter's
// sequence number. They are kept because losing them here is silent — see the
// package comment — and whoever maps a Point onto the wire can then decide
// deliberately what to do with them.
type Point struct {
	Type TypeID
	IOA  IOA

	Kind  ValueKind
	Bool  bool
	Float float64
	Int   int64

	Q Quality

	// Cause is the cause of transmission the ASDU carried. It is a header
	// field, shared by every object in the frame, and it is copied onto each
	// point here rather than left for the caller to remember.
	//
	// That copy is the whole reason this field exists: causes 11 and 12 say a
	// value changed BECAUSE of a remote or a local command, which is the
	// difference between "the breaker opened" and "the breaker opened because
	// someone opened it". A caller that has to fetch it from the header
	// separately is a caller that will one day forget, and forgetting loses no
	// value — only the answer to "why".
	Cause Cause

	// Negative is the header's P/N bit and Test its T bit, carried along for
	// the same reason.
	Negative bool
	Test     bool

	// HasTime says the type carried a CP56Time2a. Ask Time.Usable before
	// trusting the instant: a device may send a tag it has itself marked
	// invalid.
	HasTime bool
	Time    CP56Time2a

	// DoubleState is the raw two-bit state, meaningful for M_DP_NA_1 and
	// M_DP_TB_1 only. Bool is set from it via Determined/DPOn, so a caller that
	// only wants a boolean can ignore it — but a caller that needs to tell "the
	// breaker is moving" from "the breaker's position sensing is broken" must
	// look here.
	DoubleState DoublePointState

	// Transient is the VTI transient flag, meaningful for M_ST_NA_1 and
	// M_ST_TB_1 only: the equipment is still moving.
	Transient bool

	// CounterSeq, CounterCarry and CounterAdjusted are meaningful for M_IT_NA_1
	// and M_IT_TB_1 only. Without the sequence number two counter readings
	// cannot be differenced into a consumption.
	CounterSeq      uint8
	CounterCarry    bool
	CounterAdjusted bool
}

// layout describes how one information element of a type is laid out.
type layout struct {
	// elemLen is the element width in octets, excluding the information object
	// address. SplitElements needs it before anything can be decoded.
	elemLen int

	kind    ValueKind
	hasTime bool

	// decode fills the value, quality and per-type fields from the element
	// bytes, which are exactly elemLen long. The timestamp, when hasTime, is
	// the last seven octets and is handled by the caller so that no decoder has
	// to remember to do it.
	decode func(pt *Point, b []byte)
}

// monitorLayouts is the registry of monitor-direction types this gateway
// understands.
//
// Absent types are not errors — see ErrUnsupportedType. Four are absent on
// purpose:
//
//	20     M_PS_NA_1 packed single points with change detection. One object
//	       carries sixteen points and two bitmaps, so it does not collapse to a
//	       single value the way everything else here does; supporting it means a
//	       different shape all the way up. The reference RTU does not send it.
//	38-40  M_EP_T*_1 protection equipment events. They carry their own event
//	       structures (SEP/SPE/OCI, QDP, a CP16 elapsed time) that nothing else
//	       uses. The reference RTU does not send them either.
//
// Both can be added when a device actually sends them; until then they are
// machinery for a frame nobody produces, and the log-and-skip path handles them
// correctly in the meantime.
var monitorLayouts = map[TypeID]layout{
	// --- without a time tag ---
	MSpNa1: {elemLen: 1, kind: KindBool, decode: decodeSinglePoint},
	MDpNa1: {elemLen: 1, kind: KindBool, decode: decodeDoublePoint},
	MStNa1: {elemLen: 2, kind: KindInt, decode: decodeStepPosition},
	MBoNa1: {elemLen: 5, kind: KindInt, decode: decodeBitstring},
	MMeNa1: {elemLen: 3, kind: KindFloat, decode: decodeNormalised},
	MMeNb1: {elemLen: 3, kind: KindFloat, decode: decodeScaled},
	MMeNc1: {elemLen: 5, kind: KindFloat, decode: decodeShortFloat},
	MItNa1: {elemLen: 5, kind: KindInt, decode: decodeCounter},

	// M_ME_ND_1 is the odd one out: a normalised value with NO quality octet.
	// The absent descriptor is not "good quality", it is no information, and
	// the zero Quality says exactly that.
	MMeNd1: {elemLen: 2, kind: KindFloat, decode: decodeNormalisedNoQuality},

	// --- with CP56Time2a ---
	MSpTb1: {elemLen: 1 + CP56Time2aLen, kind: KindBool, hasTime: true, decode: decodeSinglePoint},
	MDpTb1: {elemLen: 1 + CP56Time2aLen, kind: KindBool, hasTime: true, decode: decodeDoublePoint},
	MStTb1: {elemLen: 2 + CP56Time2aLen, kind: KindInt, hasTime: true, decode: decodeStepPosition},
	MBoTb1: {elemLen: 5 + CP56Time2aLen, kind: KindInt, hasTime: true, decode: decodeBitstring},
	MMeTd1: {elemLen: 3 + CP56Time2aLen, kind: KindFloat, hasTime: true, decode: decodeNormalised},
	MMeTe1: {elemLen: 3 + CP56Time2aLen, kind: KindFloat, hasTime: true, decode: decodeScaled},
	MMeTf1: {elemLen: 5 + CP56Time2aLen, kind: KindFloat, hasTime: true, decode: decodeShortFloat},
	MItTb1: {elemLen: 5 + CP56Time2aLen, kind: KindInt, hasTime: true, decode: decodeCounter},
}

// SupportsMonitorType reports whether this gateway can decode the type.
func SupportsMonitorType(t TypeID) bool {
	_, ok := monitorLayouts[t]
	return ok
}

// ElementLen returns how many octets one information element of the type
// occupies, excluding its address.
func ElementLen(t TypeID) (int, error) {
	l, ok := monitorLayouts[t]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrUnsupportedType, t)
	}
	return l.elemLen, nil
}

// ParseMonitor decodes a monitor-direction ASDU into points.
//
// The three shapes each measurand comes in — no time tag, and with CP56Time2a —
// collapse to the same Point here. That is the whole job of this function: above
// this line nothing should have to know that the same measurement arrives as
// type 9 during an interrogation and as type 34 when it changes on its own.
//
// A type outside the registry returns ErrUnsupportedType. The caller counts it,
// drops the ASDU and keeps the connection: see that error's comment.
func (p Params) ParseMonitor(h Header, body []byte) ([]Point, error) {
	if err := h.Type.CheckPermitted(); err != nil {
		return nil, err
	}
	l, ok := monitorLayouts[h.Type]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, h.Type)
	}

	elems, err := p.SplitElements(h, body, l.elemLen)
	if err != nil {
		return nil, err
	}

	points := make([]Point, len(elems))
	for i, e := range elems {
		pt := Point{
			Type:     h.Type,
			IOA:      e.IOA,
			Kind:     l.kind,
			Cause:    h.Cause,
			Negative: h.Negative,
			Test:     h.Test,
		}

		data := e.Data
		if l.hasTime {
			// The tag is always the last seven octets. Taking it off here means
			// no decoder has to remember it exists, which is the sort of thing
			// that gets forgotten in exactly one of eight places.
			split := len(data) - CP56Time2aLen
			tag, err := ParseCP56Time2a(data[split:])
			if err != nil {
				return nil, fmt.Errorf("%s at IOA %d: %w", h.Type, e.IOA, err)
			}
			pt.HasTime = true
			pt.Time = tag
			data = data[:split]
		}

		l.decode(&pt, data)
		points[i] = pt
	}
	return points, nil
}

// --- per-type decoders ------------------------------------------------------
//
// Each one receives exactly the value octets, with any time tag already removed,
// so none of them can read past its element.

func decodeSinglePoint(pt *Point, b []byte) {
	sp := ParseSIQ(b[0])
	pt.Bool = sp.On
	pt.Q = sp.Q
}

func decodeDoublePoint(pt *Point, b []byte) {
	dp := ParseDIQ(b[0])
	pt.DoubleState = dp.State
	pt.Q = dp.Q

	// Bool is the convenience view and can only be honest about two of the four
	// states. The other two are not "off": they are "no position to report", so
	// the reading is marked invalid rather than silently reported as OFF.
	// DoubleState still carries which of the two it was.
	pt.Bool = dp.State == DPOn
	if !dp.State.Determined() {
		pt.Q.Invalid = true
	}
}

func decodeStepPosition(pt *Point, b []byte) {
	// ParseVTI cannot fail here: the layout guarantees two octets.
	sp, _ := ParseVTI(b)
	pt.Int = int64(sp.Value)
	pt.Transient = sp.Transient
	pt.Q = sp.Q
}

// decodeBitstring reads a 32-bit bit pattern plus its quality.
//
// The value is a pattern, not a number, so it is zero-extended into Int rather
// than sign-extended: bit 31 set means "that bit is on", not "negative".
func decodeBitstring(pt *Point, b []byte) {
	pt.Int = int64(binary.LittleEndian.Uint32(b[0:4]))
	pt.Q = ParseQDS(b[4])
}

func decodeNormalised(pt *Point, b []byte) {
	pt.Float = normalised(b[0:2])
	pt.Q = ParseQDS(b[2])
}

// decodeNormalisedNoQuality handles M_ME_ND_1, which carries no quality octet.
// Quality is left at its zero value — no flags set — because the device said
// nothing about it, and inventing anything else would be inventing data.
func decodeNormalisedNoQuality(pt *Point, b []byte) {
	pt.Float = normalised(b[0:2])
}

func decodeScaled(pt *Point, b []byte) {
	pt.Float = float64(int16(binary.LittleEndian.Uint16(b[0:2])))
	pt.Q = ParseQDS(b[2])
}

func decodeShortFloat(pt *Point, b []byte) {
	pt.Float = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:4])))
	pt.Q = ParseQDS(b[4])
}

func decodeCounter(pt *Point, b []byte) {
	// ParseBCR cannot fail here: the layout guarantees five octets.
	c, _ := ParseBCR(b)
	pt.Int = int64(c.Value)
	pt.CounterSeq = c.Sequence
	pt.CounterCarry = c.Carry
	pt.CounterAdjusted = c.Adjusted
	pt.Q = Quality{Invalid: c.Invalid}
}

// normalised converts a signed 16-bit fraction of full scale.
func normalised(b []byte) float64 {
	return float64(int16(binary.LittleEndian.Uint16(b))) / normalisedFullScale
}
