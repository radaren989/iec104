package station

import (
	"errors"
	"testing"
	"time"

	"github.com/radaren989/iec104/clock"
)

func TestGIValidateRejectsWhatCannotWork(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		gi   GI
		want bool // want an error
	}{
		{"the defaults", DefaultGI(), false},
		{"zero timeout", GI{Timeout: 0}, true},
		{"negative timeout", GI{Timeout: -time.Second}, true},
		{"negative period", GI{Timeout: time.Minute, Period: -time.Second}, true},
		{"period disabled", GI{Timeout: time.Minute, Period: 0}, false},
		{"period comfortably beyond the timeout", GI{Timeout: time.Minute, Period: time.Hour}, false},
		// A period inside the timeout means the next interrogation falls due
		// before the previous one has been given up on, and two would overlap
		// on a station that is merely slow.
		{"period inside the timeout", GI{Timeout: time.Minute, Period: 30 * time.Second}, true},
		{"period equal to the timeout", GI{Timeout: time.Minute, Period: time.Minute}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.gi.Validate()
			if (err != nil) != tc.want {
				t.Fatalf("Validate() = %v, want error: %v", err, tc.want)
			}
			if err != nil && !errors.Is(err, ErrConfig) {
				t.Errorf("%v does not wrap ErrConfig", err)
			}
		})
	}
}

// The bracket is the cold-start gate (Study Reference §10). A station is topical
// when the actterm arrives and not one frame earlier: until then the cache holds
// whatever subset has turned up, which looks complete and is not.
func TestAStationIsTopicalOnlyAfterTheBracketCloses(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	// Interrogating came first, and it is not topical.
	var sawInterrogating bool
	for _, m := range rec.snapshot() {
		if m.to == StateInterrogating {
			sawInterrogating = true
		}
		if m.to == StateTopical && !sawInterrogating {
			t.Fatal("topical without ever interrogating")
		}
	}
	if !sawInterrogating {
		t.Fatal("never interrogated")
	}

	if reports := rec.giReports(); len(reports) != 1 {
		t.Fatalf("%d interrogations reported, want 1", len(reports))
	} else if reports[0].err != nil {
		t.Errorf("the interrogation reported %v", reports[0].err)
	}
}

// A station that confirms the interrogation and never terminates it is the worst
// case: the connection is healthy, frames may even be arriving, and the cache is
// a subset of unknown size. The timeout is what stops that from looking fine.
func TestAnInterrogationThatNeverTerminatesFailsTheStation(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	o := newOutstation(t)
	o.giNoActTerm = true

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.Clock = fake
	cfg.Backoff = Backoff{Base: time.Second, Cap: time.Minute, Factor: 2}
	// Set explicitly rather than relying on the default: New takes its Config
	// by value, so the defaults it fills in are invisible here and cfg.GI.Timeout
	// would still read zero. A test that advanced by "the timeout" would then
	// advance by a second and wait for something that had not come due.
	cfg.GI = GI{OnConnect: true, Timeout: time.Minute}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateInterrogating)

	// Two timers: the connection's t1/t2/t3 timer and the interrogation's
	// timeout. Waiting for the second to exist is what stops the Advance below
	// from landing before the deadline it is meant to pass.
	fake.BlockUntilAtLeast(2)
	fake.Advance(cfg.GI.Timeout + time.Second)

	rec.waitFor(t, StateFailed)

	for _, m := range rec.snapshot() {
		if m.to != StateFailed {
			continue
		}
		if !errors.Is(m.reason, ErrGITimeout) {
			t.Errorf("failed with %v, want ErrGITimeout", m.reason)
		}
		break
	}

	reports := rec.giReports()
	if len(reports) == 0 {
		t.Fatal("the timed-out interrogation was not reported")
	}
	if !errors.Is(reports[len(reports)-1].err, ErrGITimeout) {
		t.Errorf("OnGI reported %v, want ErrGITimeout", reports[len(reports)-1].err)
	}
}

