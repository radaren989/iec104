package asdu

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex vector %q: %v", s, err)
	}
	return b
}

// --- parameters -------------------------------------------------------------

func TestDefaultParamsAreWhat104Uses(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	if err := p.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if p.CASDUOctets != 2 || p.COTOctets != 2 || p.IOAOctets != 3 {
		t.Errorf("got %d/%d/%d, want 2/2/3", p.CASDUOctets, p.COTOctets, p.IOAOctets)
	}
	if got := p.HeaderLen(); got != 6 {
		t.Errorf("HeaderLen = %d, want 6", got)
	}
}

func TestValidateRejectsImpossibleWidths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		p    Params
	}{
		{"casdu of zero", Params{CASDUOctets: 0, COTOctets: 2, IOAOctets: 3}},
		{"casdu of three", Params{CASDUOctets: 3, COTOctets: 2, IOAOctets: 3}},
		{"cot of zero", Params{CASDUOctets: 2, COTOctets: 0, IOAOctets: 3}},
		{"cot of three", Params{CASDUOctets: 2, COTOctets: 3, IOAOctets: 3}},
		{"ioa of zero", Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: 0}},
		{"ioa of four", Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: 4}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := tc.p.Validate(); !errors.Is(err, ErrParams) {
				t.Fatalf("got %v, want ErrParams", err)
			}
		})
	}
}

func TestHeaderLenFollowsTheWidths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		casdu, cot uint8
		want       int
	}{
		{1, 1, 4}, {1, 2, 5}, {2, 1, 5}, {2, 2, 6},
	}
	for _, tc := range cases {
		p := Params{CASDUOctets: tc.casdu, COTOctets: tc.cot, IOAOctets: 3}
		if got := p.HeaderLen(); got != tc.want {
			t.Errorf("casdu=%d cot=%d: HeaderLen = %d, want %d", tc.casdu, tc.cot, got, tc.want)
		}
	}
}

// --- the header, every width combination ------------------------------------

// The acceptance criterion for T5.1. A master configured for one width talking
// to a device using another misparses every frame, so every combination has to
// round-trip.
func TestHeaderRoundTripsInEveryWidthCombination(t *testing.T) {
	t.Parallel()

	for _, casdu := range []uint8{1, 2} {
		for _, cot := range []uint8{1, 2} {
			for _, ioa := range []uint8{1, 2, 3} {
				p := Params{CASDUOctets: casdu, COTOctets: cot, IOAOctets: ioa}
				name := fmt.Sprintf("casdu=%d cot=%d ioa=%d", casdu, cot, ioa)

				t.Run(name, func(t *testing.T) {
					t.Parallel()

					h := Header{
						Type:       MSpTb1,
						Sequence:   true,
						Count:      7,
						Cause:      CauseSpontaneous,
						Negative:   true,
						Test:       true,
						CommonAddr: 200,
					}
					if cot == 2 {
						h.Originator = 42
					}
					if casdu == 1 {
						h.CommonAddr = 200 // still fits one octet
					}

					wire, err := h.AppendTo(nil, p)
					if err != nil {
						t.Fatalf("AppendTo: %v", err)
					}
					if len(wire) != p.HeaderLen() {
						t.Fatalf("encoded %d octet(s), HeaderLen says %d", len(wire), p.HeaderLen())
					}

					got, body, err := p.ParseHeader(wire)
					if err != nil {
						t.Fatalf("ParseHeader: %v", err)
					}
					if len(body) != 0 {
						t.Errorf("body = % X, want empty", body)
					}
					if got != h {
						t.Errorf("round trip changed the header\n got %+v\nwant %+v", got, h)
					}
				})
			}
		}
	}
}

// A hand-written vector, so the test cannot pass by agreeing with its own
// encoder. M_SP_NA_1, one object, spontaneous, CASDU 1 — the shape of the
// single most common frame on a 104 link.
func TestHeaderGoldenVector(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	wire := mustHex(t, "01 01 03 00 01 00")

	h, body, err := p.ParseHeader(wire)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	want := Header{Type: MSpNa1, Count: 1, Cause: CauseSpontaneous, CommonAddr: 1}
	if h != want {
		t.Errorf("got %+v, want %+v", h, want)
	}
	if len(body) != 0 {
		t.Errorf("body = % X", body)
	}

	back, err := h.AppendTo(nil, p)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	if !bytes.Equal(back, wire) {
		t.Errorf("re-encode\n got % X\nwant % X", back, wire)
	}
}

