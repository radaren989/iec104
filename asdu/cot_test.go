package asdu

import (
	"strings"
	"testing"
)

// --- the four negative confirmations ----------------------------------------

// These four are the ONLY signal that the data map is wrong. A point configured
// with a type the device does not have produces no error anywhere else: the
// value simply never arrives, which looks exactly like a quiet field.
func TestNegativeConfirmations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cause  Cause
		reason RejectReason
	}{
		{CauseUnknownType, ReasonStationUnknownType},
		{CauseUnknownCause, ReasonStationUnknownCause},
		{CauseUnknownCommonAd, ReasonStationUnknownCommonAddress},
		{CauseUnknownIOA, ReasonStationUnknownIOA},
	}

	for _, tc := range cases {
		t.Run(tc.cause.String(), func(t *testing.T) {
			t.Parallel()

			if !tc.cause.IsNegativeConfirmation() {
				t.Fatal("must be a negative confirmation")
			}
			if !tc.cause.IsConfirmation() {
				t.Error("a negative confirmation is still a confirmation")
			}

			reason, ok := tc.cause.RejectReason()
			if !ok {
				t.Fatal("must map to a reject reason")
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}

			// The explanation is for whoever reads the log at three in the
			// morning, so it has to name what to go and look at.
			explain, ok := tc.cause.Explain()
			if !ok {
				t.Fatal("must have an explanation")
			}
			if !strings.Contains(explain, "check") {
				t.Errorf("the explanation must say what to check: %q", explain)
			}
		})
	}
}

func TestOrdinaryCausesAreNotNegativeConfirmations(t *testing.T) {
	t.Parallel()

	for _, c := range []Cause{
		CausePeriodic, CauseBackground, CauseSpontaneous, CauseInitialised,
		CauseRequest, CauseActivation, CauseActCon, CauseActTerm,
		CauseRetRem, CauseRetLoc, CauseIntroGeneral, CauseReqCounter, 43,
	} {
		if c.IsNegativeConfirmation() {
			t.Errorf("%s must not be a negative confirmation", c)
		}
		if _, ok := c.RejectReason(); ok {
			t.Errorf("%s must not map to a reject reason", c)
		}
		if _, ok := c.Explain(); ok {
			t.Errorf("%s must not have a negative-confirmation explanation", c)
		}
	}
}

// The gateway-side and station-side labels must stay apart: one is a gap in this
// gateway, the other is a data map that does not match the device. A single
// label for both makes the dashboard useless exactly when it matters.
func TestGatewayAndStationReasonsAreDistinct(t *testing.T) {
	t.Parallel()

	all := []RejectReason{
		ReasonCP24Time, ReasonUnknownType, ReasonMalformed, ReasonUnknownCommonAddress,
		ReasonStationUnknownType, ReasonStationUnknownCause,
		ReasonStationUnknownCommonAddress, ReasonStationUnknownIOA,
	}
	seen := make(map[RejectReason]bool, len(all))
	for _, r := range all {
		if seen[r] {
			t.Errorf("duplicate reject reason %q", r)
		}
		seen[r] = true
	}

	if ReasonUnknownType == ReasonStationUnknownType {
		t.Error("\"we cannot decode it\" and \"the station cannot understand it\" must not share a label")
	}
	if ReasonUnknownCommonAddress == ReasonStationUnknownCommonAddress {
		t.Error("the two unknown-common-address reasons must not share a label")
	}

	// Metric label values: renaming one silently breaks every dashboard.
	pinned := map[RejectReason]string{
		ReasonStationUnknownType:          "station_unknown_type",
		ReasonStationUnknownCause:         "station_unknown_cause",
		ReasonStationUnknownCommonAddress: "station_unknown_common_address",
		ReasonStationUnknownIOA:           "station_unknown_ioa",
	}
	for got, want := range pinned {
		if string(got) != want {
			t.Errorf("reject reason changed: %q, want %q", got, want)
		}
	}
}

// --- command feedback -------------------------------------------------------

// 11 and 12 are the difference between "the breaker opened" and "the breaker
// opened because someone opened it".
func TestCommandFeedbackCauses(t *testing.T) {
	t.Parallel()

	for _, c := range []Cause{CauseRetRem, CauseRetLoc} {
		if !c.IsCommandFeedback() {
			t.Errorf("%s must be command feedback", c)
		}
	}
	for _, c := range []Cause{
		CauseSpontaneous, CausePeriodic, CauseActCon, CauseActTerm,
		CauseIntroGeneral, CauseUnknownType,
	} {
		if c.IsCommandFeedback() {
			t.Errorf("%s must not be command feedback", c)
		}
	}
}

