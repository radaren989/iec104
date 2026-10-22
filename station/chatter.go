package station

import (
	"fmt"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// This file is the chatter filter (T7.2).
//
// A bouncing contact reports hundreds of changes a minute, and an alarm system
// that raises each one drowns. But the events are NOT swallowed: the contract
// publishes them with chatter_suppressed set, so the alarm system learns "this
// contact is bouncing" instead of learning nothing. What this file decides is
// only whether that flag is set.
//
// Three decisions, each made with the developer (2026-09-30):
//
//   - Contacts only (asdu.TypeID.IsContact). A measurand reporting every time it
//     crosses its deadband is working as designed.
//   - The device's timestamp when it has a usable one, ours when it does not.
//     After an outage an RTU empties its buffer in one burst: measured by
//     arrival, changes that were hours apart would all look like chatter, at the
//     moment an operator most needs to read them. The two clocks are never
//     compared with each other — see contactHistory.
//   - Here, next to the COT decision, not in eventsink: the type, the station's
//     policy, the clock and the timestamp are all already here.

// Chatter is the per-station chatter policy: more than MaxChanges changes of one
// contact within Window flags the ones beyond. The contract's EventPolicy
// chatter_max_changes and chatter_window_sec.
//
// The zero value means DefaultChatter. A policy with only one of the two fields
// set is rejected rather than half filled in.
type Chatter struct {
	MaxChanges int
	Window     time.Duration
}

// Chatter defaults and bounds. The defaults are the contract's; the thresholds
// are to be calibrated from field experience.
//
// The upper bounds are safety limits: every contact that changes keeps up to
// MaxChanges+1 instants, so an unbounded MaxChanges is an unbounded buffer per
// point, and an unbounded Window keeps quiet contacts' history forever.
const (
	DefaultChatterMaxChanges = 5
	DefaultChatterWindow     = 10 * time.Second

	MaxChatterMaxChanges = 1000
	MinChatterWindow     = time.Second
	MaxChatterWindow     = time.Hour
)

// DefaultChatter returns the policy a station gets when it specifies none.
func DefaultChatter() Chatter {
	return Chatter{MaxChanges: DefaultChatterMaxChanges, Window: DefaultChatterWindow}
}

// Validate rejects a policy that cannot work or would not be bounded.
func (c Chatter) Validate() error {
	if c.MaxChanges < 1 || c.MaxChanges > MaxChatterMaxChanges {
		return fmt.Errorf("%w: chatter max changes %d is outside 1..%d",
			ErrConfig, c.MaxChanges, MaxChatterMaxChanges)
	}
	if c.Window < MinChatterWindow || c.Window > MaxChatterWindow {
		return fmt.Errorf("%w: chatter window %s is outside %s..%s",
			ErrConfig, c.Window, MinChatterWindow, MaxChatterWindow)
	}
	return nil
}

// chatterFilter is one station's chatter state.
//
// It is touched only by absorb, on the connection's runLoop, so it has no lock.
// That holds across reconnects too: attempt closes a connection before dialling
// the next, and Close waits for the connection's goroutines, so two runLoops of
// the same station never overlap. The history outlives the connection on
// purpose — a contact bouncing hard enough to disturb the link is the one whose
// history should not reset every time the link drops.
type chatterFilter struct {
	policy Chatter

	// base is the instant arrival times are measured from. Differences between
	// times read from one Clock are monotonic, which a wall-clock reading is
	// not.
	base time.Time

	points map[asdu.IOA]*contactHistory

	// lastSweep is the arrival instant, in milliseconds since base, of the last
	// sweep for quiet contacts.
	lastSweep int64
}

func newChatterFilter(policy Chatter, base time.Time) *chatterFilter {
	return &chatterFilter{
		policy: policy,
		base:   base,
		points: make(map[asdu.IOA]*contactHistory),
	}
}

// instantSource says which clock a contact's instants were read from.
type instantSource uint8

const (
	fromArrival instantSource = iota + 1 // our clock, ms since chatterFilter.base
	fromDevice                           // the device's CP56Time2a, epoch ms
)

// contactHistory is the last MaxChanges+1 change instants of one contact, in
// milliseconds, in a ring.
//
// All instants in one history come from ONE clock. The device's epoch
// milliseconds and our milliseconds-since-base are different scales entirely,
// and even two device instants cannot be compared across a step backwards in
// the device's clock. Either event resets the history: the price is that a
// contact might go unflagged for up to MaxChanges changes, which is far cheaper
// than a flag computed from a meaningless difference.
type contactHistory struct {
	source  instantSource
	instant []int64
	head    int // index of the oldest instant once the ring is full
	newest  int64

	// seen is when this contact last changed, by OUR clock, whichever clock its
	// instants come from. The sweep has to judge every contact on one scale.
	seen int64
}

func (h *contactHistory) reset(src instantSource) {
	h.source = src
	h.instant = h.instant[:0]
	h.head = 0
}

// push records an instant, overwriting the oldest once size are held.
func (h *contactHistory) push(at int64, size int) {
	h.newest = at
	if len(h.instant) < size {
		h.instant = append(h.instant, at)
		return
	}
	h.instant[h.head] = at
	h.head = (h.head + 1) % size
}

// oldest is the earliest instant held.
func (h *contactHistory) oldest() int64 {
	return h.instant[h.head]
}

// observe records one change of a contact and reports whether it is chatter:
// the policy's MaxChanges exceeded within its Window, this change included.
//
// now is the instant the frame arrived. It is always needed, even for a
// timestamped reading, because the sweep runs on our clock.
func (f *chatterFilter) observe(r Reading, now time.Time) bool {
	arrival := now.Sub(f.base).Milliseconds()
	f.sweep(arrival)

	at, src := arrival, fromArrival
	if r.TSValid {
		at, src = r.TSUTCMillis, fromDevice
	}

	h := f.points[r.Point.IOA]
	if h == nil {
		h = &contactHistory{source: src}
		f.points[r.Point.IOA] = h
	}
	if h.source != src || (len(h.instant) > 0 && at < h.newest) {
		h.reset(src)
	}

	size := f.policy.MaxChanges + 1
	h.push(at, size)
	h.seen = arrival

	return len(h.instant) == size && at-h.oldest() < f.policy.Window.Milliseconds()
}

// sweep forgets contacts that have been quiet for a whole window, at most once
// per window.
//
// This is what bounds the map: without it every contact that
// ever changed would keep its history for the life of the station, and a device
// reporting addresses it does not have would grow it without limit. With it the
// map holds the contacts that changed within roughly the last two windows. The
// sweep is O(contacts) on the runLoop, once per window — a few microseconds
// every ten seconds by default.
//
// A contact forgotten here and heard from again starts a fresh history. Its
// device timestamps might still have fallen inside the window — a buffer
// emptied after an outage — and then it is flagged later than it could have
// been. That is the error in the harmless direction.
func (f *chatterFilter) sweep(arrival int64) {
	window := f.policy.Window.Milliseconds()
	if arrival-f.lastSweep < window {
		return
	}
	f.lastSweep = arrival
	for ioa, h := range f.points {
		if arrival-h.seen >= window {
			delete(f.points, ioa)
		}
	}
}
