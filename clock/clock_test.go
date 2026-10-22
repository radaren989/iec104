package clock_test

import (
	"testing"
	"time"

	"github.com/radaren989/iec104/clock"
)

// base is a fixed, readable instant so a failure message is reproducible.
var base = time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

func TestFake_NowDoesNotMoveOnItsOwn(t *testing.T) {
	f := clock.NewFake(base)
	if got := f.Now(); !got.Equal(base) {
		t.Fatalf("Now() = %v, want %v", got, base)
	}
	if got := f.Now(); !got.Equal(base) {
		t.Fatalf("Now() moved without Advance: %v", got)
	}
}

func TestFake_TimerFiresOnlyAfterItsDeadline(t *testing.T) {
	f := clock.NewFake(base)
	timer := f.NewTimer(10 * time.Second)

	f.Advance(9 * time.Second)
	select {
	case v := <-timer.C():
		t.Fatalf("timer fired early at %v", v)
	default:
	}

	f.Advance(time.Second)
	select {
	case v := <-timer.C():
		if want := base.Add(10 * time.Second); !v.Equal(want) {
			t.Fatalf("fired with %v, want the deadline %v", v, want)
		}
	default:
		t.Fatal("timer did not fire at its deadline")
	}
}

// A timer must report the instant it was scheduled for, not the instant the test
// advanced to. Handlers that stamp events from Now() depend on this.
func TestFake_FiresAtDeadlineNotAtTarget(t *testing.T) {
	f := clock.NewFake(base)
	timer := f.NewTimer(2 * time.Second)

	f.Advance(time.Hour)

	got := <-timer.C()
	if want := base.Add(2 * time.Second); !got.Equal(want) {
		t.Fatalf("fired with %v, want %v", got, want)
	}
	if want := base.Add(time.Hour); !f.Now().Equal(want) {
		t.Fatalf("Now() = %v after advancing an hour, want %v", f.Now(), want)
	}
}

// Several deadlines crossed by one Advance must fire in chronological order.
// Batching them would hide ordering bugs, which is what the fake exists to expose.
func TestFake_MultipleDeadlinesFireInOrder(t *testing.T) {
	f := clock.NewFake(base)
	late := f.NewTimer(30 * time.Second)
	early := f.NewTimer(10 * time.Second)
	middle := f.NewTimer(20 * time.Second)

	f.Advance(time.Minute)

	for i, tc := range []struct {
		name  string
		timer clock.Timer
		after time.Duration
	}{
		{"early", early, 10 * time.Second},
		{"middle", middle, 20 * time.Second},
		{"late", late, 30 * time.Second},
	} {
		select {
		case got := <-tc.timer.C():
			if want := base.Add(tc.after); !got.Equal(want) {
				t.Errorf("%d %s: fired with %v, want %v", i, tc.name, got, want)
			}
		default:
			t.Errorf("%d %s: did not fire", i, tc.name)
		}
	}
}

func TestFake_TickerRepeats(t *testing.T) {
	f := clock.NewFake(base)
	ticker := f.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Drain one tick at a time: the channel holds a single slot, exactly like
	// time.Ticker, so ticks are dropped rather than queued.
	for i := 1; i <= 3; i++ {
		f.Advance(5 * time.Second)
		select {
		case got := <-ticker.C():
			if want := base.Add(time.Duration(i) * 5 * time.Second); !got.Equal(want) {
				t.Fatalf("tick %d: got %v, want %v", i, got, want)
			}
		default:
			t.Fatalf("tick %d: ticker did not fire", i)
		}
	}
}

// A slow consumer loses ticks instead of building a backlog - the property the
// event path relies on so a stalled reader cannot grow unbounded memory.
func TestFake_TickerDropsTicksForSlowConsumer(t *testing.T) {
	f := clock.NewFake(base)
	ticker := f.NewTicker(time.Second)
	defer ticker.Stop()

	f.Advance(10 * time.Second)

	if _, ok := <-ticker.C(); !ok {
		t.Fatal("expected one buffered tick")
	}
	select {
	case got := <-ticker.C():
		t.Fatalf("expected the remaining ticks to be dropped, got %v", got)
	default:
	}
}

