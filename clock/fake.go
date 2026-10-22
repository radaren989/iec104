package clock

import (
	"sync"
	"time"
)

// Fake is a Clock whose time only moves when Advance or Set is called.
//
// It is safe for concurrent use: the code under test usually registers timers
// from its own goroutines while the test advances time from the test goroutine.
//
// Firing is chronological, not batched. Advancing past several deadlines at once
// fires them in deadline order with Now() set to each deadline in turn, so a
// handler that reads Now() sees the instant it was scheduled for and not the
// instant the test happened to advance to. Batching them at the target instant
// would quietly hide ordering bugs, which is the whole class of bug a fake clock
// exists to expose.
type Fake struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []*fakeWaiter
}

// NewFake returns a Fake positioned at t. Tests should pass a fixed, readable
// instant so failures are reproducible.
func NewFake(t time.Time) *Fake {
	f := &Fake{now: t}
	f.cond = sync.NewCond(&f.mu)
	return f
}

var _ Clock = (*Fake)(nil)

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

func (f *Fake) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

func (f *Fake) NewTimer(d time.Duration) Timer {
	return f.addWaiter(d, 0)
}

func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	return fakeTicker{f.addWaiter(d, d)}
}

// Advance moves time forward by d, firing every deadline it passes. It panics on
// a negative d: time does not run backwards, and a test that wants an earlier
// instant should build a new Fake.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic("clock: Fake.Advance with negative duration")
	}
	f.Set(f.Now().Add(d))
}

// Set moves time to t, firing every deadline in (now, t].
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if t.Before(f.now) {
		panic("clock: Fake.Set to an earlier instant")
	}

	for {
		w := f.earliestDueLocked(t)
		if w == nil {
			break
		}
		f.now = w.deadline
		w.sendLocked(f.now)
		if w.period > 0 {
			// A ticker whose period is shorter than the advance fires repeatedly.
			w.deadline = w.deadline.Add(w.period)
		} else {
			f.removeLocked(w)
		}
	}

	f.now = t
	f.cond.Broadcast()
}

// BlockUntil waits until exactly n timers and tickers are registered.
//
// Without it a test that advances time immediately after starting a goroutine
// races that goroutine's NewTimer call and fails intermittently - the exact
// flakiness the fake clock is meant to remove.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) != n {
		f.cond.Wait()
	}
}

// BlockUntilAtLeast waits until at least n timers and tickers are registered.
//
// It exists because BlockUntil's exact match stops being usable the moment more
// than one component shares a clock. A station holds its connection's timer, its
// interrogation timeout and its periodic timer at different moments, so a test
// that says "exactly one" is really saying "nothing else in this system has a
// timer" - which is a statement about the whole program, made by accident, that
// will be false after the next feature.
//
// "At least" is sound here for the same reason BlockUntil is: a waiter's
// deadline is computed under this lock when it is registered, so once it exists
// an Advance cannot invalidate it.
func (f *Fake) BlockUntilAtLeast(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) < n {
		f.cond.Wait()
	}
}

// Waiters reports how many timers and tickers are currently registered. Useful
// in assertions that a component cleaned up after itself.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

func (f *Fake) addWaiter(d time.Duration, period time.Duration) *fakeWaiter {
	f.mu.Lock()
	defer f.mu.Unlock()

	w := &fakeWaiter{
		f:        f,
		deadline: f.now.Add(d),
		period:   period,
		ch:       make(chan time.Time, 1),
	}
	f.waiters = append(f.waiters, w)
	f.cond.Broadcast()
	return w
}

// earliestDueLocked returns the registered waiter with the smallest deadline not
// after limit, or nil. Ties are broken by registration order so the result is
// deterministic.
func (f *Fake) earliestDueLocked(limit time.Time) *fakeWaiter {
	var best *fakeWaiter
	for _, w := range f.waiters {
		if w.deadline.After(limit) {
			continue
		}
		if best == nil || w.deadline.Before(best.deadline) {
			best = w
		}
	}
	return best
}

func (f *Fake) removeLocked(w *fakeWaiter) {
	for i, cand := range f.waiters {
		if cand == w {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			return
		}
	}
}

type fakeWaiter struct {
	f        *Fake
	deadline time.Time
	period   time.Duration // 0 for a timer, > 0 for a ticker
	ch       chan time.Time
}

func (w *fakeWaiter) C() <-chan time.Time { return w.ch }

// sendLocked delivers a tick without blocking. The channel is buffered with one
// slot, so a receiver that is not ready loses the tick - the same behaviour as
// time.Ticker, and the reason a slow consumer never builds a backlog.
func (w *fakeWaiter) sendLocked(t time.Time) {
	select {
	case w.ch <- t:
	default:
	}
}

func (w *fakeWaiter) Stop() bool {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	for _, cand := range w.f.waiters {
		if cand == w {
			w.f.removeLocked(w)
			w.f.cond.Broadcast()
			return true
		}
	}
	return false
}

func (w *fakeWaiter) Reset(d time.Duration) bool {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()

	active := false
	for _, cand := range w.f.waiters {
		if cand == w {
			active = true
			break
		}
	}
	if !active {
		w.f.waiters = append(w.f.waiters, w)
	}
	w.deadline = w.f.now.Add(d)
	if w.period > 0 {
		w.period = d
	}
	w.f.cond.Broadcast()
	return active
}

// fakeTicker adapts fakeWaiter to the Ticker interface, whose Stop and Reset
// return nothing.
type fakeTicker struct{ *fakeWaiter }

func (t fakeTicker) Stop()                 { t.fakeWaiter.Stop() }
func (t fakeTicker) Reset(d time.Duration) { t.fakeWaiter.Reset(d) }
