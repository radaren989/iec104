package asdu

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// --- QDS: every bit on its own ----------------------------------------------

// A flag dropped here produces no error anywhere. It produces a perfectly
// ordinary-looking reading whose alarm can no longer explain itself, so every
// bit is asserted separately and in isolation.
func TestQDSBitsAreIndependent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		oct  byte
		want Quality
	}{
		{"all clear", 0x00, Quality{}},
		{"overflow", 0x01, Quality{Overflow: true}},
		{"blocked", 0x10, Quality{Blocked: true}},
		{"substituted", 0x20, Quality{Substituted: true}},
		{"not topical", 0x40, Quality{NotTopical: true}},
		{"invalid", 0x80, Quality{Invalid: true}},
		{"every flag", 0xF1, Quality{Overflow: true, Blocked: true, Substituted: true, NotTopical: true, Invalid: true}},
		// The classic pair: the RTU is fine, the field wiring is not.
		{"invalid and not topical together", 0xC0, Quality{NotTopical: true, Invalid: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ParseQDS(tc.oct)
			if got != tc.want {
				t.Fatalf("0x%02X -> %+v, want %+v", tc.oct, got, tc.want)
			}
			if back := got.QDS(); back != tc.oct {
				t.Errorf("re-encode: 0x%02X, want 0x%02X", back, tc.oct)
			}
		})
	}
}

// Bits 1-3 are reserved. Reading them into a neighbouring flag would mark
// perfectly good values as blocked.
func TestQDSReservedBitsAreIgnored(t *testing.T) {
	t.Parallel()

	if got := ParseQDS(0x0E); !got.Good() {
		t.Errorf("reserved bits leaked into the flags: %+v", got)
	}
	if got := ParseQDS(0x0F); got != (Quality{Overflow: true}) {
		t.Errorf("reserved bits leaked: %+v", got)
	}
}

func TestQualityString(t *testing.T) {
	t.Parallel()

	if got := (Quality{}).String(); got != "good" {
		t.Errorf("clean quality prints %q", got)
	}
	if got := (Quality{Invalid: true, NotTopical: true}).String(); got != "IV|NT" {
		t.Errorf("got %q, want IV|NT", got)
	}
}

// --- SIQ --------------------------------------------------------------------

func TestSIQ(t *testing.T) {
	t.Parallel()

	cases := []struct {
		oct  byte
		want SinglePoint
	}{
		{0x00, SinglePoint{On: false}},
		{0x01, SinglePoint{On: true}},
		{0x81, SinglePoint{On: true, Q: Quality{Invalid: true}}},
		{0x40, SinglePoint{On: false, Q: Quality{NotTopical: true}}},
		{0xF1, SinglePoint{On: true, Q: Quality{Blocked: true, Substituted: true, NotTopical: true, Invalid: true}}},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("0x%02X", tc.oct), func(t *testing.T) {
			t.Parallel()

			got := ParseSIQ(tc.oct)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if back := got.SIQ(); back != tc.oct {
				t.Errorf("re-encode: 0x%02X, want 0x%02X", back, tc.oct)
			}
		})
	}
}

// SIQ has no overflow bit; bit 0 is the value. Reusing the QDS parser here would
// read every ON point as overflowed.
func TestSIQHasNoOverflow(t *testing.T) {
	t.Parallel()

	if got := ParseSIQ(0x01); got.Q.Overflow {
		t.Error("bit 0 of an SIQ is the value, not overflow")
	}
}

// --- DIQ: 0 and 3 are not the same thing ------------------------------------

// The acceptance criterion for T5.3. A double point is two contacts wired in
// opposition: 0 means neither is closed (mid-travel, or a contact failed open),
// 3 means both are (electrically impossible — a wiring fault). Collapsing them
// throws away the difference between "the breaker is moving" and "the breaker's
// position sensing is broken".
func TestDoublePointDistinguishesBothIndeterminateStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		oct        byte
		state      DoublePointState
		determined bool
		name       string
	}{
		{0x00, DPIntermediate, false, "intermediate"},
		{0x01, DPOff, true, "off"},
		{0x02, DPOn, true, "on"},
		{0x03, DPIndeterminate, false, "indeterminate"},
	}

	seen := make(map[DoublePointState]bool)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ParseDIQ(tc.oct)
			if got.State != tc.state {
				t.Fatalf("0x%02X -> %s, want %s", tc.oct, got.State, tc.state)
			}
			if got.State.Determined() != tc.determined {
				t.Errorf("Determined = %v, want %v", got.State.Determined(), tc.determined)
			}
			if back := got.DIQ(); back != tc.oct {
				t.Errorf("re-encode: 0x%02X, want 0x%02X", back, tc.oct)
			}
		})
		seen[tc.state] = true
	}

	if len(seen) != 4 {
		t.Fatalf("the four DPI values must map to four distinct states, got %d", len(seen))
	}
	if DPIntermediate == DPIndeterminate {
		t.Fatal("0 and 3 must not be the same value")
	}
	if DPIntermediate.String() == DPIndeterminate.String() {
		t.Errorf("0 and 3 must not print the same: both say %q", DPIntermediate.String())
	}
}

