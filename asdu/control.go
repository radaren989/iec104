package asdu

import (
	"errors"
	"fmt"
	"math"
)

// Bit layout of the command qualifier octets.
//
// SCO, DCO and RCO share a shape: the state in the low bits, the output
// duration in bits 2-6, and the select/execute bit at the top. QOS, used by the
// set-point commands, has a seven-bit qualifier and the same top bit.
const (
	cmdSelectBit = 0x80 // S/E, in every command qualifier

	scoStateBit  = 0x01 // SCS: single command state
	dcoStateMask = 0x03 // DCS
	rcoStateMask = 0x03 // RCS

	cmdDurationShift = 2
	cmdDurationMask  = 0x1F // QU occupies bits 2-6

	qosQualifierMask = 0x7F // QL
)

// ErrCommand means a command cannot be encoded. Nothing is written when it is
// returned — see AppendCommand.
var ErrCommand = errors.New("invalid command")

// OutputDuration is QU: how long the output stays energised.
type OutputDuration uint8

const (
	// DurationNone (0): no additional definition. The device uses whatever it
	// is configured for, which is the normal choice — the master rarely knows
	// the contactor better than the RTU does.
	DurationNone OutputDuration = 0

	DurationShortPulse OutputDuration = 1
	DurationLongPulse  OutputDuration = 2

	// DurationPersistent (3): the output stays on until commanded otherwise.
	DurationPersistent OutputDuration = 3
)

// MaxOutputDuration is the largest value this gateway will send. The field is
// five bits wide, but 4..31 are reserved by the standard or private to a vendor,
// and sending a reserved qualifier to a device that interprets it privately is
// how an output behaves in a way nobody can explain. Contract VALIDATION.md §7.
const MaxOutputDuration = DurationPersistent

func (d OutputDuration) String() string {
	switch d {
	case DurationNone:
		return "no-definition"
	case DurationShortPulse:
		return "short-pulse"
	case DurationLongPulse:
		return "long-pulse"
	case DurationPersistent:
		return "persistent"
	default:
		return fmt.Sprintf("OutputDuration(%d)", uint8(d))
	}
}

// DoubleCommand is the two-bit state of a double command.
//
// The mapping is counter-intuitive and is the single most dangerous constant in
// this package: 1 is OFF and 2 is ON. Swapping them drives the breaker the wrong
// way, and the frame is perfectly valid, so nothing anywhere reports an error.
type DoubleCommand uint8

const (
	// DoubleNotPermitted0 (0) and DoubleNotPermitted3 (3) are "indeterminate".
	// They are not positions, and sending one to a breaker is undefined
	// behaviour. Both are refused before any octet is written.
	DoubleNotPermitted0 DoubleCommand = 0

	DoubleOff DoubleCommand = 1 // yes, 1 is OFF
	DoubleOn  DoubleCommand = 2 // and 2 is ON

	DoubleNotPermitted3 DoubleCommand = 3
)

func (c DoubleCommand) String() string {
	switch c {
	case DoubleOff:
		return "off"
	case DoubleOn:
		return "on"
	default:
		return fmt.Sprintf("not-permitted(%d)", uint8(c))
	}
}

// Permitted reports whether the state is one a device may be given.
func (c DoubleCommand) Permitted() bool { return c == DoubleOff || c == DoubleOn }

// StepCommand is the two-bit direction of a regulating step command, with the
// same 0-and-3-are-not-values rule as DoubleCommand.
type StepCommand uint8

const (
	StepNotPermitted0 StepCommand = 0
	StepLower         StepCommand = 1
	StepHigher        StepCommand = 2
	StepNotPermitted3 StepCommand = 3
)

func (c StepCommand) String() string {
	switch c {
	case StepLower:
		return "lower"
	case StepHigher:
		return "higher"
	default:
		return fmt.Sprintf("not-permitted(%d)", uint8(c))
	}
}

// Permitted reports whether the direction is one a device may be given.
func (c StepCommand) Permitted() bool { return c == StepLower || c == StepHigher }

