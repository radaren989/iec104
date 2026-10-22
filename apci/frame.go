package apci

import "fmt"

// Frame is one decoded APDU.
//
// Which fields carry meaning depends on Format:
//
//	FormatI: SendSeq, RecvSeq, ASDU
//	FormatS: RecvSeq
//	FormatU: Function
//
// Fields outside that set are zero after Decode and are ignored by Encode, so a
// Frame never half-describes two formats at once.
type Frame struct {
	Format Format

	// SendSeq is N(S): how many I-frames this side has sent, mod 32768.
	// I-format only.
	SendSeq uint16

	// RecvSeq is N(R): the sequence number this side next expects, i.e. an
	// acknowledgement of everything below it. I- and S-format.
	RecvSeq uint16

	// Function is the U-format control function. U-format only.
	Function UFunction

	// ASDU is the application payload, I-format only.
	//
	// Decode COPIES it out of the caller's buffer. That costs one small
	// allocation per received frame and buys the guarantee that a Frame stays
	// valid after its read buffer is reused — which is exactly the kind of
	// aliasing bug that shows up as one station reporting another's values,
	// months later, under load. If profiling ever says this matters, the fix is
	// a separate decode-into-buffer entry point, not making this one share
	// memory.
	ASDU []byte
}

func (f Frame) String() string {
	switch f.Format {
	case FormatI:
		return fmt.Sprintf("I(ns=%d nr=%d asdu=%dB)", f.SendSeq, f.RecvSeq, len(f.ASDU))
	case FormatS:
		return fmt.Sprintf("S(nr=%d)", f.RecvSeq)
	case FormatU:
		return fmt.Sprintf("U(%s)", f.Function)
	default:
		return "invalid frame"
	}
}

// NewI builds an I-format frame.
func NewI(sendSeq, recvSeq uint16, asdu []byte) Frame {
	return Frame{Format: FormatI, SendSeq: sendSeq, RecvSeq: recvSeq, ASDU: asdu}
}

// NewS builds an S-format acknowledgement.
func NewS(recvSeq uint16) Frame {
	return Frame{Format: FormatS, RecvSeq: recvSeq}
}

// NewU builds a U-format control frame.
func NewU(fn UFunction) Frame {
	return Frame{Format: FormatU, Function: fn}
}

// NextFrameLen reports the total length, start byte included, of the frame at
// the head of b.
//
// It is the "how much more do I need" half of reading a stream: call it, and if
// it returns ErrIncomplete read more bytes and call it again. Any other error is
// a protocol violation and the connection is finished — there is no recovery
// path, because 0x68 occurs inside ASDU payloads and scanning for the next one
// would lock onto a false boundary and deliver plausible nonsense.
func NextFrameLen(b []byte) (int, error) {
	if len(b) < HeaderLen {
		return 0, ErrIncomplete
	}
	if b[0] != StartByte {
		return 0, fmt.Errorf("%w: got 0x%02X, want 0x%02X", ErrStartByte, b[0], StartByte)
	}
	apduLen := int(b[1])
	if apduLen < MinAPDULen || apduLen > MaxAPDULen {
		return 0, fmt.Errorf("%w: %d is outside %d..%d", ErrLength, apduLen, MinAPDULen, MaxAPDULen)
	}
	return HeaderLen + apduLen, nil
}

