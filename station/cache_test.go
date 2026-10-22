package station

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
)

func point(ioa asdu.IOA, v float64) Reading {
	return Reading{Point: asdu.Point{
		Type:  asdu.MMeNc1,
		IOA:   ioa,
		Kind:  asdu.KindFloat,
		Float: v,
		Cause: asdu.CauseSpontaneous,
	}}
}

// --- the copy promise -------------------------------------------------------

// The cache's central promise is that a reader cannot reach back into it through
// the value it was handed. That rests entirely on asdu.Point being made of value
// types: add a []byte or a map to it and every read starts sharing memory with
// the cache — nothing fails, the copies simply stop being copies, and a caller
// that modifies what it was given corrupts the station's view of the field.
func TestAReadingCanBeCopiedByAssignment(t *testing.T) {
	t.Parallel()

	if field, ok := pointHasOnlyValueFields(reflect.TypeOf(Reading{})); !ok {
		t.Fatalf("station.Reading contains a reference field: %s.\n"+
			"Every read from the cache would share memory with it, and a caller that "+
			"modified what it was given would corrupt the station's view of the field. "+
			"Either keep Point a value type, or make the cache copy that field by hand.", field)
	}
}

func TestAReadingCannotBeUsedToChangeTheCache(t *testing.T) {
	t.Parallel()

	c := newCache()
	c.store(point(100, 1.5))

	got, ok := c.read(100)
	if !ok {
		t.Fatal("the point was not stored")
	}

	// The caller does what callers do with a struct they were handed.
	got.Point.Float = 999
	got.Point.Q.Invalid = true
	got.Point.Cause = asdu.CauseActTerm
	got.SkewMillis = 12345

	again, _ := c.read(100)
	if again.Point.Float != 1.5 || again.Point.Q.Invalid ||
		again.Point.Cause != asdu.CauseSpontaneous || again.SkewMillis != 0 {
		t.Errorf("the cache changed to %+v; a reading must be a copy", again)
	}
}

func TestReadPointsReturnsCopiesToo(t *testing.T) {
	t.Parallel()

	c := newCache()
	c.store(point(1, 10))
	c.store(point(2, 20))

	found, _ := c.readMany([]asdu.IOA{1, 2})
	for i := range found {
		found[i].Point.Float = -1
	}

	for _, ioa := range []asdu.IOA{1, 2} {
		p, _ := c.read(ioa)
		if p.Point.Float < 0 {
			t.Errorf("point %d was changed through the slice returned to the caller", ioa)
		}
	}
}

// --- missing points ---------------------------------------------------------

// An answer that silently returned a hundred and ninety of two hundred requested
// addresses would be read as "the other ten are fine".
func TestMissingAddressesAreReportedNotOmitted(t *testing.T) {
	t.Parallel()

	c := newCache()
	c.store(point(1, 1))
	c.store(point(3, 3))

	found, missing := c.readMany([]asdu.IOA{1, 2, 3, 4})

	if len(found) != 2 {
		t.Errorf("found %d points, want 2", len(found))
	}
	want := []asdu.IOA{2, 4}
	if len(missing) != len(want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	for i, ioa := range want {
		if missing[i] != ioa {
			t.Errorf("missing[%d] = %d, want %d", i, missing[i], ioa)
		}
	}
}

func TestReadingNothingAsksForNothing(t *testing.T) {
	t.Parallel()

	c := newCache()
	c.store(point(1, 1))

	found, missing := c.readMany(nil)
	if found != nil || missing != nil {
		t.Errorf("readMany(nil) = %v, %v; want nothing", found, missing)
	}
}

// --- generations ------------------------------------------------------------

// A point removed from the device's configuration must not sit in the cache
// forever, served as part of a topical station, looking exactly like a real
// reading that happens not to have changed in a while.
func TestAnInterrogationDropsPointsTheDeviceNoLongerHas(t *testing.T) {
	t.Parallel()

	c := newCache()

	// First snapshot: three points.
	c.beginInterrogation()
	c.store(point(1, 1))
	c.store(point(2, 2))
	c.store(point(3, 3))
	c.endInterrogation()

	if got := c.size(); got != 3 {
		t.Fatalf("%d points after the first interrogation, want 3", got)
	}

	// Second snapshot: the device no longer reports point 2.
	c.beginInterrogation()
	c.store(point(1, 11))
	c.store(point(3, 33))
	dropped := c.endInterrogation()

	if dropped != 1 {
		t.Errorf("dropped %d points, want 1", dropped)
	}
	if _, ok := c.read(2); ok {
		t.Error("a point missing from a complete interrogation is still cached")
	}
	if p, _ := c.read(1); p.Point.Float != 11 {
		t.Errorf("point 1 = %v, want the refreshed 11", p.Point.Float)
	}
}

// A value that arrived by itself while the interrogation was running belongs to
// the current snapshot. Sweeping it would delete a reading that is newer than
// everything else in the cache.
func TestASpontaneousValueDuringAnInterrogationSurvivesTheSweep(t *testing.T) {
	t.Parallel()

	c := newCache()

	c.beginInterrogation()
	c.store(point(1, 1))
	c.endInterrogation()

	c.beginInterrogation()
	c.store(point(1, 11))
	c.store(point(9, 99)) // spontaneous, mid-bracket
	c.endInterrogation()

	if p, ok := c.read(9); !ok || p.Point.Float != 99 {
		t.Errorf("the spontaneous point was swept: %+v ok=%v", p, ok)
	}
}

// A timed-out interrogation collected a subset of unknown size. Treating the
// points it happened to miss as deleted would turn a slow station into one that
// appears to have lost half its configuration.
func TestATimedOutInterrogationDoesNotSweep(t *testing.T) {
	t.Parallel()

	c := newCache()

	c.beginInterrogation()
	c.store(point(1, 1))
	c.store(point(2, 2))
	c.endInterrogation()

	// A second interrogation starts and never finishes: no endInterrogation.
	c.beginInterrogation()
	c.store(point(1, 11))

	if got := c.size(); got != 2 {
		t.Errorf("%d points held while an interrogation is in flight, want 2", got)
	}
	if _, ok := c.read(2); !ok {
		t.Error("a point was dropped by an interrogation that never terminated")
	}
}

// --- through a live station -------------------------------------------------

func TestAStationCachesWhatItIsToldAndReportsMissing(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	o.sendMeasurand(200, 42.5)
	waitCached(t, s, 200)

	found, missing := s.ReadPoints([]asdu.IOA{200, 201})
	if len(found) != 1 || found[0].Point.IOA != 200 {
		t.Fatalf("found %+v, want one point at 200", found)
	}
	if math.Abs(found[0].Point.Float-42.5) > 0.01 {
		t.Errorf("value %v, want 42.5", found[0].Point.Float)
	}
	if len(missing) != 1 || missing[0] != 201 {
		t.Errorf("missing %v, want [201]", missing)
	}
}

// --- wait_topical -----------------------------------------------------------

func TestWaitTopicalReturnsAtOnceWhenAlreadyTopical(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	if err := s.WaitTopical(t.Context(), 0); err != nil {
		t.Errorf("WaitTopical on a topical station: %v", err)
	}
}

// A caller that does not want to wait gets an answer immediately rather than a
// zero-length sleep.
func TestWaitTopicalWithNoWaitDoesNotWait(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.giSilent = true // never becomes topical

	rec := newRecorder()
	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateInterrogating)

	if err := s.WaitTopical(t.Context(), 0); !errors.Is(err, ErrNotTopical) {
		t.Errorf("got %v, want ErrNotTopical", err)
	}
}

