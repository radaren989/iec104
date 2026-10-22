package asdu

import (
	"errors"
	"fmt"
)

// ErrSystem means a system-information ASDU cannot be built or understood.
var ErrSystem = errors.New("invalid system ASDU")

// systemIOA is the address every station-addressed message carries.
//
// These messages address the station, not a point, and 101/104 spell that as
// address 0 — the same value that means "not used" for a real point. A station
// message with any other address is a device doing something the standard does
// not describe, and guessing what it meant is worse than saying so.
const systemIOA IOA = 0

// --- C_IC_NA_1: general interrogation ---------------------------------------

// QOI is the qualifier of interrogation: which slice of the station's data is
// being asked for.
type QOI uint8

const (
	// QOIStation (20) asks for everything. This is the one that fills the cache
	// on connect, and until its termination arrives the cache is incomplete.
	QOIStation QOI = 20

	// QOIGroup1 (21) through group 16 (36) ask for one configured group.
	QOIGroup1 QOI = 21

	// QOIGroup16 is the last group qualifier.
	QOIGroup16 QOI = 36
)

// Group returns the qualifier for interrogation group n, 1..16.
func Group(n int) (QOI, error) {
	if n < 1 || n > 16 {
		return 0, fmt.Errorf("%w: interrogation group %d is outside 1..16", ErrSystem, n)
	}
	return QOIGroup1 + QOI(n-1), nil
}

// Valid reports whether the qualifier is one the standard defines for an
// interrogation: the station request or one of the sixteen groups.
func (q QOI) Valid() bool {
	return q == QOIStation || (q >= QOIGroup1 && q <= QOIGroup16)
}

func (q QOI) String() string {
	switch {
	case q == QOIStation:
		return "station"
	case q >= QOIGroup1 && q <= QOIGroup16:
		return fmt.Sprintf("group-%d", int(q-QOIGroup1)+1)
	default:
		return fmt.Sprintf("QOI(%d)", uint8(q))
	}
}

// AppendInterrogation writes a C_IC_NA_1 with the given cause.
//
// The full sequence this is one third of:
//
//	master -> station  COT=6  activation        "give me everything"
//	station -> master  COT=7  activation con    "starting"
//	station -> master  COT=20 ... every point
//	station -> master  COT=10 activation term   "snapshot complete"
//
// That con .. term bracket is the cold-start gate: until the termination
// arrives the cache is incomplete, so reads out of it are not topical. Treating
// the confirmation as the finish line is the mistake that makes a gateway serve
// a half-built snapshot as if it were current.
func (p Params) AppendInterrogation(dst []byte, commonAddr uint16, originator uint8, cause Cause, qoi QOI) ([]byte, error) {
	if !qoi.Valid() {
		return dst, fmt.Errorf("%w: qoi=%d is neither the station request (%d) nor a group (%d..%d)",
			ErrSystem, uint8(qoi), uint8(QOIStation), uint8(QOIGroup1), uint8(QOIGroup16))
	}
	return p.appendSystemASDU(dst, CIcNa1, commonAddr, originator, cause, []byte{byte(qoi)})
}

// ParseInterrogation reads the qualifier out of a C_IC_NA_1. The station echoes
// it in both the confirmation and the termination, which is how a master with
// several interrogations in flight tells their answers apart.
func (p Params) ParseInterrogation(h Header, body []byte) (QOI, error) {
	elem, err := p.parseSystemElement(h, CIcNa1, body, 1)
	if err != nil {
		return 0, err
	}
	qoi := QOI(elem[0])
	if !qoi.Valid() {
		return qoi, fmt.Errorf("%w: station answered with qoi=%d", ErrSystem, uint8(qoi))
	}
	return qoi, nil
}

// --- C_CI_NA_1: counter interrogation ---------------------------------------

// FreezeAction is the FRZ field of a counter interrogation: what to do with the
// counters before reading them.
type FreezeAction uint8

const (
	// FreezeRead reads the counters as they are, without freezing.
	FreezeRead FreezeAction = 0

	// FreezeNoReset copies the counters into the frozen registers and leaves
	// the running values alone. This is the one an energy meter wants: each
	// freeze is an interval boundary and the totals keep climbing.
	FreezeNoReset FreezeAction = 1

	// FreezeWithReset freezes and then zeroes the running counters.
	FreezeWithReset FreezeAction = 2

	// FreezeReset zeroes without freezing, which discards the interval.
	FreezeReset FreezeAction = 3
)

