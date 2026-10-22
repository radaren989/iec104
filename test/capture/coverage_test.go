package capture

import (
	"sort"
	"testing"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/asdu"
)

// TestCaptureCoverage pins what this capture exercises and reports what it does
// not.
//
// Two reasons it exists. First, if the fixture is ever replaced with a thinner
// one, the loss is loud rather than silent — a capture that quietly stops
// covering the command path would leave the strongest tests in the repository
// passing while proving much less. Second, the gap list is the honest record of
// what is still only tested against the specification, and therefore what a
// second capture should aim at.
func TestCaptureCoverage(t *testing.T) {
	t.Parallel()

	// The union across the whole corpus. "A real device exercised this type" is
	// a claim about the corpus, not about any one capture, so a type covered by
	// the simulator and not the RTU still counts - and the shopping list below
	// shrinks accordingly.
	var frames []frame
	for _, c := range captures {
		frames = append(frames, walk(t, readCapture(t, c.Path))...)
	}

	types := map[asdu.TypeID]int{}
	causes := map[asdu.Cause]int{}
	formats := map[apci.Format]int{}
	functions := map[apci.UFunction]int{}

	for _, f := range frames {
		formats[f.Frame.Format]++
		if f.Frame.Format == apci.FormatU {
			functions[f.Frame.Function]++
			continue
		}
		if f.Frame.Format != apci.FormatI {
			continue
		}
		h, _, err := captureParams.ParseHeader(f.Frame.ASDU)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		types[h.Type]++
		causes[h.Cause]++
	}

	// All three frame formats, or the capture is not a whole conversation.
	for _, want := range []apci.Format{apci.FormatI, apci.FormatS, apci.FormatU} {
		if formats[want] == 0 {
			t.Errorf("the capture contains no %s-format frames", want)
		}
	}
	// The startup handshake, which is what makes this a cold start rather than
	// a slice out of the middle of a session.
	for _, want := range []apci.UFunction{apci.StartDTAct, apci.StartDTCon, apci.TestFRAct, apci.TestFRCon} {
		if functions[want] == 0 {
			t.Errorf("the capture contains no %s", want)
		}
	}

	mustHaveTypes := []asdu.TypeID{
		// Monitor, untimed and timed.
		asdu.MSpNa1, asdu.MDpNa1, asdu.MStNa1, asdu.MBoNa1,
		asdu.MMeNa1, asdu.MMeNb1, asdu.MMeNc1,
		asdu.MSpTb1, asdu.MDpTb1, asdu.MStTb1, asdu.MBoTb1,
		asdu.MMeTd1, asdu.MMeTe1, asdu.MMeTf1,
		// Every untimestamped command.
		asdu.CScNa1, asdu.CDcNa1, asdu.CRcNa1,
		asdu.CSeNa1, asdu.CSeNb1, asdu.CSeNc1, asdu.CBoNa1,
		// Counters, and the normalised type with no quality octet at all.
		asdu.MItNa1, asdu.MMeNd1,
		// System. The last two come from the simulator captures only.
		asdu.CIcNa1, asdu.MEiNa1, asdu.CCsNa1, asdu.CCiNa1,
	}
	for _, want := range mustHaveTypes {
		if types[want] == 0 {
			t.Errorf("the capture no longer contains %s", want)
		}
	}

	// The full interrogation bracket and a command's full answer. Losing the
	// termination would be the worst of these to lose quietly: it is the cold
	// start gate.
	mustHaveCauses := []asdu.Cause{
		asdu.CauseSpontaneous, asdu.CauseInitialised, asdu.CauseActivation,
		asdu.CauseActCon, asdu.CauseActTerm, asdu.CauseIntroGeneral,
	}
	for _, want := range mustHaveCauses {
		if causes[want] == 0 {
			t.Errorf("the capture no longer contains cause %s", want)
		}
	}

	// What is still only tested against the standard. Logged, not failed: this
	// is a shopping list for the next capture, not a defect.
	notCovered := []asdu.TypeID{
		asdu.MItTb1,                           // the timestamped counter
		asdu.CScTa1, asdu.CDcTa1, asdu.CRcTa1, // timestamped commands
		asdu.CSeTa1, asdu.CSeTb1, asdu.CSeTc1, asdu.CBoTa1,
		asdu.CRpNa1, // reset process
	}
	var missing []string
	for _, typ := range notCovered {
		if types[typ] == 0 {
			missing = append(missing, typ.String())
		}
	}
	sort.Strings(missing)
	t.Logf("types covered by a real device: %d", len(types))
	t.Logf("still only tested against the standard: %v", missing)
	t.Log("also uncovered: quality flags other than the derived double-point IV, " +
		"an IV or SU timestamp, the negative confirmations (COT 44-47), and a P/N refusal")
}