func TestQualifierBitsAreReadIndependently(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cases := []struct {
		name     string
		wire     string
		sequence bool
		count    uint8
		cause    Cause
		negative bool
		test     bool
	}{
		{"plain", "01 01 03 00 01 00", false, 1, CauseSpontaneous, false, false},
		{"sq set", "01 85 03 00 01 00", true, 5, CauseSpontaneous, false, false},
		{"count at the maximum", "01 7F 03 00 01 00", false, MaxObjects, CauseSpontaneous, false, false},
		// P/N is the difference between "the breaker is opening" and "the
		// breaker refused to open" — the same frame apart from this bit.
		{"P/N set on a confirmation", "2D 01 47 00 01 00", false, 1, CauseActCon, true, false},
		{"test bit set", "01 01 83 00 01 00", false, 1, CauseSpontaneous, false, true},
		{"both flags with sq", "01 82 C3 00 01 00", true, 2, CauseSpontaneous, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, _, err := p.ParseHeader(mustHex(t, tc.wire))
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.Sequence != tc.sequence {
				t.Errorf("Sequence = %v, want %v", h.Sequence, tc.sequence)
			}
			if h.Count != tc.count {
				t.Errorf("Count = %d, want %d", h.Count, tc.count)
			}
			if h.Cause != tc.cause {
				t.Errorf("Cause = %s, want %s", h.Cause, tc.cause)
			}
			if h.Negative != tc.negative {
				t.Errorf("Negative = %v, want %v", h.Negative, tc.negative)
			}
			if h.Test != tc.test {
				t.Errorf("Test = %v, want %v", h.Test, tc.test)
			}
		})
	}
}

// The common address is little-endian, like every multi-octet field in 101/104.
// Reading it big-endian gives a plausible station number for small values and a
// wrong one for everything else.
func TestCommonAddressIsLittleEndian(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	h, _, err := p.ParseHeader(mustHex(t, "01 01 03 00 34 12"))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.CommonAddr != 0x1234 {
		t.Errorf("CommonAddr = 0x%04X, want 0x1234", h.CommonAddr)
	}
}

func TestParseHeaderRejectsMalformed(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cases := []struct {
		name string
		wire string
		want error
	}{
		{"empty", "", ErrTruncated},
		{"one octet short", "01 01 03 00 01", ErrTruncated},
		// The variable structure qualifier says zero objects. An ASDU exists to
		// carry information objects.
		{"no objects", "01 00 03 00 01 00", ErrObjectCount},
		{"no objects with sq set", "01 80 03 00 01 00", ErrObjectCount},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := p.ParseHeader(mustHex(t, tc.wire)); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAppendHeaderRejectsImpossibleValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		p    Params
		h    Header
		want error
	}{
		{"zero objects", DefaultParams(), Header{Type: MSpNa1, Count: 0}, ErrObjectCount},
		{"more objects than the qualifier holds", DefaultParams(),
			Header{Type: MSpNa1, Count: MaxObjects + 1}, ErrObjectCount},
		{"cause beyond six bits", DefaultParams(),
			Header{Type: MSpNa1, Count: 1, Cause: 64}, ErrParams},
		{"common address beyond one octet", Params{CASDUOctets: 1, COTOctets: 2, IOAOctets: 3},
			Header{Type: MSpNa1, Count: 1, CommonAddr: 256}, ErrParams},
		// Dropping the originator silently would produce a frame that looks
		// right and routes confirmations to the wrong master.
		{"originator with a one-octet cot", Params{CASDUOctets: 2, COTOctets: 1, IOAOctets: 3},
			Header{Type: MSpNa1, Count: 1, Originator: 7}, ErrParams},
		{"bad parameters", Params{}, Header{Type: MSpNa1, Count: 1}, ErrParams},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := tc.h.AppendTo(nil, tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// --- information objects ----------------------------------------------------

// The other half of the acceptance criterion: a device may send the same data
// as N addressed objects or as one base address plus N elements, and nothing
// above this line should have to care which.
func TestSequenceAndAddressedEncodingsProduceTheSameData(t *testing.T) {
	t.Parallel()

	p := DefaultParams() // 3-octet IOA
	const elemLen = 1

	// Three single points at 100, 101, 102 with values 0x01, 0x00, 0x01.
	addressed := mustHex(t, "64 00 00 01  65 00 00 00  66 00 00 01")
	sequential := mustHex(t, "64 00 00  01 00 01")

	fromAddressed, err := p.SplitElements(Header{Type: MSpNa1, Count: 3}, addressed, elemLen)
	if err != nil {
		t.Fatalf("addressed: %v", err)
	}
	fromSequence, err := p.SplitElements(Header{Type: MSpNa1, Sequence: true, Count: 3}, sequential, elemLen)
	if err != nil {
		t.Fatalf("sequence: %v", err)
	}

	if len(fromAddressed) != len(fromSequence) {
		t.Fatalf("%d vs %d elements", len(fromAddressed), len(fromSequence))
	}
	for i := range fromAddressed {
		a, s := fromAddressed[i], fromSequence[i]
		if a.IOA != s.IOA {
			t.Errorf("element %d: IOA %d vs %d", i, a.IOA, s.IOA)
		}
		if !bytes.Equal(a.Data, s.Data) {
			t.Errorf("element %d: data % X vs % X", i, a.Data, s.Data)
		}
	}
	if fromAddressed[0].IOA != 100 || fromAddressed[2].IOA != 102 {
		t.Errorf("addresses = %d..%d, want 100..102", fromAddressed[0].IOA, fromAddressed[2].IOA)
	}
}

func TestSplitElementsAcrossIOAWidths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ioa  uint8
		body string
		want IOA
	}{
		{"one octet", 1, "07 AA", 7},
		{"two octets, little endian", 2, "34 12 AA", 0x1234},
		{"three octets, little endian", 3, "56 34 12 AA", 0x123456},
		{"three octets at the maximum", 3, "FF FF FF AA", 0xFFFFFF},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: tc.ioa}
			elems, err := p.SplitElements(Header{Type: MSpNa1, Count: 1}, mustHex(t, tc.body), 1)
			if err != nil {
				t.Fatalf("SplitElements: %v", err)
			}
			if elems[0].IOA != tc.want {
				t.Errorf("IOA = %d (0x%X), want %d", elems[0].IOA, elems[0].IOA, tc.want)
			}
			if !bytes.Equal(elems[0].Data, []byte{0xAA}) {
				t.Errorf("data = % X", elems[0].Data)
			}
		})
	}
}