// CounterRequest is the RQT field: which counter group to read.
type CounterRequest uint8

const (
	CounterGroup1  CounterRequest = 1
	CounterGroup4  CounterRequest = 4
	CounterGeneral CounterRequest = 5 // every counter
	maxCounterRQT                 = CounterGeneral
)

const (
	qccRequestMask = 0x3F
	qccFreezeShift = 6
)

// AppendCounterInterrogation writes a C_CI_NA_1.
func (p Params) AppendCounterInterrogation(dst []byte, commonAddr uint16, originator uint8, cause Cause, req CounterRequest, frz FreezeAction) ([]byte, error) {
	if req < CounterGroup1 || req > maxCounterRQT {
		return dst, fmt.Errorf("%w: counter request %d is outside 1..%d (groups 1..4, or %d for all)",
			ErrSystem, uint8(req), uint8(maxCounterRQT), uint8(CounterGeneral))
	}
	if frz > FreezeReset {
		return dst, fmt.Errorf("%w: freeze action %d does not fit two bits", ErrSystem, uint8(frz))
	}
	qcc := byte(req)&qccRequestMask | byte(frz)<<qccFreezeShift
	return p.appendSystemASDU(dst, CCiNa1, commonAddr, originator, cause, []byte{qcc})
}

// ParseCounterInterrogation reads the qualifier the station echoed back.
func (p Params) ParseCounterInterrogation(h Header, body []byte) (CounterRequest, FreezeAction, error) {
	elem, err := p.parseSystemElement(h, CCiNa1, body, 1)
	if err != nil {
		return 0, 0, err
	}
	return CounterRequest(elem[0] & qccRequestMask), FreezeAction(elem[0] >> qccFreezeShift), nil
}

// --- C_CS_NA_1: clock synchronisation ---------------------------------------

// AppendClockSync writes a C_CS_NA_1 carrying the instant to set.
//
// Sending this is opt-in per station and off by default. Two masters setting one
// RTU's clock is worse than none: the timestamps on that station's events then
// step back and forth, and the event ordering people reason with stops being an
// ordering at all.
func (p Params) AppendClockSync(dst []byte, commonAddr uint16, originator uint8, cause Cause, at CP56Time2a) ([]byte, error) {
	if !at.InRange() {
		return dst, fmt.Errorf("%w: clock synchronisation needs a valid CP56Time2a, got %s", ErrSystem, at)
	}
	if at.Invalid {
		// Setting a clock to a time we have ourselves marked invalid is not a
		// synchronisation, it is corruption with extra steps.
		return dst, fmt.Errorf("%w: refusing to synchronise a clock to a timestamp marked invalid", ErrSystem)
	}
	return p.appendSystemASDU(dst, CCsNa1, commonAddr, originator, cause, at.AppendTo(nil))
}

// ParseClockSync reads the timestamp out of a C_CS_NA_1.
func (p Params) ParseClockSync(h Header, body []byte) (CP56Time2a, error) {
	elem, err := p.parseSystemElement(h, CCsNa1, body, CP56Time2aLen)
	if err != nil {
		return CP56Time2a{}, err
	}
	return ParseCP56Time2a(elem)
}

// --- C_RP_NA_1: reset process -----------------------------------------------

// ResetQualifier is QRP: what a reset process command resets.
type ResetQualifier uint8

const (
	// ResetGeneral (1) restarts the station's process image.
	ResetGeneral ResetQualifier = 1

	// ResetPendingEvents (2) discards buffered time-tagged information that has
	// not been delivered yet. It throws away events, so it is the one to be
	// careful with: anything the station was holding for us is gone.
	ResetPendingEvents ResetQualifier = 2
)

// AppendResetProcess writes a C_RP_NA_1.
func (p Params) AppendResetProcess(dst []byte, commonAddr uint16, originator uint8, cause Cause, qrp ResetQualifier) ([]byte, error) {
	if qrp != ResetGeneral && qrp != ResetPendingEvents {
		return dst, fmt.Errorf("%w: reset qualifier %d is not defined; use %d or %d",
			ErrSystem, uint8(qrp), uint8(ResetGeneral), uint8(ResetPendingEvents))
	}
	return p.appendSystemASDU(dst, CRpNa1, commonAddr, originator, cause, []byte{byte(qrp)})
}

