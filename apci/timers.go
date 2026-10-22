package apci

import "time"

// Action is what the APCI timers say must happen now.
type Action uint8

const (
	// ActionNone: nothing is due.
	ActionNone Action = iota

	// ActionClose: t1 expired. A sent I- or U-frame went unacknowledged for
	// longer than t1, and 104 has no retransmission — the only defined response
	// is to close the connection and start over with a new STARTDT handshake.
	ActionClose

	// ActionSendSupervisory: t2 expired. Received frames have gone
	// unacknowledged too long on a link too quiet to reach the w window.
	ActionSendSupervisory

	// ActionSendTestFR: t3 expired. The link has been idle; send TESTFR act to
	// find out whether it is still there. A dead TCP connection is otherwise
	// indistinguishable from a quiet one, sometimes for hours.
	ActionSendTestFR
)

func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionClose:
		return "close (t1)"
	case ActionSendSupervisory:
		return "send S-frame (t2)"
	case ActionSendTestFR:
		return "send TESTFR act (t3)"
	default:
		return "invalid action"
	}
}

// Timers is the t1/t2/t3 state of one connection.
//
// It holds no clock. Every method takes the current instant, which the
// connection layer gets from its clock.Clock. That keeps this package free of
// any dependency and makes the tests a table of instants rather than a
// choreography with a fake clock — the timer logic is then tested by reading it,
// not by driving it.
//
// t0 is deliberately absent. It is the TCP connect timeout, so it belongs to the
// dial: the connection layer hands Params.T0 to its dialer. Implementing it here
// as well would mean two timeouts for one event, and which one won would depend
// on which layer returned first.
//
// Timers is not safe for concurrent use; it belongs to one goroutine, like
// Sequencer.
type Timers struct {
	p Params

	// Zero means disarmed. Deadlines rather than durations, so that a missed
	// tick does not push the deadline forward: a loop that wakes late still
	// fires at the right moment rather than the right interval after waking.
	t1, t2, t3 time.Time
}

// NewTimers returns the timer state for a connection whose parameters are p.
// The connection is not yet started — nothing is armed until Start.
func NewTimers(p Params) *Timers {
	return &Timers{p: p}
}

// Start records that the connection is established and begins the idle timer.
// Call it once, when the TCP connection is up.
func (t *Timers) Start(now time.Time) {
	t.t3 = now.Add(t.p.T3)
}

// Stop disarms everything. After it, Check reports ActionNone and no deadline.
func (t *Timers) Stop() {
	t.t1, t.t2, t.t3 = time.Time{}, time.Time{}, time.Time{}
}

// --- outgoing frames --------------------------------------------------------

// SentI records an outgoing I-frame.
//
// It arms t1 (the frame must be acknowledged), and because every outgoing frame
// carries N(R) it also acknowledges everything received, so it disarms t2.
func (t *Timers) SentI(now time.Time) {
	t.armT1(now)
	t.t2 = time.Time{}
	t.resetIdle(now)
}

// SentS records an outgoing S-format acknowledgement. S-frames are not
// acknowledged in turn, so t1 is untouched.
func (t *Timers) SentS(now time.Time) {
	t.t2 = time.Time{}
	t.resetIdle(now)
}

// SentU records an outgoing U-format frame. An activation expects a
// confirmation and therefore arms t1; a confirmation answers someone else's
// activation and expects nothing.
//
// This is what makes TESTFR useful rather than decorative: the probe t3 sends is
// covered by t1, so a link that has silently died is closed one t1 later instead
// of lingering.
func (t *Timers) SentU(now time.Time, fn UFunction) {
	if fn.IsAct() {
		t.armT1(now)
	}
	t.resetIdle(now)
}

// --- incoming frames --------------------------------------------------------

// ReceivedI records an incoming I-frame. outstanding is how many sent frames
// remain unacknowledged after applying the frame's N(R) — that is,
// Sequencer.Outstanding after Sequencer.Receive.
func (t *Timers) ReceivedI(now time.Time, outstanding uint16) {
	t.applyAck(now, outstanding)
	if t.t2.IsZero() {
		// t2 measures from the FIRST unacknowledged frame, not the latest.
		// Restarting it on every arrival would let a steady trickle of frames
		// go unacknowledged forever and the peer would time us out.
		t.t2 = now.Add(t.p.T2)
	}
	t.resetIdle(now)
}

// ReceivedS records an incoming S-format acknowledgement. outstanding is as in
// ReceivedI.
func (t *Timers) ReceivedS(now time.Time, outstanding uint16) {
	t.applyAck(now, outstanding)
	t.resetIdle(now)
}

// ReceivedU records an incoming U-format frame. A confirmation answers an
// activation this side sent, which is what t1 was waiting for.
func (t *Timers) ReceivedU(now time.Time, fn UFunction) {
	if !fn.IsAct() {
		// A confirmation clears t1 outright. Unlike an I-frame acknowledgement
		// there is no partial case: a U activation is answered by exactly one
		// confirmation.
		t.t1 = time.Time{}
	}
	t.resetIdle(now)
}

// --- the query --------------------------------------------------------------

// Check reports the most urgent action due at now, and how long until the next
// deadline (zero if nothing is armed).
//
// It does not mutate: acting on what it reports is the caller's job, and the
// caller records what it did through the Sent* methods. A caller that ignores a
// due action is told again on the next call — deliberately, because silently
// consuming an action the caller never carried out would turn a missing frame
// into a mystery.
//
// The ordering is by consequence, not by timer number: closing wins over
// anything else, because a connection about to be closed should not first be
// asked to send frames on it.
func (t *Timers) Check(now time.Time) (Action, time.Duration) {
	switch {
	case expired(t.t1, now):
		return ActionClose, 0
	case expired(t.t2, now):
		return ActionSendSupervisory, 0
	case expired(t.t3, now):
		return ActionSendTestFR, 0
	}

	var next time.Time
	for _, d := range [...]time.Time{t.t1, t.t2, t.t3} {
		if d.IsZero() {
			continue
		}
		if next.IsZero() || d.Before(next) {
			next = d
		}
	}
	if next.IsZero() {
		return ActionNone, 0
	}
	return ActionNone, next.Sub(now)
}

// --- internals --------------------------------------------------------------

// armT1 starts t1 if it is not already running. It is not restarted on every
// send: t1 measures the oldest unacknowledged frame, and restarting it on each
// new frame would let a busy sender hold a dead peer open indefinitely.
func (t *Timers) armT1(now time.Time) {
	if t.t1.IsZero() {
		t.t1 = now.Add(t.p.T1)
	}
}

// applyAck updates t1 after an acknowledgement.
//
// With nothing left outstanding t1 stops. With frames still in flight it
// restarts from now rather than from when the oldest remaining frame was sent.
// Tracking per-frame send times would be more faithful, but it means a ring
// buffer of up to k timestamps for a difference that only shows up as closing a
// dead link slightly later — and a dead link is already being probed by t3.
func (t *Timers) applyAck(now time.Time, outstanding uint16) {
	if outstanding == 0 {
		t.t1 = time.Time{}
		return
	}
	t.t1 = now.Add(t.p.T1)
}

// resetIdle restarts t3. Any traffic in either direction proves the link is
// alive, which is the only thing t3 is asking about.
func (t *Timers) resetIdle(now time.Time) {
	t.t3 = now.Add(t.p.T3)
}

// expired reports whether an armed deadline has been reached. A disarmed
// (zero) deadline never expires.
func expired(deadline, now time.Time) bool {
	return !deadline.IsZero() && !now.Before(deadline)
}