// Decode parses exactly one complete frame, start byte included.
//
// b must hold the whole frame and nothing after it; use NextFrameLen to carve
// it out of a stream. Trailing bytes are an error rather than silently ignored,
// because silently ignoring them is how a length-field bug survives testing.
func Decode(b []byte) (Frame, error) {
	total, err := NextFrameLen(b)
	if err != nil {
		return Frame{}, err
	}
	if len(b) < total {
		return Frame{}, ErrIncomplete
	}
	if len(b) > total {
		return Frame{}, fmt.Errorf("%w: %d trailing byte(s) after a %d-byte frame",
			ErrLength, len(b)-total, total)
	}

	ctrl := b[HeaderLen : HeaderLen+ControlLen]
	payload := b[HeaderLen+ControlLen : total]

	switch {
	case ctrl[0]&0x01 == 0: // I-format: low bit clear
		if len(payload) == 0 {
			return Frame{}, ErrEmptyASDU
		}
		sendSeq, err := decodeSeq(ctrl[0], ctrl[1], "N(S)")
		if err != nil {
			return Frame{}, err
		}
		recvSeq, err := decodeSeq(ctrl[2], ctrl[3], "N(R)")
		if err != nil {
			return Frame{}, err
		}
		// Copy: see the note on Frame.ASDU.
		asdu := make([]byte, len(payload))
		copy(asdu, payload)
		return Frame{Format: FormatI, SendSeq: sendSeq, RecvSeq: recvSeq, ASDU: asdu}, nil

	case ctrl[0]&uFormatMarker == uFormatMarker: // U-format: low two bits set
		if len(payload) != 0 {
			return Frame{}, fmt.Errorf("%w: U-format frame carries %d payload byte(s)", ErrFormat, len(payload))
		}
		fn := UFunction(ctrl[0] &^ uFormatMarker)
		if !fn.Valid() {
			return Frame{}, fmt.Errorf("%w: ctrl0 0x%02X is not exactly one U function", ErrFormat, ctrl[0])
		}
		if ctrl[1]|ctrl[2]|ctrl[3] != 0 {
			return Frame{}, fmt.Errorf("%w: U-format octets 2-4 must be zero, got %02X %02X %02X",
				ErrFormat, ctrl[1], ctrl[2], ctrl[3])
		}
		return Frame{Format: FormatU, Function: fn}, nil

	default: // S-format: ctrl0 == 0x01
		if ctrl[0] != sFormatCtrl0 {
			return Frame{}, fmt.Errorf("%w: ctrl0 0x%02X is neither I, S nor U", ErrFormat, ctrl[0])
		}
		if len(payload) != 0 {
			return Frame{}, fmt.Errorf("%w: S-format frame carries %d payload byte(s)", ErrFormat, len(payload))
		}
		if ctrl[1] != 0 {
			return Frame{}, fmt.Errorf("%w: S-format octet 2 must be zero, got 0x%02X", ErrFormat, ctrl[1])
		}
		recvSeq, err := decodeSeq(ctrl[2], ctrl[3], "N(R)")
		if err != nil {
			return Frame{}, err
		}
		return Frame{Format: FormatS, RecvSeq: recvSeq}, nil
	}
}

// decodeSeq reads a 15-bit sequence number from a little-endian octet pair whose
// low bit is reserved and must be zero.
//
// The reserved bit is checked rather than masked away. Masking would accept a
// frame the peer built wrong and hide the disagreement until the sequence
// numbers drifted apart — which surfaces as a connection that dies for no
// visible reason after hours of correct operation.
func decodeSeq(lo, hi byte, name string) (uint16, error) {
	if lo&0x01 != 0 {
		return 0, fmt.Errorf("%w: reserved bit set in %s low octet (0x%02X)", ErrFormat, name, lo)
	}
	return uint16(lo)>>1 | uint16(hi)<<7, nil
}

// encodeSeq is the inverse of decodeSeq.
func encodeSeq(seq uint16) (lo, hi byte) {
	return byte(seq<<1) & 0xFE, byte(seq >> 7)
}

// Encode renders the frame into a new slice.
func (f Frame) Encode() ([]byte, error) {
	return f.AppendTo(nil)
}

// AppendTo renders the frame onto dst and returns the extended slice, in the
// manner of append. The write loop uses it with a reusable buffer so that
// sending a frame does not allocate.
func (f Frame) AppendTo(dst []byte) ([]byte, error) {
	var ctrl [ControlLen]byte
	var payload []byte

	switch f.Format {
	case FormatI:
		if f.SendSeq > MaxSeq {
			return dst, fmt.Errorf("%w: N(S)=%d exceeds %d", ErrSeqRange, f.SendSeq, MaxSeq)
		}
		if f.RecvSeq > MaxSeq {
			return dst, fmt.Errorf("%w: N(R)=%d exceeds %d", ErrSeqRange, f.RecvSeq, MaxSeq)
		}
		if len(f.ASDU) == 0 {
			return dst, ErrEmptyASDU
		}
		if len(f.ASDU) > MaxASDULen {
			return dst, fmt.Errorf("%w: %d bytes, limit is %d", ErrASDUTooLong, len(f.ASDU), MaxASDULen)
		}
		ctrl[0], ctrl[1] = encodeSeq(f.SendSeq)
		ctrl[2], ctrl[3] = encodeSeq(f.RecvSeq)
		payload = f.ASDU

	case FormatS:
		if f.RecvSeq > MaxSeq {
			return dst, fmt.Errorf("%w: N(R)=%d exceeds %d", ErrSeqRange, f.RecvSeq, MaxSeq)
		}
		ctrl[0] = sFormatCtrl0
		ctrl[2], ctrl[3] = encodeSeq(f.RecvSeq)

	case FormatU:
		if !f.Function.Valid() {
			return dst, fmt.Errorf("%w: 0x%02X is not a U function", ErrFormat, byte(f.Function))
		}
		ctrl[0] = byte(f.Function) | uFormatMarker

	default:
		return dst, fmt.Errorf("%w: unknown format %d", ErrFormat, f.Format)
	}

	dst = append(dst, StartByte, byte(ControlLen+len(payload)))
	dst = append(dst, ctrl[:]...)
	dst = append(dst, payload...)
	return dst, nil
}
