// Package apci implements the CS 104 APCI layer: the framing, sequence numbers,
// flow-control windows and timer logic that sit between a TCP stream and an
// ASDU.
//
// The package is pure: no network, no Redis, no gRPC, no
// logging, no metrics, no context. Input is a byte slice, output is a value.
// That is not a style preference — it is the only thing that makes byte-level
// golden tests possible, and byte-level golden tests are the only trustworthy
// proof that a protocol implementation is right. A layer that reaches for a
// socket cannot be replayed against a capture.
//
// Everything here is IEC 60870-5-104 (CS 104) specific. Nothing in this package
// knows what an ASDU means; it only knows how long one is and which frame
// carried it.
package apci

import "errors"

// Frame geometry. Every APDU on the wire is:
//
//	0x68 | length | ctrl0 | ctrl1 | ctrl2 | ctrl3 | [ASDU...]
//	     \________ the length field counts from here _________/
//
// The length field therefore counts the four control octets plus the ASDU, and
// does NOT count itself or the start byte. Getting that off by two is the
// classic first bug in a 104 implementation: the connection looks alive, and
// every frame after the first is misaligned.
const (
	// StartByte opens every APDU.
	StartByte = 0x68

	// HeaderLen is the start byte plus the length byte.
	HeaderLen = 2

	// ControlLen is the fixed four-octet control field.
	ControlLen = 4

	// MinAPDULen is the smallest legal length field: control field only, no
	// ASDU. S- and U-format frames are always exactly this.
	MinAPDULen = ControlLen

	// MaxAPDULen is the largest legal length field. The field is one octet, but
	// the standard caps it at 253 rather than 255.
	MaxAPDULen = 253

	// MaxASDULen is how much application data one frame can carry.
	MaxASDULen = MaxAPDULen - ControlLen // 249

	// MaxFrameLen is the largest complete frame, start byte included.
	MaxFrameLen = HeaderLen + MaxAPDULen // 255

	// MaxSeq is the largest sequence number. N(S) and N(R) are 15-bit counters
	// that wrap at 32768 — see the sequence-number logic for why that matters.
	MaxSeq = 32767
)

// Format is the APCI frame format, decided by the low bits of the first control
// octet.
type Format uint8

const (
	// FormatI carries an ASDU and is numbered in both directions: it has an
	// N(S) and an N(R). Low bit of ctrl0 is 0.
	FormatI Format = iota

	// FormatS acknowledges received I-frames. It carries an N(R) and nothing
	// else. ctrl0 is exactly 0x01.
	FormatS

	// FormatU is unnumbered control: STARTDT, STOPDT, TESTFR. Low two bits of
	// ctrl0 are both 1.
	FormatU
)

func (f Format) String() string {
	switch f {
	case FormatI:
		return "I"
	case FormatS:
		return "S"
	case FormatU:
		return "U"
	default:
		return "invalid"
	}
}

// UFunction is the single function bit of a U-format frame. The value is the
// bit as it sits in ctrl0, so encoding is `byte(fn) | uFormatMarker`.
//
// Exactly one of these bits may be set. A frame with two of them, or none, is
// not "a U-frame we do not recognise" — it is malformed, and is rejected.
type UFunction byte

const (
	// StartDTAct asks the remote station to begin data transfer. Until the
	// controlled station has answered StartDTCon it sends no I-frames at all,
	// which is why a connection that skips this handshake looks established and
	// delivers nothing.
	StartDTAct UFunction = 0x04
	StartDTCon UFunction = 0x08

	// StopDTAct suspends data transfer without closing TCP.
	StopDTAct UFunction = 0x10
	StopDTCon UFunction = 0x20

	// TestFRAct probes an idle link; it is what timer t3 fires.
	TestFRAct UFunction = 0x40
	TestFRCon UFunction = 0x80
)

// uFormatMarker is the low two bits that mark ctrl0 as U-format.
const uFormatMarker = 0x03

// sFormatCtrl0 is the whole of ctrl0 for an S-format frame.
const sFormatCtrl0 = 0x01

func (fn UFunction) String() string {
	switch fn {
	case StartDTAct:
		return "STARTDT act"
	case StartDTCon:
		return "STARTDT con"
	case StopDTAct:
		return "STOPDT act"
	case StopDTCon:
		return "STOPDT con"
	case TestFRAct:
		return "TESTFR act"
	case TestFRCon:
		return "TESTFR con"
	default:
		return "invalid U function"
	}
}

// Valid reports whether exactly one known function bit is set.
func (fn UFunction) Valid() bool {
	switch fn {
	case StartDTAct, StartDTCon, StopDTAct, StopDTCon, TestFRAct, TestFRCon:
		return true
	default:
		return false
	}
}

// IsAct reports whether this is an activation rather than a confirmation.
func (fn UFunction) IsAct() bool {
	return fn == StartDTAct || fn == StopDTAct || fn == TestFRAct
}

// Confirm returns the confirmation that answers an activation. The second
// result is false for a function that is already a confirmation.
func (fn UFunction) Confirm() (UFunction, bool) {
	switch fn {
	case StartDTAct:
		return StartDTCon, true
	case StopDTAct:
		return StopDTCon, true
	case TestFRAct:
		return TestFRCon, true
	default:
		return 0, false
	}
}

// Decoding errors. They are sentinels because the connection layer reacts
// differently to "I need more bytes" and "this peer is speaking nonsense".
var (
	// ErrIncomplete means the buffer does not yet hold a whole frame. It is the
	// only error here that is not a protocol violation: the caller reads more
	// and tries again.
	ErrIncomplete = errors.New("incomplete frame")

	// ErrStartByte means the stream is not aligned on 0x68. There is no
	// resynchronisation: 0x68 appears inside ASDU payloads all the time, so
	// hunting for the next one would silently lock onto a false frame boundary.
	// The connection is closed instead.
	ErrStartByte = errors.New("bad start byte")

	// ErrLength means the length octet is outside 4..253.
	ErrLength = errors.New("bad length field")

	// ErrFormat means the control field does not describe a legal frame:
	// an unknown or ambiguous U function, a reserved bit set, or an S/U frame
	// carrying a payload.
	ErrFormat = errors.New("bad control field")

	// ErrSeqRange means a sequence number is above MaxSeq. It cannot happen on
	// decode (15 bits cannot exceed 32767) but it can happen on encode, where it
	// means the caller's counter arithmetic is wrong.
	ErrSeqRange = errors.New("sequence number out of range")

	// ErrASDUTooLong means an I-frame's payload exceeds MaxASDULen.
	ErrASDUTooLong = errors.New("ASDU longer than a frame can carry")

	// ErrEmptyASDU means an I-frame has no payload. An I-frame exists to carry
	// an ASDU; one without is malformed, not an empty delivery.
	ErrEmptyASDU = errors.New("no ASDU in an I-frame")
)
