package asdu

import (
	"bytes"
	"errors"
	"math"
	"testing"
	"time"
)

func activation(cmd Command) CommandRequest {
	return CommandRequest{CommonAddr: 1, Cause: CauseActivation, Cmd: cmd}
}

// --- one golden per control type --------------------------------------------

// The acceptance criterion for T5.5. Every byte is written out by hand, because
// this is the code that opens a breaker in a substation and a round trip against
// our own encoder would prove only that it agrees with itself.
func TestControlGoldens(t *testing.T) {
	t.Parallel()

	p := DefaultParams() // casdu 2, cot 2, ioa 3

	// Header for every case: type, VSQ=1 object, COT=6 activation, OA=0,
	// CASDU=1 little endian.
	header := func(typ TypeID) string {
		return map[TypeID]string{
			CScNa1: "2D", CDcNa1: "2E", CRcNa1: "2F", CSeNa1: "30",
			CSeNb1: "31", CSeNc1: "32", CBoNa1: "33",
			CScTa1: "3A", CDcTa1: "3B", CRcTa1: "3C", CSeTa1: "3D",
			CSeTb1: "3E", CSeTc1: "3F", CBoTa1: "40",
		}[typ] + " 01 06 00 01 00"
	}

	cases := []struct {
		name string
		cmd  Command
		wire string // header + IOA 100 (64 00 00) + element
	}{
		{
			name: "C_SC_NA_1 execute ON, no pulse definition",
			cmd:  Command{Type: CScNa1, IOA: 100, On: true},
			wire: header(CScNa1) + " 64 00 00 01",
		},
		{
			name: "C_SC_NA_1 execute OFF",
			cmd:  Command{Type: CScNa1, IOA: 100},
			wire: header(CScNa1) + " 64 00 00 00",
		},
		{
			// S/E set: this is the select half of select-before-operate, which
			// reserves the point without driving anything.
			name: "C_SC_NA_1 SELECT ON",
			cmd:  Command{Type: CScNa1, IOA: 100, On: true, Select: true},
			wire: header(CScNa1) + " 64 00 00 81",
		},
		{
			// QU=1 short pulse sits in bits 2-6: 1<<2 = 0x04.
			name: "C_SC_NA_1 ON with a short pulse",
			cmd:  Command{Type: CScNa1, IOA: 100, On: true, Duration: DurationShortPulse},
			wire: header(CScNa1) + " 64 00 00 05",
		},
		{
			name: "C_SC_NA_1 ON with a long pulse",
			cmd:  Command{Type: CScNa1, IOA: 100, On: true, Duration: DurationLongPulse},
			wire: header(CScNa1) + " 64 00 00 09",
		},
		{
			name: "C_SC_NA_1 ON with a persistent output",
			cmd:  Command{Type: CScNa1, IOA: 100, On: true, Duration: DurationPersistent},
			wire: header(CScNa1) + " 64 00 00 0D",
		},
		{
			// 1 is OFF. This is the counter-intuitive one.
			name: "C_DC_NA_1 execute OFF",
			cmd:  Command{Type: CDcNa1, IOA: 100, Double: DoubleOff},
			wire: header(CDcNa1) + " 64 00 00 01",
		},
		{
			// and 2 is ON.
			name: "C_DC_NA_1 execute ON",
			cmd:  Command{Type: CDcNa1, IOA: 100, Double: DoubleOn},
			wire: header(CDcNa1) + " 64 00 00 02",
		},
		{
			name: "C_DC_NA_1 SELECT ON with a short pulse",
			cmd:  Command{Type: CDcNa1, IOA: 100, Double: DoubleOn, Select: true, Duration: DurationShortPulse},
			wire: header(CDcNa1) + " 64 00 00 86",
		},
		{
			name: "C_RC_NA_1 step lower",
			cmd:  Command{Type: CRcNa1, IOA: 100, Step: StepLower},
			wire: header(CRcNa1) + " 64 00 00 01",
		},
		{
			name: "C_RC_NA_1 step higher, selected",
			cmd:  Command{Type: CRcNa1, IOA: 100, Step: StepHigher, Select: true},
			wire: header(CRcNa1) + " 64 00 00 82",
		},
		{
			// Normalised 0.5 -> 16384 -> 00 40 little endian, QOS 0.
			name: "C_SE_NA_1 normalised set point, half scale",
			cmd:  Command{Type: CSeNa1, IOA: 100, Value: 0.5},
			wire: header(CSeNa1) + " 64 00 00 00 40 00",
		},
		{
			name: "C_SE_NA_1 normalised set point, selected",
			cmd:  Command{Type: CSeNa1, IOA: 100, Value: -1, Select: true},
			wire: header(CSeNa1) + " 64 00 00 00 80 80",
		},
		{
			name: "C_SE_NB_1 scaled set point",
			cmd:  Command{Type: CSeNb1, IOA: 100, Scaled: 1000},
			wire: header(CSeNb1) + " 64 00 00 E8 03 00",
		},
		{
			name: "C_SE_NB_1 negative scaled set point",
			cmd:  Command{Type: CSeNb1, IOA: 100, Scaled: -2000},
			wire: header(CSeNb1) + " 64 00 00 30 F8 00",
		},
		{
			// IEEE 754 single: 1.0 is 00 00 80 3F little endian.
			name: "C_SE_NC_1 short float set point",
			cmd:  Command{Type: CSeNc1, IOA: 100, Value: 1},
			wire: header(CSeNc1) + " 64 00 00 00 00 80 3F 00",
		},
		{
			// C_BO carries no qualifier octet at all.
			name: "C_BO_NA_1 bitstring",
			cmd:  Command{Type: CBoNa1, IOA: 100, Bits: 0x0000000F},
			wire: header(CBoNa1) + " 64 00 00 0F 00 00 00",
		},

		// --- the timestamped variants ---
		{
			name: "C_SC_TA_1 single command with a timestamp",
			cmd:  Command{Type: CScTa1, IOA: 100, On: true, Time: theGoldenCP56(t)},
			wire: header(CScTa1) + " 64 00 00 01 " + theGoldenTag,
		},
		{
			name: "C_DC_TA_1 double command with a timestamp",
			cmd:  Command{Type: CDcTa1, IOA: 100, Double: DoubleOn, Time: theGoldenCP56(t)},
			wire: header(CDcTa1) + " 64 00 00 02 " + theGoldenTag,
		},
		{
			name: "C_RC_TA_1 step command with a timestamp",
			cmd:  Command{Type: CRcTa1, IOA: 100, Step: StepHigher, Time: theGoldenCP56(t)},
			wire: header(CRcTa1) + " 64 00 00 02 " + theGoldenTag,
		},
		{
			name: "C_SE_TA_1 normalised set point with a timestamp",
			cmd:  Command{Type: CSeTa1, IOA: 100, Value: 0.5, Time: theGoldenCP56(t)},
			wire: header(CSeTa1) + " 64 00 00 00 40 00 " + theGoldenTag,
		},
		{
			name: "C_SE_TB_1 scaled set point with a timestamp",
			cmd:  Command{Type: CSeTb1, IOA: 100, Scaled: 1000, Time: theGoldenCP56(t)},
			wire: header(CSeTb1) + " 64 00 00 E8 03 00 " + theGoldenTag,
		},
		{
			name: "C_SE_TC_1 short float set point with a timestamp",
			cmd:  Command{Type: CSeTc1, IOA: 100, Value: 1, Time: theGoldenCP56(t)},
			wire: header(CSeTc1) + " 64 00 00 00 00 80 3F 00 " + theGoldenTag,
		},
		{
			name: "C_BO_TA_1 bitstring with a timestamp",
			cmd:  Command{Type: CBoTa1, IOA: 100, Bits: 0x0000000F, Time: theGoldenCP56(t)},
			wire: header(CBoTa1) + " 64 00 00 0F 00 00 00 " + theGoldenTag,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := p.AppendCommand(nil, activation(tc.cmd))
			if err != nil {
				t.Fatalf("AppendCommand: %v", err)
			}
			want := mustHex(t, tc.wire)
			if !bytes.Equal(got, want) {
				t.Errorf("\n got % X\nwant % X", got, want)
			}

			// The header must parse back to a single addressed object, which is
			// the only shape a command ASDU may have.
			h, _, err := p.ParseHeader(got)
			if err != nil {
				t.Fatalf("the encoded command must parse: %v", err)
			}
			if h.Count != 1 || h.Sequence {
				t.Errorf("count=%d sq=%v, want exactly one addressed object", h.Count, h.Sequence)
			}
			if h.Cause != CauseActivation {
				t.Errorf("cause = %s, want activation", h.Cause)
			}
		})
	}
}

