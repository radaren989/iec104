package station

import (
	"testing"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// This file is T6.9: the whole life of a station, round and round.
//
// The failure it exists to catch does not show up in a single cycle. A
// connection that leaks one goroutine, or a cache that keeps one generation of
// points too many, looks perfectly healthy the first time and takes the process
// down after a week on a site whose link flaps. At a thousand stations the
// arithmetic is unforgiving: one leaked goroutine per reconnection, on a fleet
// where a handful of stations flap every minute, is thousands of goroutines a
// day.

// The full cycle, a hundred times: connect, handshake, interrogate, become
// topical, lose the link, back off, reconnect, interrogate again.
func TestAHundredReconnectionsLeakNothing(t *testing.T) {
	// Not parallel: it counts goroutines process-wide.

	const cycles = 100

	o := newOutstation(t)
	rec := newRecorder()

	cfg := testConfig(t, o.addr(), rec)
	// A backoff short enough to run a hundred cycles in a test, with the same
	// shape as the real one. The schedule itself is tested in backoff_test.go;
	// what is under test here is that going round it repeatedly costs nothing.
	cfg.Backoff = Backoff{Base: time.Millisecond, Cap: 5 * time.Millisecond, Factor: 2}

	before := settle(0)

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 1; i <= cycles; i++ {
		waitCount(t, rec, StateTopical, i)

		// The link dies the way links die: the far end goes away.
		o.dropAll()
		waitCount(t, rec, StateFailed, i)
	}

	// Every cycle really did the whole thing, rather than the station finding
	// some shortcut back to topical.
	if n := o.handshakes(); n < cycles {
		t.Errorf("%d handshakes over %d cycles; 104 has no session to resume", n, cycles)
	}
	if n := o.interrogations(); n < cycles {
		t.Errorf("%d interrogations over %d cycles; a reconnected station must re-interrogate",
			n, cycles)
	}

	// settle rather than a bare count: a hundred sockets have just been torn
	// down on both sides, and the goroutines that served them exit when the
	// scheduler gets to them, not when the last drop returns. A real leak
	// cannot be waited away, so this weakens nothing.
	if during := settle(before + 12); during > before+12 {
		t.Errorf("goroutines went from %d to %d over %d reconnections", before, during, cycles)
	}

	// The cache must not grow across reconnections either. This is the memory
	// half of the criterion, asserted as a COUNT rather than as heap bytes:
	// heap bytes move for reasons that have nothing to do with this code — the
	// allocator, the test framework, whatever else the package is running — so
	// a byte assertion would either be so loose it proved nothing or so tight
	// it failed on a Tuesday. The count is exact, and it is the thing that
	// would actually grow.
	if n := s.cache.size(); n != 0 {
		t.Errorf("%d points cached from a station that never sent any", n)
	}

	if err := s.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if after := settle(before); after > before+3 {
		t.Errorf("goroutines went from %d to %d after release", before, after)
	}
}

// The same cycle with data flowing, because a cache that keeps a generation per
// interrogation is a leak the empty-station test above cannot see.
func TestRepeatedInterrogationsDoNotGrowTheCache(t *testing.T) {
	t.Parallel()

	const (
		cycles = 20
		points = 3
	)

	o := newOutstation(t)
	rec := newRecorder()

	cfg := testConfig(t, o.addr(), rec)
	cfg.Backoff = Backoff{Base: time.Millisecond, Cap: 5 * time.Millisecond, Factor: 2}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	for i := 1; i <= cycles; i++ {
		waitCount(t, rec, StateTopical, i)

		// A different block of addresses each cycle, so that a cache which
		// failed to sweep would be holding sixty points by the end rather
		// than overwriting the same three. Re-sending the same addresses
		// would hide exactly the bug this test is for.
		for k := 1; k <= points; k++ {
			o.sendMeasurand(asdu.IOA(i*100+k), float32(i))
		}
		waitCached(t, s, asdu.IOA(i*100+points))

		o.dropAll()
		waitCount(t, rec, StateFailed, i)
	}

	// One more interrogation, so the assertion below is made on a settled
	// station rather than one halfway through a sweep.
	waitCount(t, rec, StateTopical, cycles+1)

	if n := s.cache.size(); n > points {
		t.Errorf("%d points after %d interrogations of a %d-point station; "+
			"the cache is keeping a generation per cycle", n, cycles, points)
	}
}

// Release must be safe at any point in the cycle, not only on a settled
// station. A registry releasing one that happens to be mid-dial or
// mid-interrogation is the ordinary case, not the edge one.
func TestReleaseIsSafeAtEveryPointInTheCycle(t *testing.T) {
	// Not parallel: it counts goroutines process-wide.

	o := newOutstation(t)
	before := settle(0)

	// Increasing delays, so the release lands somewhere different each time:
	// queued, dialling, handshaking, interrogating, topical.
	for _, d := range []time.Duration{
		0, 100 * time.Microsecond, time.Millisecond, 3 * time.Millisecond,
		10 * time.Millisecond, 30 * time.Millisecond, 100 * time.Millisecond,
	} {
		rec := newRecorder()
		s, err := New(testConfig(t, o.addr(), rec))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		// The point is to land the release at an arbitrary moment in the
		// cycle. There is no event to wait on for "somewhere in the middle",
		// and a fake clock would remove the very arbitrariness this is buying.
		//nolint:forbidigo // see above: the sleep is the test input, not a
		// synchronisation step.
		time.Sleep(d)

		if err := s.Release(); err != nil {
			t.Errorf("Release after %s: %v", d, err)
		}
		if got := s.State(); got != StateReleased {
			t.Errorf("state %s after Release at %s, want released", got, d)
		}
	}

	if after := settle(before); after > before+3 {
		t.Errorf("goroutines went from %d to %d after releasing at seven different moments",
			before, after)
	}
}