func TestFake_StopPreventsFiring(t *testing.T) {
	f := clock.NewFake(base)
	timer := f.NewTimer(time.Second)

	if !timer.Stop() {
		t.Fatal("Stop() on a pending timer = false, want true")
	}
	if timer.Stop() {
		t.Fatal("Stop() on an already stopped timer = true, want false")
	}

	f.Advance(time.Minute)
	select {
	case got := <-timer.C():
		t.Fatalf("stopped timer fired with %v", got)
	default:
	}
	if got := f.Waiters(); got != 0 {
		t.Fatalf("Waiters() = %d after Stop, want 0", got)
	}
}

func TestFake_ResetReschedulesFromNow(t *testing.T) {
	f := clock.NewFake(base)
	timer := f.NewTimer(10 * time.Second)

	f.Advance(5 * time.Second)
	if !timer.Reset(10 * time.Second) {
		t.Fatal("Reset() on a pending timer = false, want true")
	}

	f.Advance(5 * time.Second) // would have been the original deadline
	select {
	case got := <-timer.C():
		t.Fatalf("timer fired at the original deadline: %v", got)
	default:
	}

	f.Advance(5 * time.Second)
	if got := <-timer.C(); !got.Equal(base.Add(15 * time.Second)) {
		t.Fatalf("got %v, want %v", got, base.Add(15*time.Second))
	}
}

func TestFake_WaitersTracksRegistration(t *testing.T) {
	f := clock.NewFake(base)
	if got := f.Waiters(); got != 0 {
		t.Fatalf("Waiters() = %d on a fresh Fake, want 0", got)
	}

	timer := f.NewTimer(time.Second)
	ticker := f.NewTicker(time.Second)
	if got := f.Waiters(); got != 2 {
		t.Fatalf("Waiters() = %d, want 2", got)
	}

	// A fired one-shot timer deregisters itself; a ticker stays.
	f.Advance(time.Second)
	<-timer.C()
	if got := f.Waiters(); got != 1 {
		t.Fatalf("Waiters() = %d after the timer fired, want 1", got)
	}
	ticker.Stop()
	if got := f.Waiters(); got != 0 {
		t.Fatalf("Waiters() = %d after Stop, want 0", got)
	}
}

// BlockUntil is what makes a test that advances time from one goroutine while
// the code under test registers a timer from another deterministic.
func TestFake_BlockUntilWaitsForRegistration(t *testing.T) {
	f := clock.NewFake(base)
	fired := make(chan time.Time, 1)

	go func() {
		timer := f.NewTimer(time.Second)
		fired <- <-timer.C()
	}()

	f.BlockUntil(1)
	f.Advance(time.Second)

	select {
	case got := <-fired:
		if !got.Equal(base.Add(time.Second)) {
			t.Fatalf("got %v, want %v", got, base.Add(time.Second))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timer never fired")
	}
}

func TestFake_PanicsOnInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*clock.Fake)
	}{
		{"negative advance", func(f *clock.Fake) { f.Advance(-time.Second) }},
		{"set into the past", func(f *clock.Fake) { f.Set(base.Add(-time.Second)) }},
		{"non-positive ticker", func(f *clock.Fake) { f.NewTicker(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			tc.call(clock.NewFake(base))
		})
	}
}

func TestSystem_NowAdvances(t *testing.T) {
	c := clock.System()
	first := c.Now()
	<-c.After(time.Millisecond)
	if !c.Now().After(first) {
		t.Fatal("System() Now did not advance across a real timer")
	}
	if c.Since(first) <= 0 {
		t.Fatal("Since() <= 0 after waiting")
	}
}

func TestSystem_TimerAndTicker(t *testing.T) {
	c := clock.System()

	timer := c.NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(2 * time.Second):
		t.Fatal("system timer did not fire")
	}

	ticker := c.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-ticker.C():
		case <-time.After(2 * time.Second):
			t.Fatalf("system ticker did not fire (tick %d)", i)
		}
	}
}
