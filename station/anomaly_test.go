package station

import (
	"strings"
	"testing"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// These tests are the record of a real morning's work, 2026-09-28.
//
// A Freyr simulator was driving the station perfectly happily, and the only
// reason to look closer was that its act-con and act-term counts did not match.
// They did not match because it sends the bracket backwards — the termination
// before the confirmation — and repeats the whole response several times a
// second without being asked. The station coped by accident: it discarded the
// early termination, waited, and picked up a confirmation from the NEXT
// repetition. A reference outstation written separately in Python
// (test/sim/outstation.py) settled whose bug it was — against a conformant
// bracket the same station exchanged 3 confirmations and 3 terminations and
// finished an interrogation in 20ms, against the same device's ordering WITHOUT
// the repetition it timed out for sixty seconds and reconnected for ever.
//
// So the tolerance was never protecting anyone. It was hiding the one piece of
// information that would have made the field failure diagnosable in a minute
// instead of a day.

// --- out-of-order bracket ----------------------------------------------------

func TestATerminationBeforeAConfirmationIsReported(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.giReversed = true // act-term, then act-con — what Freyr does

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.GI = GI{OnConnect: true, Timeout: 5 * time.Second}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	// The station still gets there: the outstation repeats itself, so a
	// confirmation does arrive. That is the accident this test pins down.
	rec.waitFor(t, StateTopical)

	if n := rec.anomalyCount(AnomalyBracketOutOfOrder); n == 0 {
		t.Fatal("the bracket arrived backwards and nothing said so; " +
			"a device that does this ONCE leaves the station timing out for ever " +
			"with no clue anywhere as to why")
	}

	// The detail is what an operator reads at three in the morning, so it has
	// to say what happened rather than name a constant.
	rec.mu.Lock()
	detail := rec.anomalies[0].detail
	rec.mu.Unlock()
	if !strings.Contains(detail, "act-term") || !strings.Contains(detail, "act-con") {
		t.Errorf("detail %q names neither half of the bracket", detail)
	}
}

// The other half of the rule: a device that merely repeats itself in the right
// order is doing nothing wrong, and crying wolf about it would make the signal
// worthless.
func TestARepeatedConfirmationIsNotAnAnomaly(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.giExtraActCon = true // act-con, act-con, data, act-term — legal

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.GI = GI{OnConnect: true, Timeout: 5 * time.Second}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	if n := rec.anomalyCount(AnomalyBracketOutOfOrder); n != 0 {
		t.Errorf("%d out-of-order reports for a device that repeated a confirmation "+
			"in the correct order; this signal is only worth having if it is quiet "+
			"when nothing is wrong", n)
	}
}

// --- unsolicited interrogation traffic ---------------------------------------

func TestInterrogationDataWithNoInterrogationOutstandingIsReported(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	cfg := testConfig(t, o.addr(), rec)
	// No interrogation on connect, so anything with an interrogation cause is
	// unambiguously unrequested rather than merely late.
	cfg.GI = GI{OnConnect: false, Timeout: 5 * time.Second}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	// Started, not topical: without an interrogation on connect nothing ever
	// declares the snapshot complete, which is exactly the state in which any
	// interrogation traffic is unambiguously unrequested.
	rec.waitFor(t, StateStarted)

	o.sendInterrogated(asdu.IOA(1), 42.0)
	waitCached(t, s, asdu.IOA(1))

	if n := rec.anomalyCount(AnomalyUnsolicitedInterrogation); n == 0 {
		t.Error("interrogation data arrived with no interrogation outstanding and " +
			"nothing said so; a device looping on its own is how a link fills with " +
			"traffic nobody ordered")
	}

	// The reading is kept. It is real, current data from the device, and
	// dropping it to make a point about the specification would lose a value an
	// operator needs.
	if _, ok := s.ReadPoint(asdu.IOA(1)); !ok {
		t.Error("the point was not cached; the anomaly is the announcement, not the measurement")
	}
}

func TestDataDuringAnInterrogationIsNotAnAnomaly(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.giPoints = 3 // stream three interrogated points inside the bracket

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.GI = GI{OnConnect: true, Timeout: 5 * time.Second}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	if n := rec.anomalyCount(AnomalyUnsolicitedInterrogation); n != 0 {
		t.Errorf("%d unsolicited reports for data that arrived inside the bracket "+
			"we opened ourselves", n)
	}
}

// --- the label set ------------------------------------------------------------

// Every kind has to be a usable metric label: low cardinality, stable spelling,
// no spaces. A renamed or prettified label silently breaks every dashboard built
// on it, which is the kind of breakage nobody notices until an alert does not fire.
func TestEveryAnomalyKindIsAUsableMetricLabel(t *testing.T) {
	t.Parallel()

	seen := make(map[AnomalyKind]bool, len(anomalyKinds))
	for _, k := range anomalyKinds {
		if k == "" {
			t.Error("an anomaly kind is empty")
			continue
		}
		if seen[k] {
			t.Errorf("%q is listed twice", k)
		}
		seen[k] = true

		if strings.ToLower(string(k)) != string(k) {
			t.Errorf("%q is not lower case", k)
		}
		if strings.ContainsAny(string(k), " \t\n\"{}") {
			t.Errorf("%q contains a character that has no business in a label", k)
		}
	}
}