// Command is one control-direction command.
//
// Which fields matter depends on Type; the others are ignored. Encoding
// validates everything first and writes nothing on failure, because a
// half-written command buffer is a frame nobody meant to send.
type Command struct {
	Type TypeID

	// IOA is the point to drive. Zero is refused: in 101/104 address 0 means
	// "not used", so a command aimed at it is a configuration mistake that the
	// device would answer in an implementation-defined way.
	IOA IOA

	// Select is the S/E bit. True is the first half of select-before-operate:
	// the device reserves the point and answers, and only then does an execute
	// with the same values actually drive the output. False is direct operate.
	Select bool

	// Duration is QU, used by C_SC, C_DC and C_RC.
	Duration OutputDuration

	// On is the state for C_SC_NA_1 and C_SC_TA_1.
	On bool

	// Double is the state for C_DC_NA_1 and C_DC_TA_1.
	Double DoubleCommand

	// Step is the direction for C_RC_NA_1 and C_RC_TA_1.
	Step StepCommand

	// Value is the set point for the normalised (C_SE_NA_1, C_SE_TA_1) and
	// short float (C_SE_NC_1, C_SE_TC_1) commands. Normalised set points are
	// refused outside -1..+1 rather than wrapped.
	Value float64

	// Scaled is the set point for C_SE_NB_1 and C_SE_TB_1.
	Scaled int16

	// Bits is the pattern for C_BO_NA_1 and C_BO_TA_1.
	Bits uint32

	// Time is required by the timestamped variants (58..64) and ignored by the
	// others. The standard uses it to let a device refuse a command that spent
	// too long in transit.
	Time CP56Time2a
}

// controlLayout says what a control type carries.
type controlLayout struct {
	hasTime bool

	// encode writes the element, qualifier included, and returns an error only
	// after the shared checks in AppendCommand have passed.
	encode func(dst []byte, c Command) ([]byte, error)
}

var controlLayouts = map[TypeID]controlLayout{
	CScNa1: {encode: encodeSingleCommand},
	CDcNa1: {encode: encodeDoubleCommand},
	CRcNa1: {encode: encodeStepCommand},
	CSeNa1: {encode: encodeSetpointNormalised},
	CSeNb1: {encode: encodeSetpointScaled},
	CSeNc1: {encode: encodeSetpointFloat},
	CBoNa1: {encode: encodeBitstringCommand},

	CScTa1: {hasTime: true, encode: encodeSingleCommand},
	CDcTa1: {hasTime: true, encode: encodeDoubleCommand},
	CRcTa1: {hasTime: true, encode: encodeStepCommand},
	CSeTa1: {hasTime: true, encode: encodeSetpointNormalised},
	CSeTb1: {hasTime: true, encode: encodeSetpointScaled},
	CSeTc1: {hasTime: true, encode: encodeSetpointFloat},
	CBoTa1: {hasTime: true, encode: encodeBitstringCommand},
}

// SupportsControlType reports whether this gateway can encode the type.
func SupportsControlType(t TypeID) bool {
	_, ok := controlLayouts[t]
	return ok
}

// CommandRequest is a complete command ASDU: the command and who it is for.
type CommandRequest struct {
	CommonAddr uint16
	Originator uint8

	// Cause is CauseActivation for a command and CauseDeactivation to cancel a
	// selection. Nothing else is accepted: a command sent with, say, a
	// spontaneous cause is a frame the device is entitled to ignore silently.
	Cause Cause

	// Test marks the command as a test that must not drive the output.
	Test bool

	Cmd Command
}