// --- change reports ---------------------------------------------------------

// The event plane publishes exactly these three causes. Every one of the 64
// values the six-bit field can hold is checked, not a sample: the dangerous
// mistake is not misreading 3, it is a range check that one day also catches
// 20, and a sample would only notice if it happened to include the value.
func TestOnlyThreeCausesAreChangeReports(t *testing.T) {
	t.Parallel()

	want := map[Cause]bool{
		CauseSpontaneous: true,
		CauseRetRem:      true,
		CauseRetLoc:      true,
	}
	for c := Cause(0); c < 64; c++ {
		if got := c.IsChangeReport(); got != want[c] {
			t.Errorf("%s (%d): IsChangeReport = %v, want %v", c, uint8(c), got, want[c])
		}
	}
}

// An interrogation response and a change report are never the same thing. This
// is the reconnect-replays-every-point bug stated as a property.
func TestNoInterrogationCauseIsAChangeReport(t *testing.T) {
	t.Parallel()

	for c := Cause(0); c < 64; c++ {
		if c.IsInterrogation() && c.IsChangeReport() {
			t.Errorf("%s is both an interrogation response and a change report", c)
		}
	}
}

func TestIsConfirmation(t *testing.T) {
	t.Parallel()

	for _, c := range []Cause{CauseActCon, CauseDeactCon, CauseActTerm, CauseUnknownType, CauseUnknownIOA} {
		if !c.IsConfirmation() {
			t.Errorf("%s must be a confirmation", c)
		}
	}
	for _, c := range []Cause{CauseSpontaneous, CausePeriodic, CauseActivation, CauseIntroGeneral, CauseRetRem} {
		if c.IsConfirmation() {
			t.Errorf("%s must not be a confirmation", c)
		}
	}
}

// --- the cause reaches every point ------------------------------------------

// The cause is a header field, shared by every object in the frame. A caller
// that has to fetch it separately is a caller that will one day forget, and
// forgetting loses no value — only the answer to "why".
func TestCauseReachesEveryPoint(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	cases := []struct {
		name  string
		cause Cause
		neg   bool
		test  bool
	}{
		{"spontaneous", CauseSpontaneous, false, false},
		{"interrogation response", CauseIntroGeneral, false, false},
		{"changed by a remote command", CauseRetRem, false, false},
		{"changed by a local operator", CauseRetLoc, false, false},
		{"periodic with the test bit", CausePeriodic, false, true},
		{"a refused activation", CauseActCon, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := Header{
				Type: MMeNb1, Count: 3, Cause: tc.cause,
				Negative: tc.neg, Test: tc.test, CommonAddr: 1,
			}
			parsed, body := buildASDU(t, p, h,
				"64 00 00  0A 00 00   65 00 00  14 00 00   66 00 00  1E 00 00")

			points, err := p.ParseMonitor(parsed, body)
			if err != nil {
				t.Fatalf("ParseMonitor: %v", err)
			}
			if len(points) != 3 {
				t.Fatalf("%d points, want 3", len(points))
			}
			for i, pt := range points {
				if pt.Cause != tc.cause {
					t.Errorf("point %d: cause = %s, want %s", i, pt.Cause, tc.cause)
				}
				if pt.Negative != tc.neg {
					t.Errorf("point %d: Negative = %v, want %v", i, pt.Negative, tc.neg)
				}
				if pt.Test != tc.test {
					t.Errorf("point %d: Test = %v, want %v", i, pt.Test, tc.test)
				}
			}
		})
	}
}

// Interrogation responses refresh the cache but are not events. Being able to
// tell them apart from spontaneous reports, per point, is what stops a reconnect
// from replaying the whole point list into the event stream.
func TestPointCauseDistinguishesInterrogationFromSpontaneous(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	for _, tc := range []struct {
		cause           Cause
		isInterrogation bool
	}{
		{CauseSpontaneous, false},
		{CauseIntroGeneral, true},
		{Cause(25), true}, // group 5
		{CausePeriodic, false},
		{CauseRetRem, false},
	} {
		h := Header{Type: MSpNa1, Count: 1, Cause: tc.cause, CommonAddr: 1}
		parsed, body := buildASDU(t, p, h, "64 00 00  01")

		points, err := p.ParseMonitor(parsed, body)
		if err != nil {
			t.Fatalf("%s: %v", tc.cause, err)
		}
		if got := points[0].Cause.IsInterrogation(); got != tc.isInterrogation {
			t.Errorf("%s: IsInterrogation = %v, want %v", tc.cause, got, tc.isInterrogation)
		}
	}
}
