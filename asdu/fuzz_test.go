package asdu

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

// fuzzParams turns three arbitrary octets into a valid width configuration, so
// the fuzzer spends its time on the payload rather than on parameter
// combinations that Validate rejects in one line.
func fuzzParams(casdu, cot, ioa uint8) Params {
	return Params{
		CASDUOctets: 1 + casdu%2,
		COTOctets:   1 + cot%2,
		IOAOctets:   1 + ioa%3,
	}
}

// seedASDUs are the shapes worth starting from: one of each element width, both
// encodings, a time tag, a system message, and a few that are wrong in
// interesting ways.
var seedASDUs = []string{
	"01 01 03 00 01 00 64 00 00 01",                         // M_SP_NA_1
	"03 01 03 00 01 00 64 00 00 03",                         // M_DP_NA_1, indeterminate
	"0B 01 14 00 01 00 64 00 00 E8 03 00",                   // M_ME_NB_1, interrogated
	"0D 01 03 00 01 00 64 00 00 00 00 80 3F 00",             // M_ME_NC_1
	"0F 01 25 00 01 00 64 00 00 E8 03 00 00 05",             // M_IT_NA_1
	"1E 01 03 00 01 00 64 00 00 01 6E B2 1E 0E 4F 09 1A",    // M_SP_TB_1
	"0B 83 14 00 01 00 64 00 00 0A 00 00 14 00 00 1E 00 00", // sequence of three
	"64 01 07 00 01 00 00 00 00 14",                         // C_IC_NA_1 actcon
	"46 01 04 00 01 00 00 00 00 00",                         // M_EI_NA_1
	"01 00 03 00 01 00",                                     // zero objects
	"02 01 03 00 01 00 64 00 00 01",                         // a forbidden CP24 type
	"C8 01 03 00 01 00 64 00 00 01",                         // an unknown type
	"",
	"01",
}

// FuzzParseMonitor throws arbitrary bytes at the monitor path.
//
// Field input is untrusted. An RTU with a firmware bug, a
// half-configured device or a truncated frame must not be able to panic this
// process — a panic in the read loop takes down every other station on the
// gateway with it.
func FuzzParseMonitor(f *testing.F) {
	for _, seed := range seedASDUs {
		f.Add(uint8(2), uint8(2), uint8(3), mustHexBytes(f, seed))
	}

	f.Fuzz(func(t *testing.T, casdu, cot, ioa uint8, b []byte) {
		p := fuzzParams(casdu, cot, ioa)

		h, body, err := p.ParseHeader(b)
		if err != nil {
			return
		}

		points, err := p.ParseMonitor(h, body)
		if err != nil {
			// The three refusals are all legitimate; nothing else should be
			// returned from this path, and a new error kind showing up here is
			// worth noticing rather than tolerating.
			switch {
			case errors.Is(err, ErrUnsupportedType),
				errors.Is(err, ErrForbiddenType),
				errors.Is(err, ErrLength),
				errors.Is(err, ErrIOARange),
				errors.Is(err, ErrTruncated),
				errors.Is(err, ErrObjectCount),
				errors.Is(err, ErrParams):
				return
			default:
				t.Fatalf("unexpected error kind from ParseMonitor: %v", err)
			}
		}

		// Whatever came back must be internally consistent. These are the
		// invariants everything above this package relies on without checking.
		if len(points) != int(h.Count) {
			t.Fatalf("%d points for a header promising %d", len(points), h.Count)
		}
		for _, pt := range points {
			if pt.Type != h.Type {
				t.Errorf("point type %s does not match the header's %s", pt.Type, h.Type)
			}
			if pt.Cause != h.Cause {
				t.Errorf("point cause %s does not match the header's %s", pt.Cause, h.Cause)
			}
			if pt.IOA > p.MaxIOA() {
				t.Errorf("IOA %d exceeds the %d-octet maximum %d", pt.IOA, p.IOAOctets, p.MaxIOA())
			}
			switch pt.Kind {
			case KindBool, KindFloat, KindInt:
			default:
				t.Errorf("point has no valid kind: %d", pt.Kind)
			}
			// A float value is allowed to be NaN — a device may genuinely send
			// one — but it must not be a value we invented.
			if pt.Kind != KindFloat && (pt.Float != 0 || math.IsNaN(pt.Float)) {
				t.Errorf("non-float point carries Float=%v", pt.Float)
			}
			if pt.HasTime && pt.Time == (CP56Time2a{}) && pt.Time.InRange() {
				t.Error("a timestamped point reports a zero tag as in range")
			}
			if !pt.HasTime && pt.Time != (CP56Time2a{}) {
				t.Errorf("an untimestamped point carries a tag: %s", pt.Time)
			}
		}
	})
}

// FuzzHeaderRoundTrip checks the property that matters most for the header: a
// header that parses must re-encode to exactly the octets it was read from.
//
// A parser that accepts a header it cannot reproduce has silently reinterpreted
// something — which is how field-width misconfiguration turns into plausible
// values at the wrong addresses instead of an error.
func FuzzHeaderRoundTrip(f *testing.F) {
	for _, seed := range seedASDUs {
		f.Add(uint8(2), uint8(2), uint8(3), mustHexBytes(f, seed))
	}

	f.Fuzz(func(t *testing.T, casdu, cot, ioa uint8, b []byte) {
		p := fuzzParams(casdu, cot, ioa)

		h, body, err := p.ParseHeader(b)
		if err != nil {
			return
		}

		back, err := h.AppendTo(nil, p)
		if err != nil {
			t.Fatalf("a header that parsed must encode: %v (%+v)", err, h)
		}
		want := b[:p.HeaderLen()]
		if !bytes.Equal(back, want) {
			t.Fatalf("round trip changed the header\n got % X\nwant % X", back, want)
		}
		if len(body) != len(b)-p.HeaderLen() {
			t.Fatalf("body is %d octet(s), expected %d", len(body), len(b)-p.HeaderLen())
		}
	})
}

