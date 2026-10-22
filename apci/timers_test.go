package apci

import (
	"testing"
	"time"
)

// base is an arbitrary fixed instant. Everything in these tests is expressed as
// an offset from it, so a failure reads as "at t+16s" rather than as a wall
// clock time.
var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return base.Add(d) }

// newTestTimers returns started timers on the standard parameters:
// t1=15s, t2=10s, t3=20s.
func newTestTimers(t *testing.T) *Timers {
	t.Helper()
	p := DefaultParams()
	if err := p.Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	tm := NewTimers(p)
	tm.Start(base)
	return tm
}

func assertCheck(t *testing.T, tm *Timers, when time.Duration, want Action) {
	t.Helper()
	got, _ := tm.Check(at(when))
	if got != want {
		t.Fatalf("at t+%s: got %s, want %s", when, got, want)
	}
}

// --- t1: unacknowledged frames close the connection -------------------------

func TestT1ClosesWhenAnIFrameIsNotAcknowledged(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentI(base)

	assertCheck(t, tm, 14*time.Second, ActionNone)
	// Exactly at the deadline, not a nanosecond after: an implementation that
	// waits for "strictly past" drifts by one tick on every timer.
	assertCheck(t, tm, 15*time.Second, ActionClose)
}

func TestT1StopsWhenEverythingIsAcknowledged(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentI(base)
	tm.ReceivedS(at(5*time.Second), 0) // nothing left outstanding

	assertCheck(t, tm, 30*time.Second, ActionSendTestFR) // t3, not t1
}

// A partial acknowledgement restarts t1 rather than stopping it: frames are
// still in flight and still need answering.
func TestT1RestartsOnPartialAck(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentI(base)
	tm.SentI(base)
	tm.ReceivedS(at(10*time.Second), 1) // one of the two acknowledged

	// The original deadline would have been t+15s; the restart pushes it to
	// t+25s.
	assertCheck(t, tm, 15*time.Second, ActionNone)
	assertCheck(t, tm, 25*time.Second, ActionClose)
}

// t1 measures the OLDEST unacknowledged frame. A sender that keeps sending must
// not be able to hold a dead peer open by arming a fresh deadline each time.
func TestT1IsNotRestartedByFurtherSends(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentI(base)
	for i := 1; i <= 10; i++ {
		tm.SentI(at(time.Duration(i) * time.Second))
	}

	// Still the deadline set by the first frame.
	assertCheck(t, tm, 15*time.Second, ActionClose)
}

// A U activation expects a confirmation, so it is covered by t1 too. This is
// what makes TESTFR a probe rather than decoration: a link that has silently
// died is closed one t1 after the probe.
func TestT1CoversUActivations(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentU(base, StartDTAct)

	assertCheck(t, tm, 15*time.Second, ActionClose)
}

func TestUConfirmationClearsT1(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentU(base, TestFRAct)
	tm.ReceivedU(at(time.Second), TestFRCon)

	assertCheck(t, tm, 15*time.Second, ActionNone)
}

// A confirmation this side sends answers the peer's activation and expects
// nothing back, so it must not arm t1.
func TestSentUConfirmationDoesNotArmT1(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentU(base, StartDTCon)

	assertCheck(t, tm, 15*time.Second, ActionNone)
}

// --- t2: received frames must be acknowledged -------------------------------

func TestT2MakesAnAcknowledgementDue(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.ReceivedI(base, 0)

	assertCheck(t, tm, 9*time.Second, ActionNone)
	assertCheck(t, tm, 10*time.Second, ActionSendSupervisory)
}

// t2 measures from the FIRST unacknowledged frame. Restarting it on every
// arrival would let a steady trickle go unacknowledged forever, and the peer
// would time us out on its own t1.
func TestT2MeasuresFromTheFirstUnacknowledgedFrame(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	for i := range 9 {
		tm.ReceivedI(at(time.Duration(i)*time.Second), 0)
	}

	// Nine frames, one per second, and the deadline is still the one the first
	// frame set.
	assertCheck(t, tm, 10*time.Second, ActionSendSupervisory)
}

func TestSendingClearsT2(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		send func(tm *Timers, now time.Time)
	}{
		{"an S-frame", func(tm *Timers, now time.Time) { tm.SentS(now) }},
		// An I-frame carries N(R), so it acknowledges as a side effect. This is
		// why a busy two-way link almost never needs a standalone S-frame.
		{"an I-frame, which carries N(R)", func(tm *Timers, now time.Time) { tm.SentI(now) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tm := newTestTimers(t)
			tm.ReceivedI(base, 0)
			tc.send(tm, at(5*time.Second))

			if got, _ := tm.Check(at(10 * time.Second)); got == ActionSendSupervisory {
				t.Fatal("t2 must be disarmed once the acknowledgement has gone out")
			}
		})
	}
}

// --- t3: the idle probe -----------------------------------------------------

func TestT3ProbesAnIdleLink(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)

	assertCheck(t, tm, 19*time.Second, ActionNone)
	assertCheck(t, tm, 20*time.Second, ActionSendTestFR)
}