func theGoldenCP56(t *testing.T) CP56Time2a {
	t.Helper()
	tag, err := ParseCP56Time2a(mustHex(t, theGoldenTag))
	if err != nil {
		t.Fatalf("golden tag: %v", err)
	}
	return tag
}

func TestEverySupportedControlTypeHasAGolden(t *testing.T) {
	t.Parallel()

	covered := map[TypeID]bool{
		CScNa1: true, CDcNa1: true, CRcNa1: true, CSeNa1: true,
		CSeNb1: true, CSeNc1: true, CBoNa1: true,
		CScTa1: true, CDcTa1: true, CRcTa1: true, CSeTa1: true,
		CSeTb1: true, CSeTc1: true, CBoTa1: true,
	}
	for typ := range controlLayouts {
		if !covered[typ] {
			t.Errorf("%s is encodable but has no golden", typ)
		}
	}
	for typ := range covered {
		if !SupportsControlType(typ) {
			t.Errorf("%s has a golden but is not encodable", typ)
		}
	}
}

// --- the select bit, on its own ---------------------------------------------

// Select-before-operate only works if the bit lands in the right place in every
// qualifier. A select that encodes as an execute drives the output on what the
// operator thought was a reservation.
func TestSelectBitIsTheTopBitOfEveryQualifier(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	cases := []struct {
		name string
		cmd  Command
		// qualifierAt is the offset of the qualifier octet within the whole
		// ASDU: 6 header octets + 3 IOA octets + the value octets before it.
		qualifierAt int
	}{
		{"single command", Command{Type: CScNa1, IOA: 1, On: true}, 9},
		{"double command", Command{Type: CDcNa1, IOA: 1, Double: DoubleOn}, 9},
		{"step command", Command{Type: CRcNa1, IOA: 1, Step: StepHigher}, 9},
		{"normalised set point", Command{Type: CSeNa1, IOA: 1, Value: 0.5}, 11},
		{"scaled set point", Command{Type: CSeNb1, IOA: 1, Scaled: 5}, 11},
		{"short float set point", Command{Type: CSeNc1, IOA: 1, Value: 1}, 13},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			execute, err := p.AppendCommand(nil, activation(tc.cmd))
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			selected := tc.cmd
			selected.Select = true
			sel, err := p.AppendCommand(nil, activation(selected))
			if err != nil {
				t.Fatalf("select: %v", err)
			}

			if len(execute) != len(sel) {
				t.Fatalf("select changed the length: %d vs %d", len(execute), len(sel))
			}
			if execute[tc.qualifierAt]&cmdSelectBit != 0 {
				t.Errorf("execute has the S/E bit set: 0x%02X", execute[tc.qualifierAt])
			}
			if sel[tc.qualifierAt]&cmdSelectBit == 0 {
				t.Errorf("select does not have the S/E bit set: 0x%02X", sel[tc.qualifierAt])
			}
			// Exactly one octet may differ, and it must be the qualifier.
			for i := range execute {
				if i == tc.qualifierAt {
					continue
				}
				if execute[i] != sel[i] {
					t.Errorf("octet %d changed with the select bit: 0x%02X -> 0x%02X", i, execute[i], sel[i])
				}
			}
		})
	}
}