// TestCaptureConfirmsDerivedValues pins the three conversions a real device
// round-tripped for us.
//
// Each one is a command we sent and the value the device reported back, so the
// device and this implementation independently agree on the encoding. That is a
// stronger statement than any hand-written vector can make: a vector proves the
// code matches my reading of the standard, this proves it matches a device's.
func TestCaptureConfirmsDerivedValues(t *testing.T) {
	t.Parallel()

	frames := walk(t, readCapture(t, rtuCapture().Path))

	// Collect every spontaneous measurement by address and type.
	type key struct {
		typ asdu.TypeID
		ioa asdu.IOA
	}
	got := map[key]float64{}

	for _, f := range frames {
		if f.Frame.Format != apci.FormatI {
			continue
		}
		h, body, err := captureParams.ParseHeader(f.Frame.ASDU)
		if err != nil || !h.Type.IsMonitor() || h.Cause != asdu.CauseSpontaneous {
			continue
		}
		points, err := captureParams.ParseMonitor(h, body)
		if err != nil {
			continue
		}
		for _, p := range points {
			if p.Kind == asdu.KindFloat {
				got[key{p.Type, p.IOA}] = p.Float
			}
		}
	}

	cases := []struct {
		name string
		k    key
		want float64
		why  string
	}{
		{
			name: "normalised full scale is 32768, not 32767",
			k:    key{asdu.MMeNa1, 1}, want: 0.03125,
			why: "we sent C_SE_NA_1 with the raw value 1024; 1024/32768 is exactly 0.03125, " +
				"and 1024/32767 is not. The device reported back what we asked for.",
		},
		{
			name: "scaled values are signed little-endian integers",
			k:    key{asdu.MMeNb1, 3}, want: 123,
			why: "we sent C_SE_NB_1 with 7B 00; the device reported 123.",
		},
		{
			name: "short floats are IEEE 754 little-endian",
			k:    key{asdu.MMeNc1, 1}, want: 3.140000104904175,
			why: "we sent C_SE_NC_1 with C3 F5 48 40, which is 3.14 as a float32; " +
				"the value is the float64 widening of that, not 3.14 exactly.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v, ok := got[tc.k]
			if !ok {
				t.Fatalf("the capture no longer reports %s at IOA %d", tc.k.typ, tc.k.ioa)
			}
			if v != tc.want {
				t.Errorf("got %v, want %v\n%s", v, tc.want, tc.why)
			}
		})
	}
}

// TestCaptureConfirmsTimestampDecoding checks the CP56Time2a decoding against a
// field the device sends redundantly.
//
// The tag carries both the date and the day of the week. Nothing in the decoder
// derives one from the other — the day of week is read straight out of its bits
// — so the two agreeing is an independent check on the date, the century rule
// and the day-of-week numbering all at once. 4 July 2013 was a Thursday, which
// the standard numbers 4.
func TestCaptureConfirmsTimestampDecoding(t *testing.T) {
	t.Parallel()

	frames := walk(t, readCapture(t, rtuCapture().Path))
	checked := 0

	for _, f := range frames {
		if f.Frame.Format != apci.FormatI {
			continue
		}
		h, body, err := captureParams.ParseHeader(f.Frame.ASDU)
		if err != nil || !h.Type.IsMonitor() {
			continue
		}
		points, err := captureParams.ParseMonitor(h, body)
		if err != nil {
			continue
		}
		for _, p := range points {
			if !p.HasTime {
				continue
			}
			if !p.Time.InRange() {
				t.Errorf("%s at IOA %d: a real device's tag is out of range: %s", p.Type, p.IOA, p.Time)
				continue
			}
			ts, ok := p.Time.Time(nil)
			if !ok {
				t.Fatalf("an in-range tag must convert: %s", p.Time)
			}
			// Go counts Sunday as 0; the standard counts Monday as 1.
			wantDOW := uint8((int(ts.Weekday())+6)%7 + 1)
			if p.Time.DayOfWeek != 0 && p.Time.DayOfWeek != wantDOW {
				t.Errorf("%s at IOA %d: the tag says weekday %d but %s is a %s (%d)",
					p.Type, p.IOA, p.Time.DayOfWeek, ts.Format("2006-01-02"), ts.Weekday(), wantDOW)
			}
			if ts.Year() != 2013 {
				t.Errorf("century rule: got year %d, want 2013 from a two-digit 13", ts.Year())
			}
			checked++
		}
	}

	if checked == 0 {
		t.Fatal("the capture no longer contains any timestamped values")
	}
	t.Logf("%d timestamps agreed with their own day-of-week field", checked)
}
