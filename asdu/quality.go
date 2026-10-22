package asdu

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Bit positions shared by QDS, SIQ and DIQ. The four upper flags sit in the same
// places in all three, which is why one Quality type serves all of them.
const (
	qOverflowBit    = 0x01 // OV - QDS only; SIQ and DIQ have no overflow
	qBlockedBit     = 0x10 // BL
	qSubstitutedBit = 0x20 // SB
	qNotTopicalBit  = 0x40 // NT
	qInvalidBit     = 0x80 // IV

	siqValueBit  = 0x01 // SPI
	diqStateMask = 0x03 // DPI

	vtiValueMask    = 0x7F
	vtiTransientBit = 0x80

	bcrSequenceMask = 0x1F
	bcrCarryBit     = 0x20 // CY
	bcrAdjustedBit  = 0x40 // CA
	bcrInvalidBit   = 0x80 // IV
)

// Quality is the descriptor that travels with almost every measured value.
//
// None of these flags makes the value unusable by itself — that is a policy
// decision, and it belongs to whoever consumes the value, not to the parser.
// What this layer must not do is lose them: DeviceManager derives _Quality from
// these bits, and a flag dropped here does not produce an error anywhere. It
// produces a perfectly ordinary-looking reading whose alarm can no longer
// explain itself.
type Quality struct {
	// Overflow (OV): the value ran past the range its type can represent. Only
	// QDS carries it; on SIQ and DIQ it is always false.
	Overflow bool

	// Blocked (BL): the point is blocked for transmission at the device, so the
	// value is frozen at whatever it was when blocking started.
	Blocked bool

	// Substituted (SB): a human entered this value, it was not measured.
	Substituted bool

	// NotTopical (NT): the device could not refresh the value, so it is stale.
	// This is the flag that says "the field wiring or the sensor is gone but the
	// RTU is fine" — the value is the last good one, not a current reading.
	NotTopical bool

	// Invalid (IV): the device itself says this value cannot be trusted.
	Invalid bool
}

// ParseQDS reads a quality descriptor octet, including the overflow bit.
func ParseQDS(b byte) Quality {
	q := parseQualityFlags(b)
	q.Overflow = b&qOverflowBit != 0
	return q
}

// parseQualityFlags reads the four flags shared by QDS, SIQ and DIQ.
func parseQualityFlags(b byte) Quality {
	return Quality{
		Blocked:     b&qBlockedBit != 0,
		Substituted: b&qSubstitutedBit != 0,
		NotTopical:  b&qNotTopicalBit != 0,
		Invalid:     b&qInvalidBit != 0,
	}
}

// QDS renders the quality as a descriptor octet. Reserved bits are zero.
func (q Quality) QDS() byte {
	b := q.flagBits()
	if q.Overflow {
		b |= qOverflowBit
	}
	return b
}

func (q Quality) flagBits() byte {
	var b byte
	if q.Blocked {
		b |= qBlockedBit
	}
	if q.Substituted {
		b |= qSubstitutedBit
	}
	if q.NotTopical {
		b |= qNotTopicalBit
	}
	if q.Invalid {
		b |= qInvalidBit
	}
	return b
}

// Good reports whether no flag is set at all. It is a convenience for logs and
// tests, not a policy: what to do about a flagged value is decided above this
// layer.
func (q Quality) Good() bool { return q == Quality{} }

func (q Quality) String() string {
	if q.Good() {
		return "good"
	}
	var set []string
	for _, f := range []struct {
		on   bool
		name string
	}{
		{q.Invalid, "IV"}, {q.NotTopical, "NT"}, {q.Substituted, "SB"},
		{q.Blocked, "BL"}, {q.Overflow, "OV"},
	} {
		if f.on {
			set = append(set, f.name)
		}
	}
	return strings.Join(set, "|")
}

// --- SIQ: single point ------------------------------------------------------

// SinglePoint is one octet: a boolean and its quality.
type SinglePoint struct {
	On bool
	Q  Quality
}

// ParseSIQ reads a single-point information octet.
func ParseSIQ(b byte) SinglePoint {
	return SinglePoint{On: b&siqValueBit != 0, Q: parseQualityFlags(b)}
}

// SIQ renders the point as an information octet.
func (s SinglePoint) SIQ() byte {
	b := s.Q.flagBits()
	if s.On {
		b |= siqValueBit
	}
	return b
}

// --- DIQ: double point ------------------------------------------------------

// DoublePointState is the two-bit DPI field.
//
// The two indeterminate states are NOT the same thing and are deliberately kept
// apart. A double point is two auxiliary contacts wired in opposition: 1 is one
// closed, 2 is the other closed. 0 means NEITHER is closed — the mechanism is
// mid-travel, or a contact has failed open. 3 means BOTH are closed, which is
// electrically impossible in a healthy circuit and therefore means a wiring
// fault or a stuck contact.
//
// Collapsing them to one "indeterminate" throws away the difference between "the
// breaker is moving" and "the breaker's position sensing is broken", and those
// call for opposite responses.
type DoublePointState uint8

const (
	// DPIntermediate (0): neither contact closed. Transitional, or a contact
	// failed open.
	DPIntermediate DoublePointState = 0

	// DPOff (1): determined OFF.
	DPOff DoublePointState = 1

	// DPOn (2): determined ON.
	DPOn DoublePointState = 2

	// DPIndeterminate (3): both contacts closed. Not a transition — a fault.
	DPIndeterminate DoublePointState = 3
)