// C_BO has no qualifier octet, so there is nowhere to put the select bit. Asking
// for a selection is refused rather than silently dropped — dropping it would
// turn a reservation into an immediate drive.
func TestBitstringCommandCannotBeSelected(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	for _, typ := range []TypeID{CBoNa1, CBoTa1} {
		cmd := Command{Type: typ, IOA: 1, Bits: 1, Select: true}
		if typ == CBoTa1 {
			cmd.Time = theGoldenCP56(t)
		}
		out, err := p.AppendCommand(nil, activation(cmd))
		if !errors.Is(err, ErrCommand) {
			t.Errorf("%s: got %v, want ErrCommand", typ, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: %d octet(s) written on a refused command", typ, len(out))
		}
	}
}

// --- 0 and 3 are refused, and write nothing ---------------------------------

// The other half of the acceptance criterion. In a double command the value is
// two bits: 0 and 3 mean "indeterminate", which is not a position. Sending one
// to a breaker is undefined behaviour.
func TestDoubleAndStepCommandsRefuseIndeterminateStates(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	cases := []struct {
		name string
		cmd  Command
	}{
		{"double command 0", Command{Type: CDcNa1, IOA: 1, Double: DoubleNotPermitted0}},
		{"double command 3", Command{Type: CDcNa1, IOA: 1, Double: DoubleNotPermitted3}},
		{"double command 0, timestamped", Command{Type: CDcTa1, IOA: 1, Double: DoubleNotPermitted0}},
		{"double command 3, timestamped", Command{Type: CDcTa1, IOA: 1, Double: DoubleNotPermitted3}},
		{"step command 0", Command{Type: CRcNa1, IOA: 1, Step: StepNotPermitted0}},
		{"step command 3", Command{Type: CRcNa1, IOA: 1, Step: StepNotPermitted3}},
		{"step command 0, timestamped", Command{Type: CRcTa1, IOA: 1, Step: StepNotPermitted0}},
		{"step command 3, timestamped", Command{Type: CRcTa1, IOA: 1, Step: StepNotPermitted3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := tc.cmd
			if controlLayouts[cmd.Type].hasTime {
				cmd.Time = theGoldenCP56(t)
			}

			// Start from a non-empty buffer so "wrote nothing" is a real claim
			// and not just "returned an empty slice".
			prefix := []byte{0xDE, 0xAD, 0xBE, 0xEF}
			dst := append([]byte(nil), prefix...)

			out, err := p.AppendCommand(dst, activation(cmd))
			if !errors.Is(err, ErrCommand) {
				t.Fatalf("got %v, want ErrCommand", err)
			}
			if !bytes.Equal(out, prefix) {
				t.Fatalf("a refused command wrote %d octet(s): % X", len(out)-len(prefix), out)
			}
		})
	}
}

// 1 is OFF and 2 is ON. Swapping them drives the breaker the wrong way and the
// frame is perfectly valid, so nothing anywhere reports an error.
func TestDoubleCommandMappingIsNotSwapped(t *testing.T) {
	t.Parallel()

	if DoubleOff != 1 {
		t.Errorf("DoubleOff = %d, want 1", uint8(DoubleOff))
	}
	if DoubleOn != 2 {
		t.Errorf("DoubleOn = %d, want 2", uint8(DoubleOn))
	}
	if !DoubleOff.Permitted() || !DoubleOn.Permitted() {
		t.Error("1 and 2 must be permitted")
	}
	if DoubleNotPermitted0.Permitted() || DoubleNotPermitted3.Permitted() {
		t.Error("0 and 3 must not be permitted")
	}
	if StepLower != 1 || StepHigher != 2 {
		t.Errorf("step mapping = %d/%d, want 1/2", uint8(StepLower), uint8(StepHigher))
	}
}

// --- set point ranges -------------------------------------------------------

// Clamping would turn "set 150%" into "set 100%", which is a different
// instruction that the operator never gave.
func TestNormalisedSetPointRangeIsRefusedNotClamped(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	accepted := []float64{-1, -0.5, 0, 0.5, maxNormalised}
	for _, v := range accepted {
		if _, err := p.AppendCommand(nil, activation(Command{Type: CSeNa1, IOA: 1, Value: v})); err != nil {
			t.Errorf("%v must be accepted: %v", v, err)
		}
	}

	refused := []float64{-1.0001, 1, 1.5, 150, math.NaN()}
	for _, v := range refused {
		out, err := p.AppendCommand(nil, activation(Command{Type: CSeNa1, IOA: 1, Value: v}))
		if !errors.Is(err, ErrCommand) {
			t.Errorf("%v: got %v, want ErrCommand", v, err)
		}
		if len(out) != 0 {
			t.Errorf("%v: %d octet(s) written", v, len(out))
		}
	}
}

// The normalised range is not symmetric: it reaches -1 exactly but never +1.
func TestNormalisedSetPointEdges(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	minusOne, err := p.AppendCommand(nil, activation(Command{Type: CSeNa1, IOA: 1, Value: -1}))
	if err != nil {
		t.Fatalf("-1: %v", err)
	}
	if got := minusOne[len(minusOne)-3 : len(minusOne)-1]; !bytes.Equal(got, []byte{0x00, 0x80}) {
		t.Errorf("-1 encoded as % X, want 00 80", got)
	}

	top, err := p.AppendCommand(nil, activation(Command{Type: CSeNa1, IOA: 1, Value: maxNormalised}))
	if err != nil {
		t.Fatalf("%v: %v", maxNormalised, err)
	}
	if got := top[len(top)-3 : len(top)-1]; !bytes.Equal(got, []byte{0xFF, 0x7F}) {
		t.Errorf("%v encoded as % X, want FF 7F", maxNormalised, got)
	}
}

func TestShortFloatSetPointRefusesNonNumbers(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e40} {
		out, err := p.AppendCommand(nil, activation(Command{Type: CSeNc1, IOA: 1, Value: v}))
		if !errors.Is(err, ErrCommand) {
			t.Errorf("%v: got %v, want ErrCommand", v, err)
		}
		if len(out) != 0 {
			t.Errorf("%v: %d octet(s) written", v, len(out))
		}
	}
}

