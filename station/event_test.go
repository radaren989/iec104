package station

import (
	"testing"

	"github.com/radaren989/iec104/asdu"
)

// barrierIOA is the address of the spontaneous measurand a test sends after the
// frame it is really asking about. Its event arriving proves the earlier frame
// has been handled (recorder.waitForEvent), which is what makes "no event" an
// observation rather than a guess.
const barrierIOA asdu.IOA = 999

// The acceptance test of T7.1. A reconnect interrogates the whole station, and
// if its answers were events every link flap would send one alarm per point.
func TestAnInterrogationPublishesNoEvents(t *testing.T) {
	t.Parallel()

	const points = 50

	o := newOutstation(t)
	o.giPoints = points
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	// Topical is reached on the termination, which the device sends after the
	// last interrogated point, and frames are handled in order: by now every
	// point has been through the same code path an event would have taken.
	rec.waitFor(t, StateTopical)

	ioas := make([]asdu.IOA, points)
	for i := range ioas {
		ioas[i] = asdu.IOA(i + 1)
	}
	found, missing := s.ReadPoints(ioas)
	if len(found) != points {
		// Without the points in the cache the assertion below would pass for
		// the wrong reason: nothing arrived, so nothing was published.
		t.Fatalf("%d interrogated points cached, want %d (missing %v)", len(found), points, missing)
	}

	if events := rec.eventReports(); len(events) != 0 {
		t.Errorf("an interrogation produced %d event(s), want none; first: %+v", len(events), events[0])
	}
}

// The rule is about the cause, not about whether an interrogation is running. A
// value that changes while the device is answering one is a real change, and
// muting events for the duration would lose an alarm on every reconnect.
func TestAChangeDuringAnInterrogationIsStillAnEvent(t *testing.T) {
	t.Parallel()

	const changed asdu.IOA = 1000

	o := newOutstation(t)
	o.giPoints = 20
	o.giSpontaneous = changed
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	events := rec.eventReports()
	if len(events) != 1 {
		t.Fatalf("%d event(s), want exactly the one spontaneous change: %+v", len(events), events)
	}
	if events[0].Point.IOA != changed || events[0].Point.Cause != asdu.CauseSpontaneous {
		t.Errorf("event = IOA %d cause %s, want IOA %d cause %s",
			events[0].Point.IOA, events[0].Point.Cause, changed, asdu.CauseSpontaneous)
	}
}

// Which readings become events, one cause at a time, through a live station.
//
// Each case checks the probe reached the cache before checking whether it became
// an event, so a frame that failed to parse cannot pass as a filtered one. The
// cause carried by an event is asserted too: DeviceManager builds _Cause from it,
// and 11 against 12 is the difference between "we opened the breaker" and "a
// local operator did".
func TestOnlyChangeReportsBecomeEvents(t *testing.T) {
	t.Parallel()

	const probe asdu.IOA = 300

	cases := []struct {
		name  string
		cause asdu.Cause
		test  bool // the T bit
		event bool
	}{
		{"spontaneous", asdu.CauseSpontaneous, false, true},
		{"changed by a remote command", asdu.CauseRetRem, false, true},
		{"changed by a local operator", asdu.CauseRetLoc, false, true},
		{"periodic", asdu.CausePeriodic, false, false},
		{"background scan", asdu.CauseBackground, false, false},
		{"answer to a read", asdu.CauseRequest, false, false},
		// No interrogation is open here: this is a device repeating its answer
		// unasked, the Freyr simulator's habit. Still not an event.
		{"general interrogation, unsolicited", asdu.CauseIntroGeneral, false, false},
		{"group interrogation, unsolicited", asdu.CauseIntroGroup1 + 4, false, false},
		{"spontaneous test frame", asdu.CauseSpontaneous, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := newOutstation(t)
			rec := newRecorder()

			s, err := New(testConfig(t, o.addr(), rec))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Release() })

			rec.waitFor(t, StateTopical)

			body, err := measurandWithCause(probe, 1.5, tc.cause)
			if err != nil {
				t.Fatalf("encoding the probe: %v", err)
			}
			if tc.test {
				body = withTestBit(body)
			}
			o.sendRaw(body)
			o.sendMeasurand(barrierIOA, 2.5)
			rec.waitForEvent(t, barrierIOA)

			if _, ok := s.ReadPoint(probe); !ok {
				t.Fatalf("the probe never reached the cache, so its missing event proves nothing")
			}

			var got []Event
			for _, e := range rec.eventReports() {
				if e.Point.IOA == probe {
					got = append(got, e)
				}
			}

			if !tc.event {
				if len(got) != 0 {
					t.Errorf("a reading with cause %s (test=%v) became an event", tc.cause, tc.test)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("%d event(s) for the probe, want 1", len(got))
			}
			if got[0].Point.Cause != tc.cause {
				t.Errorf("event cause = %s, want %s", got[0].Point.Cause, tc.cause)
			}
			if got[0].Point.Float != 1.5 {
				t.Errorf("event value = %v, want 1.5", got[0].Point.Float)
			}
		})
	}
}
