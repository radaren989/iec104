package station

import (
	"errors"
	"testing"
	"time"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
)

// --- the time zone ----------------------------------------------------------

func TestTheZoneMustResolveOrTheStationIsRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		zone string
		want bool // want an error
	}{
		{"empty means UTC", "", false},
		{"a real zone", "Europe/Istanbul", false},
		{"UTC by name", "UTC", false},
		// Not a fallback to UTC, deliberately: falling back would produce
		// exactly the silent hour-sized error the zone field exists to prevent,
		// on every timestamp, forever. Refusing is loud and fixable.
		{"a zone nobody has", "Mars/Olympus_Mons", true},
		{"a typo", "Europe/Istambul", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := ClockSync{Zone: tc.zone}
			err := c.Validate()
			if (err != nil) != tc.want {
				t.Fatalf("Validate() = %v, want error: %v", err, tc.want)
			}
			if err != nil && !errors.Is(err, ErrConfig) {
				t.Errorf("%v does not wrap ErrConfig", err)
			}
		})
	}
}

func TestAStationWithAnUnresolvableZoneIsNotStarted(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, "127.0.0.1:2404", newRecorder())
	cfg.ClockSync = ClockSync{Zone: "Nowhere/Nothing"}

	s, err := New(cfg)
	if err == nil {
		_ = s.Release()
		t.Fatal("a station started with a zone that does not resolve")
	}
	if !errors.Is(err, ErrConfig) {
		t.Errorf("%v does not wrap ErrConfig", err)
	}
}

// --- skew -------------------------------------------------------------------

// The zone is not decoration. CP56Time2a carries local time with no offset, so
// reading one in the wrong zone shifts it by whole hours — and the value still
// looks entirely reasonable afterwards, which is what makes it dangerous.
func TestTheZoneDecidesWhatATimestampMeans(t *testing.T) {
	t.Parallel()

	// 12:00 as the device sees it.
	deviceTime := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	tag := asdu.CP56Time2aFrom(deviceTime)

	// And 12:00 UTC is when we think it is.
	now := deviceTime

	utc := stationWithZone(t, "")
	ist := stationWithZone(t, "Europe/Istanbul")

	p := asdu.Point{Type: asdu.MSpTb1, IOA: 1, HasTime: true, Time: tag}

	inUTC := utc.reading(p, now)
	inIstanbul := ist.reading(p, now)

	if !inUTC.TSValid || !inIstanbul.TSValid {
		t.Fatal("a usable timestamp was not resolved")
	}
	if inUTC.SkewMillis != 0 {
		t.Errorf("a device on our own clock reads %d ms of skew, want 0", inUTC.SkewMillis)
	}

	// Istanbul is UTC+3, so a device clock reading 12:00 local is 09:00 UTC —
	// three hours BEHIND us, not level with us.
	if got := time.Duration(inIstanbul.SkewMillis) * time.Millisecond; got != -3*time.Hour {
		t.Errorf("skew %s, want -3h; the same octets mean different instants in different zones", got)
	}
}

func TestSkewIsMeasuredFromTheDeviceClockMinusOurs(t *testing.T) {
	t.Parallel()

	s := stationWithZone(t, "")

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		device time.Time
		want   time.Duration
	}{
		{"level", now, 0},
		{"device ahead", now.Add(5 * time.Second), 5 * time.Second},
		{"device behind", now.Add(-5 * time.Second), -5 * time.Second},
		{"device an hour ahead", now.Add(time.Hour), time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := asdu.Point{Type: asdu.MSpTb1, IOA: 1, HasTime: true, Time: asdu.CP56Time2aFrom(tc.device)}
			got := time.Duration(s.reading(p, now).SkewMillis) * time.Millisecond
			if got != tc.want {
				t.Errorf("skew %s, want %s", got, tc.want)
			}
		})
	}
}