func TestDIQCarriesQualityAlongsideTheState(t *testing.T) {
	t.Parallel()

	got := ParseDIQ(0xC2) // IV | NT | ON
	if got.State != DPOn {
		t.Errorf("state = %s, want on", got.State)
	}
	if !got.Q.Invalid || !got.Q.NotTopical {
		t.Errorf("quality = %s, want IV|NT", got.Q)
	}
	if got.Q.Overflow {
		t.Error("a DIQ has no overflow bit")
	}
	if back := got.DIQ(); back != 0xC2 {
		t.Errorf("re-encode: 0x%02X", back)
	}
}

// Bits 2-3 are reserved on a DIQ.
func TestDIQReservedBitsAreIgnored(t *testing.T) {
	t.Parallel()

	got := ParseDIQ(0x0E) // reserved bits set, state = 2
	if got.State != DPOn {
		t.Errorf("state = %s, want on", got.State)
	}
	if !got.Q.Good() {
		t.Errorf("reserved bits leaked into the quality: %s", got.Q)
	}
}

// --- VTI: the seventh bit is the sign ---------------------------------------

// Read as unsigned, a tap position of -1 becomes 127 — inside the plausible
// range for a transformer, so nothing looks wrong.
func TestStepPositionIsSevenBitSigned(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		oct  byte
		want int8
	}{
		{"zero", 0x00, 0},
		{"one", 0x01, 1},
		{"the largest positive", 0x3F, 63},
		{"minus one", 0x7F, -1},
		{"minus two", 0x7E, -2},
		{"the largest negative", 0x40, -64},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseVTI([]byte{tc.oct, 0x00})
			if err != nil {
				t.Fatalf("ParseVTI: %v", err)
			}
			if got.Value != tc.want {
				t.Fatalf("0x%02X -> %d, want %d", tc.oct, got.Value, tc.want)
			}
			if got.Transient {
				t.Error("the transient bit must be clear")
			}
			if back := got.AppendTo(nil); !bytes.Equal(back, []byte{tc.oct, 0x00}) {
				t.Errorf("re-encode: % X, want %02X 00", back, tc.oct)
			}
		})
	}
}

// Acting on a transient position is how a tap changer gets a command while it is
// still moving.
func TestStepPositionTransientBit(t *testing.T) {
	t.Parallel()

	got, err := ParseVTI([]byte{0xFF, 0x80}) // transient (0x80) | value bits 0x7F = -1, quality IV
	if err != nil {
		t.Fatalf("ParseVTI: %v", err)
	}
	if !got.Transient {
		t.Fatal("the transient bit must be read")
	}
	if got.Value != -1 {
		t.Errorf("value = %d, want -1 — the transient bit leaked into it", got.Value)
	}
	if !got.Q.Invalid {
		t.Errorf("quality = %s, want IV", got.Q)
	}
	if back := got.AppendTo(nil); !bytes.Equal(back, []byte{0xFF, 0x80}) {
		t.Errorf("re-encode: % X", back)
	}
}

func TestParseVTIRejectsShortBuffer(t *testing.T) {
	t.Parallel()

	for n := range StepPositionLen {
		if _, err := ParseVTI(make([]byte, n)); !errors.Is(err, ErrTruncated) {
			t.Errorf("%d octet(s): got %v, want ErrTruncated", n, err)
		}
	}
}

// --- BCR: the sequence number must survive ----------------------------------

// The acceptance criterion: the sequence number is what makes two readings
// comparable. Consecutive numbers mean consecutive intervals, a repeat means the
// same freeze delivered twice, a gap means an interval was missed. Dropping it
// turns energy counters into numbers nobody can integrate.
func TestCounterSequenceNumberSurvives(t *testing.T) {
	t.Parallel()

	for seq := uint8(0); seq <= bcrSequenceMask; seq++ {
		wire := []byte{0x00, 0x00, 0x00, 0x00, seq}
		got, err := ParseBCR(wire)
		if err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
		if got.Sequence != seq {
			t.Fatalf("sequence %d -> %d", seq, got.Sequence)
		}
		if got.Carry || got.Adjusted || got.Invalid {
			t.Errorf("seq %d: flags leaked from the sequence field: %+v", seq, got)
		}
		if back := got.AppendTo(nil); !bytes.Equal(back, wire) {
			t.Errorf("seq %d: re-encode % X", seq, back)
		}
	}
}