// Traffic in either direction proves the link is alive, which is the only thing
// t3 asks about.
func TestT3IsNotDueWhileThereIsTraffic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		beat func(tm *Timers, now time.Time)
	}{
		{"outgoing I-frames", func(tm *Timers, now time.Time) { tm.SentI(now) }},
		{"outgoing S-frames", func(tm *Timers, now time.Time) { tm.SentS(now) }},
		{"incoming I-frames", func(tm *Timers, now time.Time) { tm.ReceivedI(now, 0) }},
		{"incoming S-frames", func(tm *Timers, now time.Time) { tm.ReceivedS(now, 0) }},
		{"incoming U confirmations", func(tm *Timers, now time.Time) { tm.ReceivedU(now, TestFRCon) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tm := newTestTimers(t)
			// Beat every 5 seconds for well past t3, acknowledging as we go so
			// that t1 and t2 stay out of the way.
			for i := 1; i <= 20; i++ {
				now := at(time.Duration(i) * 5 * time.Second)
				tc.beat(tm, now)
				tm.SentS(now)
				tm.ReceivedS(now, 0)

				if got, _ := tm.Check(now); got == ActionSendTestFR {
					t.Fatalf("at t+%s: TESTFR must not be due on a busy link", now.Sub(base))
				}
			}
		})
	}
}

// The probe itself is covered by t1: a link that has silently died is closed one
// t1 after the TESTFR act goes out.
func TestAnUnansweredProbeClosesTheConnection(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)

	assertCheck(t, tm, 20*time.Second, ActionSendTestFR)
	tm.SentU(at(20*time.Second), TestFRAct)

	assertCheck(t, tm, 34*time.Second, ActionNone)
	assertCheck(t, tm, 35*time.Second, ActionClose)
}

// --- priority ---------------------------------------------------------------

// Closing wins over anything else: a connection about to be closed should not
// first be asked to send frames on it.
func TestClosingOutranksEveryOtherAction(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentI(base)        // t1 at +15s
	tm.ReceivedI(base, 1) // t2 at +10s, and t1 restarted at +15s

	assertCheck(t, tm, 10*time.Second, ActionSendSupervisory)
	assertCheck(t, tm, 15*time.Second, ActionClose)
	// Still close, long after both are due.
	assertCheck(t, tm, time.Hour, ActionClose)
}

// --- the reported deadline --------------------------------------------------

func TestCheckReportsTheNearestDeadline(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t) // t3 at +20s

	if _, wait := tm.Check(base); wait != 20*time.Second {
		t.Errorf("with only t3 armed: wait = %s, want 20s", wait)
	}

	tm.ReceivedI(base, 0) // t2 at +10s, t3 reset to +20s
	if _, wait := tm.Check(base); wait != 10*time.Second {
		t.Errorf("with t2 armed: wait = %s, want 10s", wait)
	}

	tm.SentI(base) // t1 at +15s, t2 cleared, t3 reset
	if _, wait := tm.Check(base); wait != 15*time.Second {
		t.Errorf("with t1 and t3 armed: wait = %s, want 15s", wait)
	}

	// Part way through, the remaining wait shrinks.
	if _, wait := tm.Check(at(5 * time.Second)); wait != 10*time.Second {
		t.Errorf("five seconds in: wait = %s, want 10s", wait)
	}
}

// Deadlines, not intervals: a loop that wakes late still fires at the right
// moment rather than a full interval after waking.
func TestALateWakeUpDoesNotPushTheDeadlineForward(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.ReceivedI(base, 0) // t2 at +10s

	// The loop oversleeps to +25s. The action is due, not rescheduled.
	assertCheck(t, tm, 25*time.Second, ActionSendSupervisory)
}

// --- lifecycle --------------------------------------------------------------

func TestNothingIsArmedBeforeStart(t *testing.T) {
	t.Parallel()

	tm := NewTimers(DefaultParams())

	got, wait := tm.Check(at(time.Hour))
	if got != ActionNone {
		t.Errorf("got %s, want none", got)
	}
	if wait != 0 {
		t.Errorf("wait = %s, want 0 with nothing armed", wait)
	}
}

func TestStopDisarmsEverything(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)
	tm.SentI(base)
	tm.ReceivedI(base, 1)
	tm.Stop()

	got, wait := tm.Check(at(time.Hour))
	if got != ActionNone {
		t.Errorf("got %s, want none after Stop", got)
	}
	if wait != 0 {
		t.Errorf("wait = %s, want 0 after Stop", wait)
	}
}

// --- a whole quiet connection ----------------------------------------------

// The sequence a real idle station goes through: handshake, silence, probe,
// answer, silence, probe. Nothing should close.
func TestIdleConnectionSurvivesOnProbesAlone(t *testing.T) {
	t.Parallel()

	tm := newTestTimers(t)

	// Handshake.
	tm.SentU(base, StartDTAct)
	tm.ReceivedU(at(100*time.Millisecond), StartDTCon)

	now := 100 * time.Millisecond
	for range 50 {
		// Nothing is due until t3.
		assertCheck(t, tm, now+19*time.Second, ActionNone)

		probeAt := now + 20*time.Second
		assertCheck(t, tm, probeAt, ActionSendTestFR)
		tm.SentU(at(probeAt), TestFRAct)

		answerAt := probeAt + 200*time.Millisecond
		tm.ReceivedU(at(answerAt), TestFRCon)
		assertCheck(t, tm, answerAt, ActionNone)

		now = answerAt
	}
}