// A tag the device itself marked invalid, or one whose calendar fields are out
// of range, is not something to measure against. Pretending otherwise would put
// a fabricated number into the skew metric — and a fabricated skew is worse than
// none, because somebody will act on it.
func TestAnUnusableTimestampProducesNoSkew(t *testing.T) {
	t.Parallel()

	s := stationWithZone(t, "")
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	t.Run("no timestamp at all", func(t *testing.T) {
		t.Parallel()
		r := s.reading(asdu.Point{Type: asdu.MSpNa1, IOA: 1}, now)
		if r.TSValid || r.TSUTCMillis != 0 || r.SkewMillis != 0 {
			t.Errorf("a type without a timestamp produced %+v", r)
		}
	})

	t.Run("the device marked it invalid", func(t *testing.T) {
		t.Parallel()
		tag := asdu.CP56Time2aFrom(now.Add(time.Hour))
		tag.Invalid = true
		r := s.reading(asdu.Point{Type: asdu.MSpTb1, IOA: 1, HasTime: true, Time: tag}, now)
		if r.TSValid || r.SkewMillis != 0 {
			t.Errorf("an invalid tag produced %+v", r)
		}
	})

	t.Run("out of range", func(t *testing.T) {
		t.Parallel()
		var tag asdu.CP56Time2a
		tag.Month = 13
		r := s.reading(asdu.Point{Type: asdu.MSpTb1, IOA: 1, HasTime: true, Time: tag}, now)
		if r.TSValid || r.SkewMillis != 0 {
			t.Errorf("an out-of-range tag produced %+v", r)
		}
	})
}

func TestTheWarningThresholdIsSymmetric(t *testing.T) {
	t.Parallel()

	// A clock three seconds behind is as wrong as one three seconds ahead, and
	// a threshold that only caught one direction would miss half the faults.
	cases := []struct {
		name   string
		skew   time.Duration
		beyond bool
	}{
		{"well within", time.Second, false},
		{"exactly at the threshold", 2 * time.Second, false},
		{"beyond, ahead", 3 * time.Second, true},
		{"beyond, behind", -3 * time.Second, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := newRecorder()
			s := &Station{cfg: Config{Observer: rec, ClockSync: ClockSync{WarnAbove: 2 * time.Second}}}
			s.noteSkew(Reading{TSValid: true, SkewMillis: tc.skew.Milliseconds()})

			reports := rec.skewReports()
			if len(reports) != 1 {
				t.Fatalf("%d skew reports, want 1", len(reports))
			}
			if reports[0].beyond != tc.beyond {
				t.Errorf("beyond = %v, want %v for a skew of %s", reports[0].beyond, tc.beyond, tc.skew)
			}
		})
	}
}

// Measuring is free and the metric is useful everywhere. Only the WARNING is
// opt-in, so a station with no threshold still reports its skew.
func TestSkewIsStillMeasuredWithoutAThreshold(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	s := &Station{cfg: Config{Observer: rec, ClockSync: ClockSync{WarnAbove: 0}}}
	s.noteSkew(Reading{TSValid: true, SkewMillis: (time.Hour).Milliseconds()})

	reports := rec.skewReports()
	if len(reports) != 1 {
		t.Fatalf("%d skew reports, want 1", len(reports))
	}
	if reports[0].beyond {
		t.Error("a station with no threshold reported one as exceeded")
	}
	if reports[0].skew != time.Hour {
		t.Errorf("skew %s, want 1h", reports[0].skew)
	}
}

func TestSkewReachesTheObserverFromARealFrame(t *testing.T) {
	t.Parallel()

	// A fake clock, so the answer is exact rather than "about". With the system
	// clock this test could only assert a range wide enough to absorb its own
	// runtime, which would not notice a skew computed against the wrong instant
	// by a second or two.
	fake := clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))

	o := newOutstation(t)
	rec := newRecorder()

	cfg := testConfig(t, o.addr(), rec)
	cfg.Clock = fake

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	o.sendTimestamped(300, 1.0, fake.Now().Add(-42*time.Second))
	waitCached(t, s, 300)

	r, ok := s.ReadPoint(300)
	if !ok {
		t.Fatal("the point never arrived")
	}
	if !r.TSValid {
		t.Fatal("a timestamped point produced no instant")
	}
	if got := time.Duration(r.SkewMillis) * time.Millisecond; got != -42*time.Second {
		t.Errorf("skew %s, want exactly -42s", got)
	}
}

