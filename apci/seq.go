package apci

import (
	"errors"
	"fmt"
)

// seqSpace is the size of the sequence-number space. N(S) and N(R) are 15-bit
// counters, so they wrap from 32767 back to 0.
//
// This wrap is the single nastiest bug class in a 104 implementation. An
// implementation that compares sequence numbers with < or > instead of modular
// distance works perfectly for the first 32768 I-frames — hours or days,
// depending on the station — and then the connection starts dropping for no
// visible reason. Nothing in the logs points at arithmetic. Every comparison in
// this file therefore goes through seqDistance.
const seqSpace = MaxSeq + 1

// Sequence-number errors. Both of them mean the peer and this side no longer
// agree on what has been sent, and the standard's answer to that is to close the
// connection: there is no resynchronisation short of a new STARTDT handshake.
var (
	// ErrUnexpectedSeq means the peer's N(S) is not the one expected — a frame
	// was lost, duplicated or reordered. TCP does not reorder, so in practice
	// this means the peer's counter is wrong or a previous frame was misparsed.
	ErrUnexpectedSeq = errors.New("unexpected N(S)")

	// ErrInvalidAck means the peer acknowledged something outside the window of
	// frames actually outstanding: either a frame never sent ("an ack from the
	// future") or one already acknowledged, which would move the window
	// backwards.
	ErrInvalidAck = errors.New("invalid N(R)")
)

// seqAdd advances a sequence number by n, wrapping.
func seqAdd(seq uint16, n uint16) uint16 {
	return (seq + n) % seqSpace
}

// seqDistance reports how many steps forward it is from lo to hi, wrapping.
//
// This is the only correct way to compare two sequence numbers: 0 is "ahead of"
// 32767 by one step, not behind it by 32767. The result is always in
// 0..MaxSeq, so "is hi within w of lo" becomes seqDistance(lo, hi) <= w and
// stays true across the wrap.
func seqDistance(lo, hi uint16) uint16 {
	return (hi - lo) % seqSpace
}

// Sequencer tracks the numbered state of one connection: what has been sent,
// what has been acknowledged, and what is expected next.
//
// It is pure state and arithmetic — no locking, no timers, no I/O. It is NOT
// safe for concurrent use: the connection layer owns exactly one Sequencer and
// touches it from a single goroutine. Other goroutines reach it by channel, not by pointer.
//
// The zero Sequencer is ready to use and describes a freshly established
// connection: nothing sent, nothing received, nothing outstanding.
type Sequencer struct {
	// sendSeq is the N(S) the next outgoing I-frame will carry.
	sendSeq uint16

	// ackLow is the oldest N(S) the peer has not yet acknowledged. Everything
	// in [ackLow, sendSeq) is in flight.
	ackLow uint16

	// recvSeq is the N(S) the next incoming I-frame must carry. It is also the
	// N(R) this side puts in every frame it sends, because acknowledging
	// "everything below recvSeq" is exactly what N(R) means.
	recvSeq uint16

	// unackedRecv counts I-frames received since the last S- or I-frame this
	// side sent. The w window (T4.3) reads it to decide when an S-frame is due.
	unackedRecv uint16
}

// Outstanding reports how many sent I-frames are still unacknowledged. The k
// window (T4.3) compares this against its limit.
func (s *Sequencer) Outstanding() uint16 {
	return seqDistance(s.ackLow, s.sendSeq)
}

// UnackedReceived reports how many received I-frames have not yet been
// acknowledged by anything this side sent. The w window reads it.
func (s *Sequencer) UnackedReceived() uint16 {
	return s.unackedRecv
}

// SendSeq is the N(S) the next outgoing I-frame will carry, without consuming it.
func (s *Sequencer) SendSeq() uint16 { return s.sendSeq }

// RecvSeq is the N(R) to put in outgoing frames: everything below it has been
// received. It is also the N(S) the next incoming I-frame must carry.
func (s *Sequencer) RecvSeq() uint16 { return s.recvSeq }

// NextSend consumes the next N(S) for an outgoing I-frame and returns the frame
// header values to use.
//
// It does NOT check the k window; that is the sender's job, because the sender
// has to decide what to do when the window is full (block) and this package
// must not make that decision for it. Calling NextSend with a full window is a
// caller bug, not a protocol error.
func (s *Sequencer) NextSend() (sendSeq, recvSeq uint16) {
	sendSeq = s.sendSeq
	s.sendSeq = seqAdd(s.sendSeq, 1)

	// Sending anything acknowledges what has been received, so the w counter
	// resets here too. This is why a busy link almost never needs an S-frame:
	// every I-frame carries the acknowledgement along.
	s.unackedRecv = 0
	return sendSeq, s.recvSeq
}

// SendSupervisory records that an S-frame is being sent and returns its N(R).
// Like NextSend it clears the received-but-unacknowledged counter, and unlike it
// it does not consume a send sequence number — S-frames are not numbered.
func (s *Sequencer) SendSupervisory() (recvSeq uint16) {
	s.unackedRecv = 0
	return s.recvSeq
}

// Receive records an incoming I-frame carrying sendSeq as its N(S) and recvSeq
// as its N(R).
//
// Both halves are validated: the N(S) must be exactly the expected one, and the
// N(R) is an acknowledgement handled as in Acknowledge. An error means the
// connection must be closed.
func (s *Sequencer) Receive(sendSeq, recvSeq uint16) error {
	if sendSeq > MaxSeq {
		return fmt.Errorf("%w: N(S)=%d exceeds %d", ErrSeqRange, sendSeq, MaxSeq)
	}
	if sendSeq != s.recvSeq {
		return fmt.Errorf("%w: got %d, expected %d", ErrUnexpectedSeq, sendSeq, s.recvSeq)
	}
	// The acknowledgement is applied first: if it is invalid the connection is
	// finished anyway, and leaving the receive counter advanced on a doomed
	// connection makes the failure harder to read in a test.
	if _, err := s.Acknowledge(recvSeq); err != nil {
		return err
	}
	s.recvSeq = seqAdd(s.recvSeq, 1)
	s.unackedRecv++
	return nil
}

// Acknowledge applies an N(R) received from the peer, in an I- or S-frame, and
// reports how many outstanding frames it cleared.
//
// A repeated N(R) clears nothing and is legal: an S-frame that acknowledges what
// was already acknowledged is idle chatter, not an error. Anything outside the
// outstanding window is a protocol violation — including an N(R) that would move
// the window backwards, which the modular distance catches as "far in the
// future" rather than as a negative number.
func (s *Sequencer) Acknowledge(recvSeq uint16) (cleared uint16, err error) {
	if recvSeq > MaxSeq {
		return 0, fmt.Errorf("%w: N(R)=%d exceeds %d", ErrSeqRange, recvSeq, MaxSeq)
	}
	advance := seqDistance(s.ackLow, recvSeq)
	if advance > s.Outstanding() {
		return 0, fmt.Errorf("%w: N(R)=%d acknowledges %d frame(s) but only %d are outstanding (window [%d,%d))",
			ErrInvalidAck, recvSeq, advance, s.Outstanding(), s.ackLow, s.sendSeq)
	}
	s.ackLow = recvSeq
	return advance, nil
}
