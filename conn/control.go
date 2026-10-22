package conn

import (
	"context"
	"fmt"

	"github.com/radaren989/iec104/apci"
)

// This file holds the STARTDT/STOPDT dialogue — what the standard calls data
// transfer control, and what the Study Reference §4 calls the mandatory startup
// handshake.
//
// It is worth being blunt about why it is mandatory rather than polite: until
// the controlled station has answered STARTDT con it sends NO I-frames at all.
// Not fewer, none. A connection that skips this step looks perfectly healthy —
// TCP is up, TESTFR is answered, no error appears anywhere — and delivers no
// data, forever. That failure is invisible from the outside, which is why the
// handshake belongs in the connection rather than in whatever remembers to call
// it.

// dataTransfer is where a connection stands in the STARTDT/STOPDT dialogue.
//
// It is owned by runLoop, like the sequencer and the timers, and for the same
// reason: it is changed both by what this side sends and by what arrives.
type dataTransfer uint8

const (
	// transferStopped is the state a connection is born in. The socket is open
	// and U-frames flow, but no I-frame may cross in either direction.
	transferStopped dataTransfer = iota

	// transferStarting means STARTDT act has gone out and its confirmation has
	// not arrived. Still no I-frames: the controlled station is allowed to send
	// one only after it has sent the confirmation.
	transferStarting

	// transferStarted is data transfer. I-frames flow both ways.
	transferStarted

	// transferStopping means STOPDT act has gone out and its confirmation has
	// not arrived.
	//
	// Incoming I-frames are still accepted here, and that is deliberate: the
	// peer may have frames already on the wire when our STOPDT act reaches it,
	// and those are not a protocol violation — they are a consequence of the
	// speed of light. Rejecting them would turn an orderly shutdown into a
	// protocol error on a link that did nothing wrong.
	transferStopping
)

func (d dataTransfer) String() string {
	switch d {
	case transferStopped:
		return "stopped"
	case transferStarting:
		return "starting"
	case transferStarted:
		return "started"
	case transferStopping:
		return "stopping"
	default:
		return "invalid"
	}
}

// acceptsIncomingData reports whether an I-frame may arrive in this state.
func (d dataTransfer) acceptsIncomingData() bool {
	return d == transferStarted || d == transferStopping
}

// Start performs the STARTDT handshake and returns when the station has
// confirmed it.
//
// It blocks until the confirmation arrives, ctx is done, or the connection
// fails. There is no separate timeout parameter: t1 already covers an
// unanswered activation, and a second deadline for one event only raises the
// question of which one won.
//
// Calling it on a connection that is already started is not an error and sends
// nothing. Re-running the handshake would make the station discard and rebuild
// state for no reason.
func (c *Conn) Start(ctx context.Context) error {
	return c.control(ctx, apci.StartDTAct)
}

// Stop performs the STOPDT handshake: data transfer is suspended, the TCP
// connection stays open.
//
// This is the orderly half of shutdown. It exists separately from Close because
// the two mean different things to the station: STOPDT is "stand by", a closed
// socket is "I am gone, re-dial me".
func (c *Conn) Stop(ctx context.Context) error {
	return c.control(ctx, apci.StopDTAct)
}

// Shutdown ends the connection the polite way: STOPDT act, wait for the
// confirmation, then close.
//
// It closes whatever happens. A station that will not confirm STOPDT is exactly
// the station that must not be left holding a half-open connection, so the
// confirmation is something Shutdown waits for, not something it requires. The
// returned error reports how the handshake went; the socket is closed either
// way, and a caller that only cares that the connection is gone can ignore it.
func (c *Conn) Shutdown(ctx context.Context) error {
	stopErr := c.Stop(ctx)
	closeErr := c.Close()

	if stopErr != nil {
		return fmt.Errorf("stopping data transfer: %w", stopErr)
	}
	return closeErr
}

// control runs one activation/confirmation dialogue and waits for the answer.
func (c *Conn) control(ctx context.Context, act apci.UFunction) error {
	r := sendRequest{fn: act, confirm: make(chan error, 1)}

	if err := c.submit(ctx, r); err != nil {
		return err
	}

	// submit returned, so runLoop either registered the dialogue or answered it
	// outright (the already-in-that-state case, which sends nothing).
	select {
	case err := <-r.confirm:
		return err
	case <-c.done:
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- the runLoop side -------------------------------------------------------

// beginControl is called by handleSend before a control activation goes out.
//
// It reports whether handleSend should send it, and the error the caller gets
// if it must not.
func (c *Conn) beginControl(r sendRequest) (send bool, err error) {
	// Already where the caller wants to be: answer at once and send nothing.
	// This is what makes Start idempotent, which matters because the station
	// layer's reconnect path (T6.3) cannot always tell whether the handshake
	// has already run on this connection.
	switch {
	case r.fn == apci.StartDTAct && c.transfer == transferStarted,
		r.fn == apci.StopDTAct && c.transfer == transferStopped:
		r.confirm <- nil
		return false, nil
	}

	if c.pending != nil {
		return false, fmt.Errorf("%w: %s is already awaiting its confirmation",
			ErrControlBusy, c.pending.fn)
	}

	return true, nil
}

// controlSent records an activation that has just been handed to writeLoop.
func (c *Conn) controlSent(r sendRequest) {
	switch r.fn {
	case apci.StartDTAct:
		c.transfer = transferStarting
	case apci.StopDTAct:
		c.transfer = transferStopping
	}
	if r.confirm != nil {
		pending := r
		c.pending = &pending
	}
}

// controlReceived applies an incoming U-format frame to the transfer state and
// resolves whatever was waiting for it. It reports whether the loop continues.
//
// Two rules are enforced here, and both are about who is allowed to say what.
// This service is the controlling station (decision G2), so STARTDT and STOPDT
// activations travel one way only: from us. One arriving means the peer is not
// the station we think it is — a controlled station that has been misconfigured
// as a master, or two masters pointed at each other. The other rule is that a
// confirmation must answer something: an unsolicited one means the two sides
// disagree about what is outstanding, and in a protocol with no
// resynchronisation that disagreement does not heal.
func (c *Conn) controlReceived(fn apci.UFunction) bool {
	switch fn {
	case apci.StartDTAct, apci.StopDTAct:
		c.fail(fmt.Errorf("%w: the peer sent %s; only the controlling station may, and that is us",
			ErrProtocol, fn))
		return false

	case apci.TestFRAct:
		// The peer is probing us. handleFrame sends the confirmation; there is
		// no dialogue of ours to resolve.
		return true

	case apci.TestFRCon:
		// Ours went out from the t3 path, which registers no dialogue because
		// nobody is waiting on it — t1 is the supervision, and Timers has
		// already cleared it. So a TESTFR con with nothing pending is the
		// normal case, not an anomaly.
		return true
	}

	// Only STARTDT con and STOPDT con reach here, and each must answer
	// something. An unsolicited one means the two sides disagree about what is
	// outstanding, and in a protocol with no resynchronisation that
	// disagreement does not heal on its own.
	if c.pending == nil {
		c.fail(fmt.Errorf("%w: unsolicited %s with no activation outstanding", ErrProtocol, fn))
		return false
	}

	want, ok := c.pending.fn.Confirm()
	if !ok || want != fn {
		c.fail(fmt.Errorf("%w: %s answers nothing; %s was outstanding", ErrProtocol, fn, c.pending.fn))
		return false
	}

	switch fn {
	case apci.StartDTCon:
		c.transfer = transferStarted
	case apci.StopDTCon:
		c.transfer = transferStopped
	}

	c.pending.confirm <- nil
	c.pending = nil
	return true
}
