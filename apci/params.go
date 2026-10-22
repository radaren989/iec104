package apci

import (
	"errors"
	"fmt"
	"time"
)

// Standard defaults (Study Reference §4, contract VALIDATION.md §2). They are
// the values the standard recommends and the values this RTU family ships with,
// so a station that specifies nothing gets an interoperable connection.
const (
	DefaultK  uint16 = 12
	DefaultW  uint16 = 8
	DefaultT0        = 30 * time.Second
	DefaultT1        = 15 * time.Second
	DefaultT2        = 10 * time.Second
	DefaultT3        = 20 * time.Second
)

// ErrParams means the APCI parameters are not a workable set. It is returned
// only by Params.Validate; the gRPC layer turns it into InvalidArgument and the
// connection is never attempted.
var ErrParams = errors.New("invalid APCI parameters")

// Params is the per-station APCI configuration: the two window sizes and the
// four timers.
//
// Both cross-field rules below describe the same failure mode, and it is the
// reason this type validates rather than clamping: the connection comes up,
// works for a while, and then drops periodically. Nothing in the logs says
// "your k and w disagree". A configuration that cannot work is refused before a
// socket is opened.
type Params struct {
	// K is the largest number of sent I-frames that may be outstanding. Once K
	// frames are unacknowledged the sender must stop; a sender that keeps going
	// is closed by the RTU.
	K uint16

	// W is how many received I-frames may accumulate before this side must send
	// an S-format acknowledgement.
	W uint16

	// T0 is the TCP connection-establishment timeout.
	//
	// It is carried here because it is part of the station's APCI configuration
	// and has to be validated with the rest, but it is NOT enforced by Timers:
	// the dial is where it applies, so the connection layer passes it to the
	// dialer. Enforcing it twice would mean two timeouts that can disagree.
	T0 time.Duration

	// T1 is how long to wait for an acknowledgement of a sent I- or U-frame.
	// On expiry the connection is closed — there is no retransmission in 104.
	T1 time.Duration

	// T2 is how long received frames may sit unacknowledged before an S-frame
	// is sent. Must be below T1, see Validate.
	T2 time.Duration

	// T3 is how long a link may be idle before a TESTFR act is sent to probe it.
	T3 time.Duration
}

// DefaultParams returns the standard's recommended set.
func DefaultParams() Params {
	return Params{K: DefaultK, W: DefaultW, T0: DefaultT0, T1: DefaultT1, T2: DefaultT2, T3: DefaultT3}
}

// Validate reports why the parameters cannot work, if they cannot.
func (p Params) Validate() error {
	switch {
	case p.K < 1 || p.K > MaxSeq:
		return fmt.Errorf("%w: k=%d is outside 1..%d", ErrParams, p.K, MaxSeq)
	case p.W < 1 || p.W > MaxSeq:
		return fmt.Errorf("%w: w=%d is outside 1..%d", ErrParams, p.W, MaxSeq)
	}

	// w <= 2k/3. Written as 3w <= 2k so that integer division cannot round the
	// limit up and admit a configuration the standard excludes.
	//
	// Why: w is how many I-APDUs may arrive before this side acknowledges. Let
	// it approach k and the peer fills its send window before any
	// acknowledgement arrives, and the flow deadlocks — both sides waiting.
	// Two thirds is the standard's safety margin.
	if 3*uint32(p.W) > 2*uint32(p.K) {
		return fmt.Errorf("%w: w=%d exceeds 2k/3 with k=%d; the flow would deadlock with both sides waiting",
			ErrParams, p.W, p.K)
	}

	for _, t := range []struct {
		name string
		d    time.Duration
	}{{"t0", p.T0}, {"t1", p.T1}, {"t2", p.T2}, {"t3", p.T3}} {
		if t.d <= 0 {
			return fmt.Errorf("%w: %s=%s must be positive", ErrParams, t.name, t.d)
		}
	}

	// t2 < t1. Why: t1 is how long the peer waits for our acknowledgement, t2
	// is how long we wait before sending one. With t2 >= t1 the peer times us
	// out before we ever speak — a connection that establishes, runs, and then
	// drops on a period nobody can explain.
	if p.T2 >= p.T1 {
		return fmt.Errorf("%w: t2=%s must be below t1=%s, otherwise the peer times us out before we acknowledge",
			ErrParams, p.T2, p.T1)
	}

	return nil
}

// CanSend reports whether another I-frame may be sent, given how many are
// already outstanding (Sequencer.Outstanding).
//
// The window is a hard stop, not a hint: sending the K+1st frame is what makes
// an RTU drop the connection. What to do while the window is full — block, and
// for how long — is the connection layer's decision, which is why this package
// answers the question and does not enforce the answer.
func (p Params) CanSend(outstanding uint16) bool {
	return outstanding < p.K
}

// AckDue reports whether an S-format acknowledgement must be sent now, given how
// many received I-frames are still unacknowledged (Sequencer.UnackedReceived).
//
// Note that this is only one of the two reasons to send an S-frame: the other is
// timer t2 expiring on a link too quiet to reach W. Timers covers that one.
func (p Params) AckDue(unackedReceived uint16) bool {
	return unackedReceived >= p.W
}