// FuzzCP56Time2a checks that a tag re-encodes to its own octets once the
// reserved bits are cleared, and that an out-of-range tag never yields an
// instant.
func FuzzCP56Time2a(f *testing.F) {
	for _, seed := range []string{
		"6E B2 1E 0E 4F 09 1A",
		"6E B2 9E 8E 4F 09 1A",
		"00 00 00 00 00 00 00",
		"FF FF FF FF FF FF FF",
	} {
		f.Add(mustHexBytes(f, seed))
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		tag, err := ParseCP56Time2a(b)
		if err != nil {
			return
		}

		// Reserved bits are dropped on read, so compare against the masked
		// input rather than the raw one.
		want := []byte{
			b[0], b[1],
			b[2] & (cp56MinuteMask | cp56InvalidBit),
			b[3] & (cp56HourMask | cp56SummerBit),
			b[4],
			b[5] & cp56MonthMask,
			b[6] & cp56YearMask,
		}
		if back := tag.AppendTo(nil); !bytes.Equal(back, want) {
			t.Fatalf("round trip\n got % X\nwant % X (from % X)", back, want, b[:CP56Time2aLen])
		}

		if !tag.InRange() {
			if _, ok := tag.Time(nil); ok {
				t.Fatal("an out-of-range tag must not yield an instant")
			}
		}
		if tag.Usable() && (tag.Invalid || !tag.InRange()) {
			t.Fatal("Usable disagrees with Invalid and InRange")
		}
	})
}

// FuzzAppendCommand is the one that matters for safety: whatever arbitrary
// combination of fields arrives, AppendCommand either refuses and writes
// NOTHING, or produces an ASDU that parses back as exactly one addressed object
// of the right type.
//
// The no-bytes-on-error half is the point. This package drives breakers, and a
// caller that appends to a shared buffer and then handles the error must not be
// left holding half a command that a later write completes into something valid.
func FuzzAppendCommand(f *testing.F) {
	f.Add(uint8(2), uint8(2), uint8(3), uint8(45), uint32(100), false, uint8(0), uint8(1), 0.5)
	f.Add(uint8(2), uint8(2), uint8(3), uint8(46), uint32(1), true, uint8(1), uint8(0), 0.0)
	f.Add(uint8(1), uint8(1), uint8(1), uint8(50), uint32(7), false, uint8(3), uint8(2), -1.0)
	f.Add(uint8(2), uint8(2), uint8(3), uint8(58), uint32(5), true, uint8(2), uint8(3), 1e40)

	f.Fuzz(func(t *testing.T, casdu, cot, ioa, typ uint8, addr uint32, sel bool, dur, state uint8, value float64) {
		p := fuzzParams(casdu, cot, ioa)

		cmd := Command{
			Type:     TypeID(typ),
			IOA:      IOA(addr),
			Select:   sel,
			Duration: OutputDuration(dur),
			On:       state&1 != 0,
			Double:   DoubleCommand(state & 0x03),
			Step:     StepCommand(state & 0x03),
			Value:    value,
			Scaled:   int16(state),
			Bits:     addr,
			Time:     CP56Time2a{Millisecond: 0, Minute: 0, Hour: 0, Day: 1, Month: 1, Year: 26},
		}
		req := CommandRequest{CommonAddr: 1, Cause: CauseActivation, Cmd: cmd}

		// Start from a non-empty buffer so that "wrote nothing" is a real claim.
		prefix := []byte{0xDE, 0xAD}
		out, err := p.AppendCommand(append([]byte(nil), prefix...), req)

		if err != nil {
			if !bytes.Equal(out, prefix) {
				t.Fatalf("a refused command wrote % X", out[len(prefix):])
			}
			return
		}

		if !bytes.HasPrefix(out, prefix) {
			t.Fatalf("AppendCommand disturbed the existing content: % X", out)
		}
		asdu := out[len(prefix):]

		h, body, err := p.ParseHeader(asdu)
		if err != nil {
			t.Fatalf("an encoded command must parse: %v (% X)", err, asdu)
		}
		if h.Type != cmd.Type {
			t.Errorf("type = %s, want %s", h.Type, cmd.Type)
		}
		if h.Count != 1 || h.Sequence {
			t.Errorf("count=%d sq=%v, want exactly one addressed object", h.Count, h.Sequence)
		}
		if h.Cause != CauseActivation {
			t.Errorf("cause = %s", h.Cause)
		}
		// The address must round-trip: a command that lands on the wrong point
		// is the failure this whole package is arranged to prevent.
		if len(body) < int(p.IOAOctets) {
			t.Fatalf("body is shorter than an address: % X", body)
		}
		if got := p.readIOA(body); got != cmd.IOA {
			t.Errorf("IOA round trip: %d -> %d", cmd.IOA, got)
		}
	})
}

func mustHexBytes(f *testing.F, s string) []byte {
	f.Helper()
	var out []byte
	for _, c := range s {
		switch {
		case c == ' ':
			continue
		case c >= '0' && c <= '9':
			out = append(out, byte(c-'0'))
		case c >= 'A' && c <= 'F':
			out = append(out, byte(c-'A'+10))
		case c >= 'a' && c <= 'f':
			out = append(out, byte(c-'a'+10))
		default:
			f.Fatalf("bad hex seed %q", s)
		}
	}
	if len(out)%2 != 0 {
		f.Fatalf("odd hex seed %q", s)
	}
	packed := make([]byte, len(out)/2)
	for i := range packed {
		packed[i] = out[2*i]<<4 | out[2*i+1]
	}
	return packed
}