// A body that does not match what the header promises means the field widths
// are misconfigured — the failure that otherwise shows up as plausible values at
// the wrong addresses.
func TestSplitElementsChecksTheLengthExactly(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cases := []struct {
		name string
		h    Header
		body string
		elem int
	}{
		{"addressed, one octet short", Header{Type: MSpNa1, Count: 2}, "64 00 00 01  65 00 00", 1},
		{"addressed, one octet long", Header{Type: MSpNa1, Count: 2}, "64 00 00 01  65 00 00 00 FF", 1},
		{"sequence, one octet short", Header{Type: MSpNa1, Sequence: true, Count: 3}, "64 00 00 01 00", 1},
		{"sequence, one octet long", Header{Type: MSpNa1, Sequence: true, Count: 3}, "64 00 00 01 00 01 FF", 1},
		{"empty body", Header{Type: MSpNa1, Count: 1}, "", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := p.SplitElements(tc.h, mustHex(t, tc.body), tc.elem); !errors.Is(err, ErrLength) {
				t.Fatalf("got %v, want ErrLength", err)
			}
		})
	}
}

// A sequence whose implied addresses run past the configured width would wrap
// and put the tail of the sequence on top of low-numbered points, which reads
// as real data.
func TestSequenceAddressesMustFitTheWidth(t *testing.T) {
	t.Parallel()

	p := Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: 1} // addresses 0..255
	h := Header{Type: MSpNa1, Sequence: true, Count: 3}

	// Base 253 with three elements reaches 255 exactly: legal.
	if _, err := p.SplitElements(h, mustHex(t, "FD 01 00 01"), 1); err != nil {
		t.Fatalf("253..255 must fit one octet: %v", err)
	}
	// Base 254 would reach 256.
	if _, err := p.SplitElements(h, mustHex(t, "FE 01 00 01"), 1); !errors.Is(err, ErrIOARange) {
		t.Fatalf("254..256 must be rejected, got %v", err)
	}
}

// --- encoding information objects -------------------------------------------