// --- M_EI_NA_1: end of initialisation ---------------------------------------

// InitCause is the COI field: why the station initialised.
type InitCause uint8

const (
	InitLocalPowerOn  InitCause = 0
	InitLocalManual   InitCause = 1
	InitRemoteReset   InitCause = 2
	maxKnownInitCause           = InitRemoteReset
)

func (c InitCause) String() string {
	switch c {
	case InitLocalPowerOn:
		return "local-power-on"
	case InitLocalManual:
		return "local-manual-reset"
	case InitRemoteReset:
		return "remote-reset"
	default:
		return fmt.Sprintf("InitCause(%d)", uint8(c))
	}
}

const coiParameterChangeBit = 0x80

// EndOfInit is an M_EI_NA_1: the station announcing that it has finished
// starting up.
//
// EVERY one of these means the cached snapshot is void and a general
// interrogation must be run again. That holds for all three causes — the station
// restarted, so its process image was rebuilt — and it holds for the parameter
// change flag too, because the point list itself may have changed. There is
// deliberately no RequiresInterrogation method returning a constant true: the
// rule is unconditional, and a method would only suggest that somewhere it is
// not. The station layer re-interrogates on receipt; see PROJECT_STRUCTURE.
type EndOfInit struct {
	Cause InitCause

	// AfterParameterChange is the BS bit: the initialisation followed a change
	// to the station's local parameters rather than a plain restart. Worth
	// logging differently — the point list may have moved under us — but it
	// changes nothing about the need to re-interrogate.
	AfterParameterChange bool
}

func (e EndOfInit) String() string {
	if e.AfterParameterChange {
		return e.Cause.String() + " (after parameter change)"
	}
	return e.Cause.String()
}

// ParseEndOfInit reads an M_EI_NA_1.
//
// An unrecognised cause is not an error: the field is seven bits and a vendor
// may use a value this table does not list. What matters is that the station
// initialised, and that is true whatever the reason.
func (p Params) ParseEndOfInit(h Header, body []byte) (EndOfInit, error) {
	elem, err := p.parseSystemElement(h, MEiNa1, body, 1)
	if err != nil {
		return EndOfInit{}, err
	}
	return EndOfInit{
		Cause:                InitCause(elem[0] &^ coiParameterChangeBit),
		AfterParameterChange: elem[0]&coiParameterChangeBit != 0,
	}, nil
}

// --- shared plumbing --------------------------------------------------------

// appendSystemASDU writes a station-addressed ASDU: one object at address 0,
// never a sequence.
func (p Params) appendSystemASDU(dst []byte, typ TypeID, commonAddr uint16, originator uint8, cause Cause, elem []byte) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return dst, err
	}

	// Build the object first so that a failure part-way leaves dst untouched,
	// the same rule the command path follows.
	obj := p.appendIOA(nil, systemIOA)
	obj = append(obj, elem...)

	h := Header{Type: typ, Count: 1, Cause: cause, Originator: originator, CommonAddr: commonAddr}
	out, err := h.AppendTo(dst, p)
	if err != nil {
		return dst, err
	}
	return append(out, obj...), nil
}

// parseSystemElement checks the shape every station-addressed ASDU must have and
// returns its element octets.
func (p Params) parseSystemElement(h Header, want TypeID, body []byte, elemLen int) ([]byte, error) {
	if h.Type != want {
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrSystem, want, h.Type)
	}
	if h.Count != 1 || h.Sequence {
		return nil, fmt.Errorf("%w: %s carries exactly one object, got count=%d sq=%v",
			ErrSystem, want, h.Count, h.Sequence)
	}

	elems, err := p.SplitElements(h, body, elemLen)
	if err != nil {
		return nil, err
	}
	if elems[0].IOA != systemIOA {
		// A station message at a point address is a device doing something the
		// standard does not describe. Guessing what it meant is worse than
		// saying so.
		return nil, fmt.Errorf("%w: %s addresses the station, so its IOA must be %d, got %d",
			ErrSystem, want, systemIOA, elems[0].IOA)
	}
	return elems[0].Data, nil
}