// AppendCommand renders a complete command ASDU onto dst.
//
// Every check happens before the first octet is written, so a rejected command
// leaves dst exactly as it was. That is not tidiness: this package drives
// breakers, and a caller that appends to a shared buffer and then handles the
// error must not be left holding half a command that a later write completes
// into something valid.
//
// A command ASDU always carries exactly one information object, addressed (never
// a sequence). That is encoded here rather than left to the caller, because it
// is a rule with no exceptions and every opportunity to get it wrong is one
// worth removing.
func (p Params) AppendCommand(dst []byte, r CommandRequest) ([]byte, error) {
	l, err := p.validateCommand(r)
	if err != nil {
		return dst, err
	}

	// Build the object into a scratch slice first. Encoding cannot fail after
	// validation, but keeping the write off dst until it is complete means the
	// no-bytes-on-error guarantee does not depend on that staying true.
	obj := make([]byte, 0, 16)
	obj = p.appendIOA(obj, r.Cmd.IOA)
	obj, err = l.encode(obj, r.Cmd)
	if err != nil {
		return dst, err
	}
	if l.hasTime {
		obj = r.Cmd.Time.AppendTo(obj)
	}

	h := Header{
		Type:       r.Cmd.Type,
		Count:      1,
		Cause:      r.Cause,
		Test:       r.Test,
		Originator: r.Originator,
		CommonAddr: r.CommonAddr,
	}
	out, err := h.AppendTo(dst, p)
	if err != nil {
		return dst, err
	}
	return append(out, obj...), nil
}

// validateCommand runs every check that does not depend on writing anything.
func (p Params) validateCommand(r CommandRequest) (controlLayout, error) {
	if err := p.Validate(); err != nil {
		return controlLayout{}, err
	}

	l, ok := controlLayouts[r.Cmd.Type]
	if !ok {
		return controlLayout{}, fmt.Errorf("%w: %s is not a control type this gateway sends",
			ErrCommand, r.Cmd.Type)
	}

	switch {
	case r.Cause != CauseActivation && r.Cause != CauseDeactivation:
		return controlLayout{}, fmt.Errorf("%w: cause %s; a command is %s or %s",
			ErrCommand, r.Cause, CauseActivation, CauseDeactivation)

	// Address 0 means "not used" in 101/104, so a command aimed at it is a
	// configuration mistake the device would answer in its own way.
	case r.Cmd.IOA == 0:
		return controlLayout{}, fmt.Errorf("%w: information object address 0 means \"not used\" and cannot be commanded",
			ErrCommand)
	case r.Cmd.IOA > p.MaxIOA():
		return controlLayout{}, fmt.Errorf("%w: address %d does not fit %d octet(s)",
			ErrIOARange, r.Cmd.IOA, p.IOAOctets)

	case r.Cmd.Duration > MaxOutputDuration:
		return controlLayout{}, fmt.Errorf("%w: qu=%d is reserved; only 0..%d are defined",
			ErrCommand, r.Cmd.Duration, MaxOutputDuration)
	}

	if l.hasTime && !r.Cmd.Time.InRange() {
		return controlLayout{}, fmt.Errorf("%w: %s needs a valid CP56Time2a, got %s",
			ErrCommand, r.Cmd.Type, r.Cmd.Time)
	}
	return l, nil
}

// --- per-type encoders ------------------------------------------------------

func encodeSingleCommand(dst []byte, c Command) ([]byte, error) {
	b := qualifierBits(c)
	if c.On {
		b |= scoStateBit
	}
	return append(dst, b), nil
}

func encodeDoubleCommand(dst []byte, c Command) ([]byte, error) {
	if !c.Double.Permitted() {
		// Two bits, and 0 and 3 mean "indeterminate" — not a position. Sending
		// one to a breaker is undefined behaviour, so it is refused here as
		// well as on the .NET side: two independent refusals for a mistake
		// whose cost is a breaker in an unknown state.
		return dst, fmt.Errorf("%w: double command state %d is not permitted; use %d (off) or %d (on)",
			ErrCommand, uint8(c.Double), uint8(DoubleOff), uint8(DoubleOn))
	}
	return append(dst, qualifierBits(c)|byte(c.Double)&dcoStateMask), nil
}

func encodeStepCommand(dst []byte, c Command) ([]byte, error) {
	if !c.Step.Permitted() {
		return dst, fmt.Errorf("%w: regulating step state %d is not permitted; use %d (lower) or %d (higher)",
			ErrCommand, uint8(c.Step), uint8(StepLower), uint8(StepHigher))
	}
	return append(dst, qualifierBits(c)|byte(c.Step)&rcoStateMask), nil
}