// A station that answers with P/N set understands the question and is declining
// it, which nearly always means the common address is wrong. That is a
// configuration fault and it must be visible as one, not as a timeout an hour
// later.
func TestARefusedInterrogationFailsImmediately(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.giRefuse = true

	rec := newRecorder()
	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateFailed)

	for _, m := range rec.snapshot() {
		if m.to != StateFailed {
			continue
		}
		if !errors.Is(m.reason, ErrGIRefused) {
			t.Errorf("failed with %v, want ErrGIRefused", m.reason)
		}
		return
	}
}

// M_EI_NA_1 means the station has restarted. Everything cached from before it
// describes a different life of the device — a value that looks current and
// belongs to a machine that has since been power-cycled.
func TestARestartAnnouncementTriggersAnotherInterrogation(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)
	if n := o.interrogations(); n != 1 {
		t.Fatalf("%d interrogations before the restart, want 1", n)
	}

	o.announceRestart()

	waitCount(t, rec, StateTopical, 2)
	if n := o.interrogations(); n != 2 {
		t.Errorf("%d interrogations after the restart, want 2", n)
	}

	// And it went back through Interrogating, so a reader asking "is this
	// current" during the second pass gets the right answer.
	if n := countState(rec, StateInterrogating); n != 2 {
		t.Errorf("entered interrogating %d times, want 2", n)
	}
}

func TestPeriodicInterrogationRepeatsOnTheClock(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	o := newOutstation(t)

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.Clock = fake
	cfg.GI = GI{OnConnect: true, Period: time.Hour, Timeout: time.Minute}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	// Two timers once the bracket has closed: the connection's, and the
	// periodic interrogation's. The interrogation timeout has been stopped.
	fake.BlockUntilAtLeast(2)
	fake.Advance(30 * time.Minute)
	if n := o.interrogations(); n != 1 {
		t.Fatalf("%d interrogations half way through the period, want 1", n)
	}

	fake.Advance(31 * time.Minute)
	waitCount(t, rec, StateTopical, 2)
	if n := o.interrogations(); n != 2 {
		t.Errorf("%d interrogations after one period, want 2", n)
	}
}

// Off is the default and should stay off: a periodic interrogation on a thousand
// stations is a thousand full snapshots, and the point of 104 is that you do not
// need them.
func TestPeriodicInterrogationIsOffByDefault(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	o := newOutstation(t)

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.Clock = fake

	if cfg.GI.Period != 0 {
		t.Fatalf("the default period is %s, want 0", cfg.GI.Period)
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	fake.BlockUntilAtLeast(2)
	fake.Advance(365 * 24 * time.Hour)

	if n := o.interrogations(); n != 1 {
		t.Errorf("%d interrogations after a year with the period off, want 1", n)
	}
}

// With gi_on_connect off, a connected station never becomes topical on its own.
// That is the honest outcome rather than a bug: nothing has been interrogated,
// so nothing is current, and a reader must be told so.
func TestWithoutGIOnConnectAStationStopsAtStarted(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	cfg := testConfig(t, o.addr(), rec)
	cfg.GI = GI{OnConnect: false, Timeout: time.Minute}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateStarted)

	//nolint:forbidigo // Proving that something does NOT happen needs a window
	// in which it could have. There is no event to wait on for "no
	// interrogation was sent".
	time.Sleep(100 * time.Millisecond)

	if n := o.interrogations(); n != 0 {
		t.Errorf("%d interrogations with gi_on_connect off, want 0", n)
	}
	if s.State().Topical() {
		t.Error("a station that was never interrogated reports itself topical")
	}
}

// A connection that dies mid-bracket must fail at once. Without this the
// interrogation sits out its whole timeout waiting for frames from a socket that
// is already gone — a minute of reporting "interrogating" while disconnected,
// and a minute added to every reconnection.
func TestALinkThatDiesMidInterrogationFailsAtOnce(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.giSilent = true // confirm nothing, so the bracket stays open

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.GI = GI{OnConnect: true, Timeout: time.Hour} // far longer than the test

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateInterrogating)
	o.dropAll()

	// The timeout is an hour and this must not wait for it.
	rec.waitFor(t, StateFailed)
}

// countState is how many times the station entered a state.
func countState(r *recorder, want State) int { return r.countReached(want) }