func TestAppendElementsRoundTrips(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	elems := []Element{
		{IOA: 100, Data: []byte{0x01}},
		{IOA: 101, Data: []byte{0x00}},
		{IOA: 102, Data: []byte{0x01}},
	}

	for _, sequence := range []bool{false, true} {
		t.Run(fmt.Sprintf("sq=%v", sequence), func(t *testing.T) {
			t.Parallel()

			h := Header{Type: MSpNa1, Sequence: sequence, Count: uint8(len(elems))}
			body, err := p.AppendElements(nil, h, elems)
			if err != nil {
				t.Fatalf("AppendElements: %v", err)
			}
			back, err := p.SplitElements(h, body, 1)
			if err != nil {
				t.Fatalf("SplitElements: %v", err)
			}
			for i := range elems {
				if back[i].IOA != elems[i].IOA || !bytes.Equal(back[i].Data, elems[i].Data) {
					t.Errorf("element %d: got %d/% X, want %d/% X",
						i, back[i].IOA, back[i].Data, elems[i].IOA, elems[i].Data)
				}
			}
		})
	}
}

func TestAppendElementsRejectsInconsistentInput(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cases := []struct {
		name  string
		h     Header
		elems []Element
		want  error
	}{
		{"count disagrees with the slice", Header{Type: MSpNa1, Count: 2},
			[]Element{{IOA: 1, Data: []byte{0}}}, ErrLength},
		{"address beyond the width", Header{Type: MSpNa1, Count: 1},
			[]Element{{IOA: 0x1000000, Data: []byte{0}}}, ErrIOARange},
		// sq=1 means "consecutive from the first address"; silently renumbering
		// would move points without telling anyone.
		{"sq=1 with a gap", Header{Type: MSpNa1, Sequence: true, Count: 2},
			[]Element{{IOA: 1, Data: []byte{0}}, {IOA: 3, Data: []byte{0}}}, ErrIOARange},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := p.AppendElements(nil, tc.h, tc.elems); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// --- names ------------------------------------------------------------------

func TestTypeAndCauseNames(t *testing.T) {
	t.Parallel()

	if got := MSpTb1.String(); got != "M_SP_TB_1" {
		t.Errorf("TypeID 30 = %q", got)
	}
	// An unknown type must still print something an operator can act on.
	if got := TypeID(200).String(); got != "TypeID(200)" {
		t.Errorf("unknown type = %q", got)
	}
	if got := Cause(25).String(); got != "interrogated-group-5" {
		t.Errorf("Cause 25 = %q, want interrogated-group-5", got)
	}
	if got := Cause(40).String(); got != "req-counter-group-3" {
		t.Errorf("Cause 40 = %q", got)
	}
}

// Interrogation responses refresh the cache but are NOT events. Treating them
// as events replays the whole point list into the event stream on every
// reconnect.
func TestIsInterrogationCoversGeneralAndEveryGroup(t *testing.T) {
	t.Parallel()

	if !CauseIntroGeneral.IsInterrogation() {
		t.Error("the general interrogation cause must count")
	}
	for c := CauseIntroGroup1; c <= 36; c++ {
		if !c.IsInterrogation() {
			t.Errorf("group cause %d must count", c)
		}
	}
	for _, c := range []Cause{CauseSpontaneous, CausePeriodic, CauseActCon, CauseReqCounter, 37, 19} {
		if c.IsInterrogation() {
			t.Errorf("%s must not count as an interrogation response", c)
		}
	}
}

func TestDirectionSplit(t *testing.T) {
	t.Parallel()

	for _, tt := range []TypeID{MSpNa1, MDpTb1, MItTb1, MEpTf1} {
		if !tt.IsMonitor() || tt.IsControl() {
			t.Errorf("%s must be monitor direction", tt)
		}
	}
	for _, tt := range []TypeID{CScNa1, CDcNa1, CSeNc1, CBoTa1} {
		if !tt.IsControl() || tt.IsMonitor() {
			t.Errorf("%s must be control direction", tt)
		}
	}
	// System types are neither.
	for _, tt := range []TypeID{MEiNa1, CIcNa1, CCsNa1} {
		if tt.IsMonitor() || tt.IsControl() {
			t.Errorf("%s must be neither monitor nor control", tt)
		}
	}
}

// Exactly four types are contacts, checked over every value the octet can
// hold. The chatter filter applies to these and nothing else, so a step
// position or a measurand slipping in would start flagging healthy points.
func TestOnlySingleAndDoublePointsAreContacts(t *testing.T) {
	t.Parallel()

	want := map[TypeID]bool{MSpNa1: true, MSpTb1: true, MDpNa1: true, MDpTb1: true}
	for v := 0; v < 256; v++ {
		tt := TypeID(v)
		if got := tt.IsContact(); got != want[tt] {
			t.Errorf("%s (%d): IsContact = %v, want %v", tt, v, got, want[tt])
		}
	}
}