func TestWaitTopicalBlocksUntilTheBracketCloses(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	done := make(chan error, 1)
	go func() { done <- s.WaitTopical(t.Context(), 5*time.Second) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitTopical: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitTopical never returned")
	}
	if !s.State().Topical() {
		t.Error("WaitTopical returned on a station that is not topical")
	}
}

func TestWaitTopicalGivesUpAtItsDeadline(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	o := newOutstation(t)
	o.giSilent = true

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.Clock = fake
	cfg.GI = GI{OnConnect: true, Timeout: time.Hour}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateInterrogating)

	done := make(chan error, 1)
	go func() { done <- s.WaitTopical(context.Background(), time.Minute) }()

	// Three timers now: the connection's, the interrogation's, and the wait's.
	fake.BlockUntilAtLeast(3)
	fake.Advance(2 * time.Minute)

	select {
	case err := <-done:
		if !errors.Is(err, ErrNotTopical) {
			t.Errorf("got %v, want ErrNotTopical", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitTopical ignored its deadline")
	}
}

// --- allocations ------------------------------------------------------------

// The acceptance criterion asks for a reasonable allocation profile on a
// two-hundred-point station. The number is asserted rather than noted, because a
// note goes stale the first time somebody adds a field.
//
// Two allocations: the found slice and the missing slice. Not two hundred.
//
// NOT parallel, and it cannot be: testing.AllocsPerRun panics when called from a
// parallel test (Go 1.25+). The reason is the measurement itself — allocations
// counted while other tests are allocating on other goroutines are not this
// function's allocations, so a parallel run would report a number that means
// nothing and would differ between machines.
func TestReadingTwoHundredPointsAllocatesTwice(t *testing.T) {
	const n = 200

	c := newCache()
	ioas := make([]asdu.IOA, 0, n)
	for i := range n {
		ioa := asdu.IOA(i + 1)
		ioas = append(ioas, ioa)
		if i%2 == 0 { // half of them known, so both slices are exercised
			c.store(point(ioa, float64(i)))
		}
	}

	got := testing.AllocsPerRun(100, func() {
		found, missing := c.readMany(ioas)
		if len(found)+len(missing) != n {
			t.Fatalf("%d + %d != %d", len(found), len(missing), n)
		}
	})

	if got > 2 {
		t.Errorf("%.0f allocations per read of %d points, want at most 2 "+
			"(the found slice and the missing slice)", got, n)
	}
}

// waitCached blocks until an address turns up in the cache.
func waitCached(t *testing.T, s *Station, ioa asdu.IOA) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for {
		if _, ok := s.ReadPoint(ioa); ok {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("point %d never reached the cache", ioa)
		}
	}
}

// The guard above is only worth having if it fires. A test that passes because
// the check is broken is worse than no test: it reports a promise nobody is
// keeping.
func TestTheCopyGuardActuallyDetectsReferenceFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		v    any
		ok   bool
	}{
		{"scalars", struct{ A int }{}, true},
		{"nested scalars", struct{ A struct{ B uint8 } }{}, true},
		{"array of scalars", struct{ A [7]byte }{}, true},
		{"a slice", struct{ A []byte }{}, false},
		{"a map", struct{ A map[int]int }{}, false},
		{"a pointer", struct{ A *int }{}, false},
		{"an interface", struct{ A error }{}, false},
		{"a slice one level down", struct{ A struct{ B []int } }{}, false},
		{"an array of slices", struct{ A [2][]int }{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, ok := pointHasOnlyValueFields(reflect.TypeOf(tc.v))
			if ok != tc.ok {
				t.Errorf("got ok=%v, want %v", ok, tc.ok)
			}
		})
	}
}