func encodeSetpointNormalised(dst []byte, c Command) ([]byte, error) {
	// A normalised set point is a fraction of full scale. Out of range is
	// refused rather than clamped: clamping turns "set 150%" into "set 100%",
	// which is a different instruction that the operator never gave.
	if c.Value < -1 || c.Value > maxNormalised || math.IsNaN(c.Value) {
		return dst, fmt.Errorf("%w: normalised set point %v is outside -1..%v",
			ErrCommand, c.Value, maxNormalised)
	}
	raw := int16(math.Round(c.Value * normalisedFullScale))
	dst = append(dst, byte(raw), byte(uint16(raw)>>8))
	return append(dst, qosBits(c)), nil
}

func encodeSetpointScaled(dst []byte, c Command) ([]byte, error) {
	dst = append(dst, byte(c.Scaled), byte(uint16(c.Scaled)>>8))
	return append(dst, qosBits(c)), nil
}

func encodeSetpointFloat(dst []byte, c Command) ([]byte, error) {
	if math.IsNaN(c.Value) || math.IsInf(c.Value, 0) {
		return dst, fmt.Errorf("%w: short float set point %v is not a number", ErrCommand, c.Value)
	}
	if math.Abs(c.Value) > math.MaxFloat32 {
		return dst, fmt.Errorf("%w: set point %v does not fit a 32-bit float", ErrCommand, c.Value)
	}
	bits := math.Float32bits(float32(c.Value))
	dst = append(dst, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
	return append(dst, qosBits(c)), nil
}

// encodeBitstringCommand writes a 32-bit pattern. C_BO carries no qualifier at
// all, so there is nowhere to put the select bit: bitstring commands are always
// direct operate, and asking for a selection is refused rather than dropped.
func encodeBitstringCommand(dst []byte, c Command) ([]byte, error) {
	if c.Select {
		return dst, fmt.Errorf("%w: %s has no qualifier octet, so it cannot be selected; use direct operate",
			ErrCommand, c.Type)
	}
	return append(dst, byte(c.Bits), byte(c.Bits>>8), byte(c.Bits>>16), byte(c.Bits>>24)), nil
}

// maxNormalised is the largest normalised value, 32767/32768. The range is not
// symmetric: it reaches -1 exactly but never +1.
const maxNormalised = 32767.0 / normalisedFullScale

// qualifierBits builds the S/E and QU bits shared by SCO, DCO and RCO.
func qualifierBits(c Command) byte {
	// Parentheses matter: << binds tighter than & in Go, so without them this
	// masks with 0x7C and every duration of 0..3 collapses to zero.
	b := (byte(c.Duration) & cmdDurationMask) << cmdDurationShift
	if c.Select {
		b |= cmdSelectBit
	}
	return b
}

// qosBits builds the QOS octet of a set-point command. QL is always zero: the
// standard reserves 1..63, and this gateway does not send reserved values.
func qosBits(c Command) byte {
	var b byte
	if c.Select {
		b |= cmdSelectBit
	}
	return b & (cmdSelectBit | qosQualifierMask)
}

// SelectBit reads the S/E bit out of an encoded control element.
//
// It exists because a select-before-operate dialogue has to tell a selection's
// confirmation from an execution's, and on the wire they are otherwise
// identical: the same type, the same address, the same cause of transmission.
// A master that could not tell them apart would take a repeated select
// confirmation for the execute confirmation and report an operation as done
// when only the selection had been acknowledged — and devices that repeat
// confirmations are not hypothetical.
//
// The qualifier is the last octet of the element, except on the timestamped
// variants where a CP56Time2a follows it. That is layout knowledge, so it lives
// here with the table that defines it rather than being copied into the caller,
// where the two could drift apart.
//
// It reports false for a type with no qualifier octet at all (C_BO_NA_1 and
// C_BO_TA_1 carry a bit pattern and nothing else, which is why they cannot be
// selected) and for an element too short to hold one.
func (t TypeID) SelectBit(data []byte) (selected, ok bool) {
	layout, known := controlLayouts[t]
	if !known {
		return false, false
	}
	if t == CBoNa1 || t == CBoTa1 {
		return false, false
	}

	end := len(data)
	if layout.hasTime {
		end -= CP56Time2aLen
	}
	if end < 1 {
		return false, false
	}
	return data[end-1]&cmdSelectBit != 0, true
}
