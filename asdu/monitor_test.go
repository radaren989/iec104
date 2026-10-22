package asdu

import (
	"errors"
	"math"
	"testing"
	"time"
)

// buildASDU assembles a complete ASDU from a header and a hand-written body, so
// the golden vectors below can be read as the wire bytes a capture would show.
func buildASDU(t *testing.T, p Params, h Header, bodyHex string) (Header, []byte) {
	t.Helper()
	wire, err := h.AppendTo(nil, p)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	wire = append(wire, mustHex(t, bodyHex)...)

	got, body, err := p.ParseHeader(wire)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	return got, body
}

// theGoldenTag is the CP56Time2a used throughout: 2026-09-15 14:30:45.678.
const theGoldenTag = "6E B2 1E 0E 4F 09 1A"

func wantGoldenInstant(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 9, 15, 14, 30, 45, 678*int(time.Millisecond), time.UTC)
}

// --- one golden per supported type ------------------------------------------

// Every type in the registry gets at least
// one golden, and the quality, cause and timestamp fields are all asserted, not
// just the value. A value with the right number and missing
// metadata is the failure this layer exists to prevent.
func TestMonitorGoldens(t *testing.T) {
	t.Parallel()

	p := DefaultParams() // casdu 2, cot 2, ioa 3

	cases := []struct {
		name string
		typ  TypeID
		// body is one information object: IOA 100 (64 00 00) then the element.
		body    string
		kind    ValueKind
		wantB   bool
		wantF   float64
		wantI   int64
		q       Quality
		hasTime bool
		extra   func(t *testing.T, pt Point)
	}{
		{
			name: "M_SP_NA_1 single point, ON, good",
			typ:  MSpNa1, body: "64 00 00  01",
			kind: KindBool, wantB: true,
		},
		{
			name: "M_SP_NA_1 single point, OFF, invalid and not topical",
			typ:  MSpNa1, body: "64 00 00  C0",
			kind: KindBool, wantB: false,
			q: Quality{Invalid: true, NotTopical: true},
		},
		{
			name: "M_DP_NA_1 double point, determined ON",
			typ:  MDpNa1, body: "64 00 00  02",
			kind: KindBool, wantB: true,
			extra: func(t *testing.T, pt Point) {
				if pt.DoubleState != DPOn {
					t.Errorf("DoubleState = %s", pt.DoubleState)
				}
			},
		},
		{
			// An indeterminate double point is not OFF. It is marked invalid
			// and the raw state says which of the two it was.
			name: "M_DP_NA_1 double point, both contacts closed",
			typ:  MDpNa1, body: "64 00 00  03",
			kind: KindBool, wantB: false,
			q: Quality{Invalid: true},
			extra: func(t *testing.T, pt Point) {
				if pt.DoubleState != DPIndeterminate {
					t.Errorf("DoubleState = %s, want indeterminate", pt.DoubleState)
				}
			},
		},
		{
			name: "M_ST_NA_1 step position, -1, transient, good quality",
			typ:  MStNa1, body: "64 00 00  FF 00",
			kind: KindInt, wantI: -1,
			extra: func(t *testing.T, pt Point) {
				if !pt.Transient {
					t.Error("the transient flag must survive")
				}
			},
		},
		{
			// A bit pattern, not a number: bit 31 set means that bit is on, not
			// that the value is negative.
			name: "M_BO_NA_1 bitstring with the top bit set",
			typ:  MBoNa1, body: "64 00 00  00 00 00 80  00",
			kind: KindInt, wantI: 0x80000000,
		},
		{
			// Normalised is a fraction of full scale: 16384/32768 = 0.5.
			name: "M_ME_NA_1 normalised, half scale",
			typ:  MMeNa1, body: "64 00 00  00 40  00",
			kind: KindFloat, wantF: 0.5,
		},
		{
			// The most negative normalised value is exactly -1.
			name: "M_ME_NA_1 normalised, full negative scale",
			typ:  MMeNa1, body: "64 00 00  00 80  00",
			kind: KindFloat, wantF: -1,
		},
		{
			name: "M_ME_NB_1 scaled, negative, overflow flagged",
			typ:  MMeNb1, body: "64 00 00  30 F8  01",
			kind: KindFloat, wantF: -2000,
			q: Quality{Overflow: true},
		},
		{
			// IEEE 754 single precision, little endian: 1.0 is 3F 80 00 00 big
			// endian, so 00 00 80 3F on the wire.
			name: "M_ME_NC_1 short float, 1.0",
			typ:  MMeNc1, body: "64 00 00  00 00 80 3F  00",
			kind: KindFloat, wantF: 1,
		},
		{
			name: "M_IT_NA_1 counter with a sequence number",
			typ:  MItNa1, body: "64 00 00  E8 03 00 00  05",
			kind: KindInt, wantI: 1000,
			extra: func(t *testing.T, pt Point) {
				if pt.CounterSeq != 5 {
					t.Errorf("CounterSeq = %d, want 5", pt.CounterSeq)
				}
				if pt.CounterCarry || pt.CounterAdjusted {
					t.Error("no carry or adjustment was signalled")
				}
			},
		},
		{
			name: "M_IT_NA_1 counter that wrapped",
			typ:  MItNa1, body: "64 00 00  01 00 00 00  23",
			kind: KindInt, wantI: 1,
			extra: func(t *testing.T, pt Point) {
				if !pt.CounterCarry {
					t.Error("the carry flag must survive: the difference is not the consumption")
				}
				if pt.CounterSeq != 3 {
					t.Errorf("CounterSeq = %d, want 3", pt.CounterSeq)
				}
			},
		},
		{
			// M_ME_ND_1 carries no quality octet at all.
			name: "M_ME_ND_1 normalised without quality",
			typ:  MMeNd1, body: "64 00 00  00 40",
			kind: KindFloat, wantF: 0.5,
		},

		// --- the same measurands with a time tag ---
		{
			name: "M_SP_TB_1 single point with a timestamp",
			typ:  MSpTb1, body: "64 00 00  01  " + theGoldenTag,
			kind: KindBool, wantB: true, hasTime: true,
		},
		{
			name: "M_DP_TB_1 double point with a timestamp",
			typ:  MDpTb1, body: "64 00 00  01  " + theGoldenTag,
			kind: KindBool, wantB: false, hasTime: true,
			extra: func(t *testing.T, pt Point) {
				if pt.DoubleState != DPOff {
					t.Errorf("DoubleState = %s, want off", pt.DoubleState)
				}
			},
		},
		{
			name: "M_ST_TB_1 step position with a timestamp",
			typ:  MStTb1, body: "64 00 00  05 00  " + theGoldenTag,
			kind: KindInt, wantI: 5, hasTime: true,
		},
		{
			name: "M_BO_TB_1 bitstring with a timestamp",
			typ:  MBoTb1, body: "64 00 00  0F 00 00 00  00  " + theGoldenTag,
			kind: KindInt, wantI: 15, hasTime: true,
		},
		{
			name: "M_ME_TD_1 normalised with a timestamp",
			typ:  MMeTd1, body: "64 00 00  00 40  00  " + theGoldenTag,
			kind: KindFloat, wantF: 0.5, hasTime: true,
		},
		{
			name: "M_ME_TE_1 scaled with a timestamp",
			typ:  MMeTe1, body: "64 00 00  E8 03  00  " + theGoldenTag,
			kind: KindFloat, wantF: 1000, hasTime: true,
		},
		{
			name: "M_ME_TF_1 short float with a timestamp",
			typ:  MMeTf1, body: "64 00 00  00 00 80 3F  00  " + theGoldenTag,
			kind: KindFloat, wantF: 1, hasTime: true,
		},
		{
			name: "M_IT_TB_1 counter with a timestamp",
			typ:  MItTb1, body: "64 00 00  E8 03 00 00  05  " + theGoldenTag,
			kind: KindInt, wantI: 1000, hasTime: true,
			extra: func(t *testing.T, pt Point) {
				if pt.CounterSeq != 5 {
					t.Errorf("CounterSeq = %d", pt.CounterSeq)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := Header{Type: tc.typ, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
			parsed, body := buildASDU(t, p, h, tc.body)

			points, err := p.ParseMonitor(parsed, body)
			if err != nil {
				t.Fatalf("ParseMonitor: %v", err)
			}
			if len(points) != 1 {
				t.Fatalf("%d points, want 1", len(points))
			}
			pt := points[0]

			if pt.Type != tc.typ {
				t.Errorf("Type = %s, want %s", pt.Type, tc.typ)
			}
			if pt.IOA != 100 {
				t.Errorf("IOA = %d, want 100", pt.IOA)
			}
			if pt.Kind != tc.kind {
				t.Fatalf("Kind = %s, want %s", pt.Kind, tc.kind)
			}

			switch tc.kind {
			case KindBool:
				if pt.Bool != tc.wantB {
					t.Errorf("Bool = %v, want %v", pt.Bool, tc.wantB)
				}
			case KindFloat:
				if pt.Float != tc.wantF {
					t.Errorf("Float = %v, want %v", pt.Float, tc.wantF)
				}
			case KindInt:
				if pt.Int != tc.wantI {
					t.Errorf("Int = %d, want %d", pt.Int, tc.wantI)
				}
			}

			// Quality is asserted for every case, not only the ones that set a
			// flag: a decoder that never fills it would otherwise pass.
			if pt.Q != tc.q {
				t.Errorf("Quality = %+v (%s), want %+v (%s)", pt.Q, pt.Q, tc.q, tc.q)
			}

			if pt.HasTime != tc.hasTime {
				t.Fatalf("HasTime = %v, want %v", pt.HasTime, tc.hasTime)
			}
			if tc.hasTime {
				if !pt.Time.Usable() {
					t.Errorf("the tag must be usable: %s", pt.Time)
				}
				instant, ok := pt.Time.Time(time.UTC)
				if !ok || !instant.Equal(wantGoldenInstant(t)) {
					t.Errorf("timestamp = %s, want %s", instant, wantGoldenInstant(t))
				}
			} else if pt.Time != (CP56Time2a{}) {
				t.Errorf("a type without a time tag must leave Time zero: %+v", pt.Time)
			}

			if tc.extra != nil {
				tc.extra(t, pt)
			}
		})
	}
}

// Every supported type must appear in the golden table above. Adding a type to
// the registry without a golden is the way an untested decoder ships.
func TestEverySupportedTypeHasAGolden(t *testing.T) {
	t.Parallel()

	covered := map[TypeID]bool{
		MSpNa1: true, MDpNa1: true, MStNa1: true, MBoNa1: true,
		MMeNa1: true, MMeNb1: true, MMeNc1: true, MItNa1: true, MMeNd1: true,
		MSpTb1: true, MDpTb1: true, MStTb1: true, MBoTb1: true,
		MMeTd1: true, MMeTe1: true, MMeTf1: true, MItTb1: true,
	}
	for typ := range monitorLayouts {
		if !covered[typ] {
			t.Errorf("%s is in the registry but has no golden in TestMonitorGoldens", typ)
		}
	}
	for typ := range covered {
		if !SupportsMonitorType(typ) {
			t.Errorf("%s has a golden but is not in the registry", typ)
		}
	}
}

// --- the timestamp carries its own validity ---------------------------------

// A device may send a tag it has itself marked invalid. The measurement is still
// good; only the reading time is not.
func TestInvalidTimestampDoesNotTaintTheValue(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	h := Header{Type: MMeTf1, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
	// The golden tag with the IV bit set on the minute octet.
	parsed, body := buildASDU(t, p, h, "64 00 00  00 00 80 3F  00  6E B2 9E 0E 4F 09 1A")

	points, err := p.ParseMonitor(parsed, body)
	if err != nil {
		t.Fatalf("ParseMonitor: %v", err)
	}
	pt := points[0]

	if pt.Float != 1 {
		t.Errorf("Float = %v, want 1 — an invalid clock must not spoil the value", pt.Float)
	}
	if !pt.Q.Good() {
		t.Errorf("value quality = %s, want good — IV on the TIMESTAMP is not IV on the value", pt.Q)
	}
	if !pt.Time.Invalid {
		t.Error("the timestamp's IV bit must be surfaced")
	}
	if pt.Time.Usable() {
		t.Error("a tag the device marked invalid is not usable")
	}
}

// --- unsupported and forbidden ----------------------------------------------

// Study Reference §12: log and skip. Dropping the link over one unsupported type
// would take every other point on that RTU down with it.
func TestUnsupportedTypeIsNotAProtocolError(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	for _, typ := range []TypeID{20, 38, 39, 40, 17, 200} {
		h := Header{Type: typ, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
		_, err := p.ParseMonitor(h, mustHex(t, "64 00 00 01"))
		if !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s: got %v, want ErrUnsupportedType", typ, err)
		}
		if SupportsMonitorType(typ) {
			t.Errorf("%s must not be in the registry", typ)
		}
	}
}

// The CP24 ban is checked before the registry, so a forbidden type reports why
// it is forbidden rather than merely that it is unknown.
func TestForbiddenTypeReportsTheBanNotIgnorance(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	for _, typ := range []TypeID{2, 4, 6, 8, 10, 12, 14, 16} {
		h := Header{Type: typ, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
		_, err := p.ParseMonitor(h, mustHex(t, "64 00 00 01"))
		if !errors.Is(err, ErrForbiddenType) {
			t.Errorf("%s: got %v, want ErrForbiddenType", typ, err)
		}
	}
}

// --- several objects, both encodings ----------------------------------------

// The same three measurements must come out identical whether the device
// addressed each one or sent a base address and a run.
func TestManyPointsInBothEncodings(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	addressed := Header{Type: MMeNb1, Count: 3, Cause: CauseIntroGeneral, CommonAddr: 1}
	seq := Header{Type: MMeNb1, Sequence: true, Count: 3, Cause: CauseIntroGeneral, CommonAddr: 1}

	// Scaled values 10, 20, 30 at IOAs 100, 101, 102.
	addressedParsed, addressedBody := buildASDU(t, p, addressed,
		"64 00 00  0A 00 00   65 00 00  14 00 00   66 00 00  1E 00 00")
	seqParsed, seqBody := buildASDU(t, p, seq,
		"64 00 00  0A 00 00  14 00 00  1E 00 00")

	fromAddressed, err := p.ParseMonitor(addressedParsed, addressedBody)
	if err != nil {
		t.Fatalf("addressed: %v", err)
	}
	fromSeq, err := p.ParseMonitor(seqParsed, seqBody)
	if err != nil {
		t.Fatalf("sequence: %v", err)
	}

	if len(fromAddressed) != 3 || len(fromSeq) != 3 {
		t.Fatalf("%d and %d points, want 3 each", len(fromAddressed), len(fromSeq))
	}
	for i := range fromAddressed {
		if fromAddressed[i] != fromSeq[i] {
			t.Errorf("point %d differs\naddressed: %+v\nsequence:  %+v", i, fromAddressed[i], fromSeq[i])
		}
		if want := IOA(100 + i); fromAddressed[i].IOA != want {
			t.Errorf("point %d: IOA = %d, want %d", i, fromAddressed[i].IOA, want)
		}
		if want := float64(10 * (i + 1)); fromAddressed[i].Float != want {
			t.Errorf("point %d: Float = %v, want %v", i, fromAddressed[i].Float, want)
		}
	}
}

// --- value edges ------------------------------------------------------------

func TestNormalisedRange(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cases := []struct {
		name string
		raw  string
		want float64
	}{
		{"zero", "00 00", 0},
		{"one step", "01 00", 1.0 / normalisedFullScale},
		// The range never quite reaches +1, which is why the divisor is 32768.
		{"the largest positive", "FF 7F", 32767.0 / normalisedFullScale},
		{"minus one exactly", "00 80", -1},
		{"one step below zero", "FF FF", -1.0 / normalisedFullScale},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := Header{Type: MMeNa1, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
			parsed, body := buildASDU(t, p, h, "64 00 00  "+tc.raw+"  00")
			points, err := p.ParseMonitor(parsed, body)
			if err != nil {
				t.Fatalf("ParseMonitor: %v", err)
			}
			if points[0].Float != tc.want {
				t.Errorf("got %v, want %v", points[0].Float, tc.want)
			}
		})
	}
}

func TestScaledIsSigned(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cases := []struct {
		raw  string
		want float64
	}{
		{"00 00", 0},
		{"FF 7F", 32767},
		{"00 80", -32768},
		{"FF FF", -1},
	}
	for _, tc := range cases {
		h := Header{Type: MMeNb1, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
		parsed, body := buildASDU(t, p, h, "64 00 00  "+tc.raw+"  00")
		points, err := p.ParseMonitor(parsed, body)
		if err != nil {
			t.Fatalf("ParseMonitor: %v", err)
		}
		if points[0].Float != tc.want {
			t.Errorf("%s -> %v, want %v", tc.raw, points[0].Float, tc.want)
		}
	}
}

// A short float that is NaN or infinite must survive as itself. Silently
// substituting zero would report a working sensor reading nothing.
func TestShortFloatSpecialValues(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	h := Header{Type: MMeNc1, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
	parsed, body := buildASDU(t, p, h, "64 00 00  00 00 C0 7F  00") // NaN
	points, err := p.ParseMonitor(parsed, body)
	if err != nil {
		t.Fatalf("ParseMonitor: %v", err)
	}
	if !math.IsNaN(points[0].Float) {
		t.Errorf("NaN became %v", points[0].Float)
	}

	parsed, body = buildASDU(t, p, h, "64 00 00  00 00 80 7F  00") // +Inf
	points, err = p.ParseMonitor(parsed, body)
	if err != nil {
		t.Fatalf("ParseMonitor: %v", err)
	}
	if !math.IsInf(points[0].Float, 1) {
		t.Errorf("+Inf became %v", points[0].Float)
	}
}

// --- length ------------------------------------------------------------------

func TestElementLen(t *testing.T) {
	t.Parallel()

	cases := map[TypeID]int{
		MSpNa1: 1, MDpNa1: 1, MStNa1: 2, MBoNa1: 5,
		MMeNa1: 3, MMeNb1: 3, MMeNc1: 5, MItNa1: 5, MMeNd1: 2,
		MSpTb1: 8, MDpTb1: 8, MStTb1: 9, MBoTb1: 12,
		MMeTd1: 10, MMeTe1: 10, MMeTf1: 12, MItTb1: 12,
	}
	for typ, want := range cases {
		got, err := ElementLen(typ)
		if err != nil {
			t.Errorf("%s: %v", typ, err)
			continue
		}
		if got != want {
			t.Errorf("%s: element length %d, want %d", typ, got, want)
		}
	}
	if _, err := ElementLen(200); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("unknown type: got %v", err)
	}
}

// A body that does not match the element length is a misconfiguration, not a
// decode to attempt.
func TestParseMonitorChecksTheBodyLength(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	h := Header{Type: MMeNb1, Count: 2, Cause: CauseSpontaneous, CommonAddr: 1}

	if _, err := p.ParseMonitor(h, mustHex(t, "64 00 00  0A 00 00")); !errors.Is(err, ErrLength) {
		t.Fatalf("got %v, want ErrLength", err)
	}
}