func (s DoublePointState) String() string {
	switch s {
	case DPIntermediate:
		return "intermediate"
	case DPOff:
		return "off"
	case DPOn:
		return "on"
	case DPIndeterminate:
		return "indeterminate"
	default:
		return fmt.Sprintf("DoublePointState(%d)", uint8(s))
	}
}

// Determined reports whether the state is a definite ON or OFF. The two other
// values are not positions; a caller that needs a boolean must decide what to do
// with them rather than defaulting to false.
func (s DoublePointState) Determined() bool { return s == DPOff || s == DPOn }

// DoublePoint is one octet: a two-bit state and its quality.
type DoublePoint struct {
	State DoublePointState
	Q     Quality
}

// ParseDIQ reads a double-point information octet.
func ParseDIQ(b byte) DoublePoint {
	return DoublePoint{State: DoublePointState(b & diqStateMask), Q: parseQualityFlags(b)}
}

// DIQ renders the point as an information octet.
func (d DoublePoint) DIQ() byte {
	return d.Q.flagBits() | byte(d.State)&diqStateMask
}

// --- VTI: step position -----------------------------------------------------

// StepPosition is a transformer tap position or similar: a signed seven-bit
// value plus a transient flag, followed by a separate quality octet.
type StepPosition struct {
	// Value is -64..63.
	Value int8

	// Transient says the equipment is in motion, so the position is the one it
	// is passing through rather than one it has reached. Acting on a transient
	// position is how a tap changer gets a command while it is still moving.
	Transient bool

	Q Quality
}

// StepPositionLen is the width of a VTI plus its quality octet.
const StepPositionLen = 2

// ParseVTI reads a value-with-transient octet and the quality octet after it.
func ParseVTI(b []byte) (StepPosition, error) {
	if len(b) < StepPositionLen {
		return StepPosition{}, fmt.Errorf("%w: %d octet(s), a step position needs %d",
			ErrTruncated, len(b), StepPositionLen)
	}
	return StepPosition{
		Value:     sevenBitSigned(b[0] & vtiValueMask),
		Transient: b[0]&vtiTransientBit != 0,
		Q:         ParseQDS(b[1]),
	}, nil
}

// AppendTo renders the step position and its quality.
func (s StepPosition) AppendTo(dst []byte) []byte {
	v := byte(s.Value) & vtiValueMask
	if s.Transient {
		v |= vtiTransientBit
	}
	return append(dst, v, s.Q.QDS())
}

// sevenBitSigned sign-extends a seven-bit two's-complement value.
//
// The seventh bit is the sign, not part of the magnitude: read as unsigned, a
// tap position of -1 becomes 127. That is inside the plausible range for a
// transformer, so nothing looks wrong.
func sevenBitSigned(b byte) int8 {
	if b&0x40 != 0 {
		return int8(b | 0x80)
	}
	return int8(b)
}

// --- BCR: binary counter reading --------------------------------------------

// CounterLen is the width of a binary counter reading.
const CounterLen = 5

// Counter is an integrated total: a 32-bit value plus the bookkeeping that says
// whether it can be compared with the previous one.
type Counter struct {
	Value int32

	// Sequence is the five-bit sequence number the device increments on each
	// freeze. It is what makes two readings comparable: consecutive numbers mean
	// consecutive intervals, a repeat means the same freeze delivered twice, and
	// a gap means an interval was missed entirely. Dropping it turns energy
	// counters into numbers nobody can integrate.
	Sequence uint8

	// Carry (CY) says the counter overflowed during the interval, so the
	// difference from the previous reading is not the consumption.
	Carry bool

	// Adjusted (CA) says the counter was set or reset during the interval, so
	// the difference is meaningless rather than merely wrong.
	Adjusted bool

	// Invalid (IV) says the device does not trust the reading.
	Invalid bool
}

// ParseBCR reads a binary counter reading.
func ParseBCR(b []byte) (Counter, error) {
	if len(b) < CounterLen {
		return Counter{}, fmt.Errorf("%w: %d octet(s), a counter needs %d",
			ErrTruncated, len(b), CounterLen)
	}
	return Counter{
		Value:    int32(binary.LittleEndian.Uint32(b[0:4])),
		Sequence: b[4] & bcrSequenceMask,
		Carry:    b[4]&bcrCarryBit != 0,
		Adjusted: b[4]&bcrAdjustedBit != 0,
		Invalid:  b[4]&bcrInvalidBit != 0,
	}, nil
}

// AppendTo renders the counter reading.
func (c Counter) AppendTo(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(c.Value))
	flags := c.Sequence & bcrSequenceMask
	if c.Carry {
		flags |= bcrCarryBit
	}
	if c.Adjusted {
		flags |= bcrAdjustedBit
	}
	if c.Invalid {
		flags |= bcrInvalidBit
	}
	return append(dst, flags)
}

// Comparable reports whether this reading can be differenced against the
// previous one to get a consumption. It is false when the counter wrapped or was
// adjusted, or when the device marked it invalid.
func (c Counter) Comparable() bool { return !c.Carry && !c.Adjusted && !c.Invalid }