// --- the shared checks ------------------------------------------------------

func TestCommandValidation(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	valid := Command{Type: CScNa1, IOA: 100, On: true}

	cases := []struct {
		name string
		req  CommandRequest
		want error
	}{
		{"a monitor type", activation(Command{Type: MSpNa1, IOA: 1}), ErrCommand},
		{"an unknown type", activation(Command{Type: 199, IOA: 1}), ErrCommand},
		// Address 0 means "not used", so a command aimed at it is a
		// configuration mistake.
		{"address zero", activation(Command{Type: CScNa1, IOA: 0}), ErrCommand},
		{"address beyond the width", activation(Command{Type: CScNa1, IOA: 0x1000000}), ErrIOARange},
		// 4..31 are reserved or vendor-private; sending one is how an output
		// behaves in a way nobody can explain.
		{"a reserved output duration", activation(Command{Type: CScNa1, IOA: 1, Duration: 4}), ErrCommand},
		{"the largest reserved duration", activation(Command{Type: CScNa1, IOA: 1, Duration: 31}), ErrCommand},
		{"a spontaneous cause", CommandRequest{CommonAddr: 1, Cause: CauseSpontaneous, Cmd: valid}, ErrCommand},
		{"an interrogation cause", CommandRequest{CommonAddr: 1, Cause: CauseIntroGeneral, Cmd: valid}, ErrCommand},
		{"a timestamped type with no timestamp", activation(Command{Type: CScTa1, IOA: 1, On: true}), ErrCommand},
		{"bad parameters", CommandRequest{CommonAddr: 1, Cause: CauseActivation, Cmd: valid}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prefix := []byte{0xAA}
			out, err := p.AppendCommand(append([]byte(nil), prefix...), tc.req)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("must be accepted: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if !bytes.Equal(out, prefix) {
				t.Errorf("a refused command wrote % X", out[len(prefix):])
			}
		})
	}
}