func TestCounterValueIsSignedLittleEndian(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		wire string
		want int32
	}{
		{"zero", "00 00 00 00 00", 0},
		{"one", "01 00 00 00 00", 1},
		{"little endian", "78 56 34 12 00", 0x12345678},
		{"minus one", "FF FF FF FF 00", -1},
		{"the most negative", "00 00 00 80 00", -2147483648},
		{"the most positive", "FF FF FF 7F 00", 2147483647},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseBCR(mustHex(t, tc.wire))
			if err != nil {
				t.Fatalf("ParseBCR: %v", err)
			}
			if got.Value != tc.want {
				t.Fatalf("got %d, want %d", got.Value, tc.want)
			}
			if back := got.AppendTo(nil); !bytes.Equal(back, mustHex(t, tc.wire)) {
				t.Errorf("re-encode: % X", back)
			}
		})
	}
}

func TestCounterFlagsAreIndependent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		flags byte
		want  Counter
	}{
		{"carry", 0x20, Counter{Carry: true}},
		{"adjusted", 0x40, Counter{Adjusted: true}},
		{"invalid", 0x80, Counter{Invalid: true}},
		{"every flag with a sequence number", 0xFF, Counter{Sequence: 31, Carry: true, Adjusted: true, Invalid: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseBCR([]byte{0, 0, 0, 0, tc.flags})
			if err != nil {
				t.Fatalf("ParseBCR: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A counter that wrapped or was adjusted cannot be differenced against the
// previous reading — the difference is wrong in the first case and meaningless
// in the second.
func TestCounterComparable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		c    Counter
		want bool
	}{
		{"a clean reading", Counter{Value: 100, Sequence: 3}, true},
		{"after a wrap", Counter{Value: 100, Carry: true}, false},
		{"after an adjustment", Counter{Value: 100, Adjusted: true}, false},
		{"marked invalid", Counter{Value: 100, Invalid: true}, false},
	}
	for _, tc := range cases {
		if got := tc.c.Comparable(); got != tc.want {
			t.Errorf("%s: Comparable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseBCRRejectsShortBuffer(t *testing.T) {
	t.Parallel()

	for n := range CounterLen {
		if _, err := ParseBCR(make([]byte, n)); !errors.Is(err, ErrTruncated) {
			t.Errorf("%d octet(s): got %v, want ErrTruncated", n, err)
		}
	}
}

// --- round trip over the whole octet space ----------------------------------

// Every one of the 256 possible octets must survive parse-then-encode, with the
// reserved bits cleared. This catches a flag that is read but not written, which
// no hand-picked case is guaranteed to.
func TestQualityOctetsRoundTrip(t *testing.T) {
	t.Parallel()

	for i := range 256 {
		oct := byte(i)

		t.Run(fmt.Sprintf("QDS 0x%02X", oct), func(t *testing.T) {
			t.Parallel()

			// Reserved bits 1-3 are dropped, so compare against the masked octet.
			const qdsKnown = qOverflowBit | qBlockedBit | qSubstitutedBit | qNotTopicalBit | qInvalidBit
			if back := ParseQDS(oct).QDS(); back != oct&qdsKnown {
				t.Errorf("QDS 0x%02X -> 0x%02X, want 0x%02X", oct, back, oct&qdsKnown)
			}

			const siqKnown = siqValueBit | qBlockedBit | qSubstitutedBit | qNotTopicalBit | qInvalidBit
			if back := ParseSIQ(oct).SIQ(); back != oct&siqKnown {
				t.Errorf("SIQ 0x%02X -> 0x%02X, want 0x%02X", oct, back, oct&siqKnown)
			}

			const diqKnown = diqStateMask | qBlockedBit | qSubstitutedBit | qNotTopicalBit | qInvalidBit
			if back := ParseDIQ(oct).DIQ(); back != oct&diqKnown {
				t.Errorf("DIQ 0x%02X -> 0x%02X, want 0x%02X", oct, back, oct&diqKnown)
			}

			vti, err := ParseVTI([]byte{oct, oct})
			if err != nil {
				t.Fatalf("ParseVTI: %v", err)
			}
			if back := vti.AppendTo(nil); back[0] != oct {
				t.Errorf("VTI 0x%02X -> 0x%02X", oct, back[0])
			}

			bcr, err := ParseBCR([]byte{0, 0, 0, 0, oct})
			if err != nil {
				t.Fatalf("ParseBCR: %v", err)
			}
			if back := bcr.AppendTo(nil); back[4] != oct {
				t.Errorf("BCR flags 0x%02X -> 0x%02X", oct, back[4])
			}
		})
	}
}
