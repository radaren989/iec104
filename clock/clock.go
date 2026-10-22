// Package clock puts the passage of time behind an interface so that
// timer-driven behaviour can be tested deterministically.
//
// Calling time.Now, time.Sleep and time.Tick directly is forbidden.
// The reason is concrete: t0-t3 APCI framing timers, the station lease sweeper
// (15 minutes) and the chatter window are all time-driven. Without a fake clock
// their tests can only be written with time.Sleep, which is slow, flaky under CI
// load, and simply cannot simulate a 15-minute lease at all.
//
// This package is the single forbidigo exception in .golangci.yml: the real
// implementation has to call the time package, and it is the only code that may.
package clock

import "time"

// Clock is the subset of the time package this service is allowed to use.
//
// There is deliberately no Sleep: blocking a goroutine on a duration is
// forbidden on the read path, and everywhere else waiting
// belongs in a select over a timer channel and a context. Adding Sleep here
// would make the forbidden thing easy.
type Clock interface {
	// Now reports the current instant.
	Now() time.Time

	// Since is Now().Sub(t). It exists so callers do not have to reach for the
	// time package for the single most common derived value.
	Since(t time.Time) time.Duration

	// NewTimer returns a Timer that fires once after d.
	NewTimer(d time.Duration) Timer

	// NewTicker returns a Ticker that fires every d. It panics if d <= 0, like
	// time.NewTicker.
	NewTicker(d time.Duration) Ticker

	// After is shorthand for NewTimer(d).C(). The timer cannot be stopped, so
	// prefer NewTimer in long-lived loops; with the real clock an un-stopped
	// timer is not collected until it fires.
	After(d time.Duration) <-chan time.Time
}

// Timer fires once. Stop and Reset follow time.Timer semantics: Stop reports
// whether the timer was still pending, and a fired timer's channel holds at most
// one value.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker fires repeatedly. As with time.Ticker, a tick is dropped rather than
// queued if the receiver is not ready - slow consumers lose ticks, they do not
// build a backlog.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// System returns the Clock backed by the real time package. Production code
// takes a Clock from its constructor; only main wires this one in.
func System() Clock { return systemClock{} }

type systemClock struct{}

//nolint:forbidigo // systemClock is the sole real-clock implementation; it must call time.Now().
func (systemClock) Now() time.Time                  { return time.Now() }
func (systemClock) Since(t time.Time) time.Duration { return time.Since(t) }
func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

func (systemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{t: time.NewTimer(d)}
}

func (systemClock) NewTicker(d time.Duration) Ticker {
	return &systemTicker{t: time.NewTicker(d)}
}

type systemTimer struct{ t *time.Timer }

func (s *systemTimer) C() <-chan time.Time        { return s.t.C }
func (s *systemTimer) Stop() bool                 { return s.t.Stop() }
func (s *systemTimer) Reset(d time.Duration) bool { return s.t.Reset(d) }

type systemTicker struct{ t *time.Ticker }

func (s *systemTicker) C() <-chan time.Time   { return s.t.C }
func (s *systemTicker) Stop()                 { s.t.Stop() }
func (s *systemTicker) Reset(d time.Duration) { s.t.Reset(d) }