// --- clock synchronisation --------------------------------------------------

// The acceptance criterion, and the one that matters most: with synchronisation
// off, NOTHING is sent. Two masters setting the same clock is worse than none —
// they fight, and the timestamps jitter between two sources instead of drifting
// predictably from one.
func TestWithSynchronisationOffNoClockFrameIsEverSent(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	o := newOutstation(t)
	rec := newRecorder()

	cfg := testConfig(t, o.addr(), rec)
	cfg.Clock = fake

	if cfg.ClockSync.Enabled {
		t.Fatal("synchronisation is on by default; V10 says it must be opt-in")
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	// Proving a negative needs a window in which the wrong thing could have
	// happened, and two of them: one for the connect-time path, one for
	// anything on a timer. Without the first, this test passes against a
	// station that synchronises on every connect — it simply checks the counter
	// before the frame has crossed the socket. That is not a hypothetical; it
	// is what the first version of this test did.
	settleForNegative()

	fake.BlockUntilAtLeast(2)
	fake.Advance(365 * 24 * time.Hour)
	settleForNegative()

	if n := o.clockSyncs(); n != 0 {
		t.Errorf("%d clock synchronisations were sent with the feature off", n)
	}
	if errs := rec.syncErrors(); len(errs) != 0 {
		t.Errorf("the observer heard about synchronisation with the feature off: %v", errs)
	}
}

func TestWithSynchronisationOnTheClockIsSetOnConnect(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	cfg := testConfig(t, o.addr(), rec)
	cfg.ClockSync = ClockSync{Enabled: true, Period: time.Hour, WarnAbove: time.Second}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)
	waitClockSyncs(t, o, 1)

	if errs := rec.syncErrors(); len(errs) != 0 {
		t.Errorf("synchronisation reported errors: %v", errs)
	}
}

// A device that refuses is a legitimate answer — some will not take a clock from
// a master at all. It must not drop a connection that is otherwise delivering
// data, and it must not be reported as if the clock had been set.
func TestARefusedSynchronisationIsReportedNotFatal(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.clockRefuse = true

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.ClockSync = ClockSync{Enabled: true, Period: time.Hour}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)
	waitSyncErrors(t, rec, 1)

	if got := rec.syncErrors()[0]; !errors.Is(got, ErrClockSyncRefused) {
		t.Errorf("got %v, want ErrClockSyncRefused", got)
	}
	if err := s.State(); !err.Topical() {
		t.Errorf("state %s; a refused clock must not take the station down", err)
	}
}

// --- helpers ----------------------------------------------------------------

// settleForNegative is the window a "this must never happen" assertion needs.
//
// There is no event to wait on for "the station did not send a frame", so the
// only honest option is to leave enough time for it to have done so. A hundred
// milliseconds is several orders of magnitude more than a loopback round trip.
func settleForNegative() {
	//nolint:forbidigo // see above: proving a negative, not synchronising.
	time.Sleep(100 * time.Millisecond)
}

// stationWithZone builds a station shell for the pure conversion tests. It never
// connects: reading() is arithmetic over a Point and an instant.
func stationWithZone(t *testing.T, zone string) *Station {
	t.Helper()

	cs := ClockSync{Zone: zone}
	if err := cs.Validate(); err != nil {
		t.Fatalf("zone %q: %v", zone, err)
	}
	return &Station{cfg: Config{ClockSync: cs, Observer: newRecorder()}}
}

func waitClockSyncs(t *testing.T, o *outstation, n int) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for o.clockSyncs() < n {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("%d of %d clock synchronisations were sent", o.clockSyncs(), n)
		}
	}
}

func waitSyncErrors(t *testing.T, r *recorder, n int) {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for len(r.syncErrors()) < n {
		select {
		case <-r.changed:
		case <-timeout.C:
			t.Fatalf("%d of %d synchronisation errors reported", len(r.syncErrors()), n)
		}
	}
}