// Deactivation cancels a selection and is the only cause besides activation a
// command may carry.
func TestDeactivationIsAllowed(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	req := CommandRequest{CommonAddr: 1, Cause: CauseDeactivation, Cmd: Command{Type: CScNa1, IOA: 1, On: true}}
	out, err := p.AppendCommand(nil, req)
	if err != nil {
		t.Fatalf("deactivation must be allowed: %v", err)
	}
	h, _, err := p.ParseHeader(out)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Cause != CauseDeactivation {
		t.Errorf("cause = %s, want deactivation", h.Cause)
	}
}

// The test bit must reach the wire: a test command that drives a real output is
// the worst possible outcome of this package.
func TestTestBitReachesTheHeader(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	req := CommandRequest{CommonAddr: 1, Cause: CauseActivation, Test: true,
		Cmd: Command{Type: CScNa1, IOA: 1, On: true}}
	out, err := p.AppendCommand(nil, req)
	if err != nil {
		t.Fatalf("AppendCommand: %v", err)
	}
	h, _, err := p.ParseHeader(out)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if !h.Test {
		t.Error("the test bit did not reach the wire")
	}
}

// --- the timestamp ----------------------------------------------------------

// The timestamped variants exist so a device can refuse a command that spent too
// long in transit. A tag that is not a date defeats that, so it is refused.
func TestTimestampedCommandsNeedAValidTag(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	for typ := range controlLayouts {
		if !controlLayouts[typ].hasTime {
			continue
		}
		cmd := Command{Type: typ, IOA: 1, On: true, Double: DoubleOn, Step: StepHigher, Value: 0.5}
		out, err := p.AppendCommand(nil, activation(cmd)) // zero Time: month 0, day 0
		if !errors.Is(err, ErrCommand) {
			t.Errorf("%s: got %v, want ErrCommand", typ, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: %d octet(s) written", typ, len(out))
		}
	}
}

func TestTimestampedCommandCarriesTheTag(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cmd := Command{Type: CScTa1, IOA: 1, On: true, Time: theGoldenCP56(t)}
	out, err := p.AppendCommand(nil, activation(cmd))
	if err != nil {
		t.Fatalf("AppendCommand: %v", err)
	}

	tag, err := ParseCP56Time2a(out[len(out)-CP56Time2aLen:])
	if err != nil {
		t.Fatalf("ParseCP56Time2a: %v", err)
	}
	instant, ok := tag.Time(time.UTC)
	if !ok {
		t.Fatal("the tag must convert")
	}
	want := time.Date(2026, 9, 15, 14, 30, 45, 678*int(time.Millisecond), time.UTC)
	if !instant.Equal(want) {
		t.Errorf("timestamp = %s, want %s", instant, want)
	}
}

// --- narrower address widths ------------------------------------------------

func TestCommandAcrossIOAWidths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ioaOctets uint8
		ioa       IOA
		wire      string
	}{
		{1, 7, "2D 01 06 00 01 00 07 01"},
		{2, 0x1234, "2D 01 06 00 01 00 34 12 01"},
		{3, 0x123456, "2D 01 06 00 01 00 56 34 12 01"},
	}

	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			t.Parallel()

			p := Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: tc.ioaOctets}
			got, err := p.AppendCommand(nil, activation(Command{Type: CScNa1, IOA: tc.ioa, On: true}))
			if err != nil {
				t.Fatalf("AppendCommand: %v", err)
			}
			if want := mustHex(t, tc.wire); !bytes.Equal(got, want) {
				t.Errorf("\n got % X\nwant % X", got, want)
			}
		})
	}
}
