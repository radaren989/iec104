package apci

import (
	"errors"
	"testing"
)

// --- modular arithmetic -----------------------------------------------------

func TestSeqDistanceWrapsForward(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		lo, hi uint16
		want   uint16
	}{
		{"same value", 0, 0, 0},
		{"one step", 0, 1, 1},
		{"one step across the wrap", MaxSeq, 0, 1},
		{"a few steps across the wrap", MaxSeq - 2, 2, 5},
		{"the far side of the space", 0, MaxSeq, MaxSeq},
		// This is the case a naive "hi - lo" comparison gets wrong: going
		// backwards by one reads as being ahead by 32767, which is exactly what
		// makes it an invalid acknowledgement rather than a negative number.
		{"one step backwards is very far forward", 1, 0, MaxSeq},
		{"halfway", 100, 100 + seqSpace/2, seqSpace / 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := seqDistance(tc.lo, tc.hi); got != tc.want {
				t.Errorf("seqDistance(%d, %d) = %d, want %d", tc.lo, tc.hi, got, tc.want)
			}
		})
	}
}

func TestSeqAddWraps(t *testing.T) {
	t.Parallel()

	if got := seqAdd(MaxSeq, 1); got != 0 {
		t.Errorf("seqAdd(%d, 1) = %d, want 0", MaxSeq, got)
	}
	if got := seqAdd(MaxSeq, 3); got != 2 {
		t.Errorf("seqAdd(%d, 3) = %d, want 2", MaxSeq, got)
	}
}

// --- the wrap, end to end ---------------------------------------------------

// A connection that sends more than 32768 I-frames must keep working. The bug
// this guards against does not show up in any smaller test: it waits until the
// counter turns over, which on a busy station is hours into production.
func TestCountersSurviveWraparound(t *testing.T) {
	t.Parallel()

	var s Sequencer

	// Send and acknowledge one frame at a time, twice around the space plus a
	// bit, so both the send counter and the ack window cross zero.
	const rounds = 2*seqSpace + 17
	for i := range rounds {
		want := uint16(i % seqSpace)

		ns, nr := s.NextSend()
		if ns != want {
			t.Fatalf("round %d: N(S) = %d, want %d", i, ns, want)
		}
		if nr != 0 {
			t.Fatalf("round %d: N(R) = %d, want 0 (nothing received)", i, nr)
		}
		if got := s.Outstanding(); got != 1 {
			t.Fatalf("round %d: outstanding = %d, want 1", i, got)
		}

		cleared, err := s.Acknowledge(seqAdd(ns, 1))
		if err != nil {
			t.Fatalf("round %d: Acknowledge: %v", i, err)
		}
		if cleared != 1 {
			t.Fatalf("round %d: cleared = %d, want 1", i, cleared)
		}
		if got := s.Outstanding(); got != 0 {
			t.Fatalf("round %d: outstanding = %d, want 0", i, got)
		}
	}
}

// The same for the receive direction.
func TestReceiveCounterSurvivesWraparound(t *testing.T) {
	t.Parallel()

	var s Sequencer

	const rounds = seqSpace + 5
	for i := range rounds {
		ns := uint16(i % seqSpace)
		if err := s.Receive(ns, 0); err != nil {
			t.Fatalf("round %d: Receive(%d): %v", i, ns, err)
		}
		if want := seqAdd(ns, 1); s.RecvSeq() != want {
			t.Fatalf("round %d: RecvSeq = %d, want %d", i, s.RecvSeq(), want)
		}
	}
}

// --- the send window --------------------------------------------------------

func TestOutstandingTracksTheWindow(t *testing.T) {
	t.Parallel()

	var s Sequencer

	for i := uint16(1); i <= 12; i++ {
		s.NextSend()
		if got := s.Outstanding(); got != i {
			t.Fatalf("after %d sends: outstanding = %d", i, got)
		}
	}

	// A partial acknowledgement clears only what it covers.
	cleared, err := s.Acknowledge(5)
	if err != nil {
		t.Fatalf("Acknowledge(5): %v", err)
	}
	if cleared != 5 {
		t.Errorf("cleared = %d, want 5", cleared)
	}
	if got := s.Outstanding(); got != 7 {
		t.Errorf("outstanding = %d, want 7", got)
	}

	// Repeating it clears nothing and is not an error: an S-frame that
	// acknowledges what was already acknowledged is idle chatter.
	cleared, err = s.Acknowledge(5)
	if err != nil {
		t.Fatalf("repeated Acknowledge(5): %v", err)
	}
	if cleared != 0 {
		t.Errorf("repeated ack cleared = %d, want 0", cleared)
	}

	// Acknowledging everything empties the window.
	if _, err := s.Acknowledge(12); err != nil {
		t.Fatalf("Acknowledge(12): %v", err)
	}
	if got := s.Outstanding(); got != 0 {
		t.Errorf("outstanding = %d, want 0", got)
	}
}

// --- rejection --------------------------------------------------------------

func TestAcknowledgeRejectsImpossibleValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sent int    // I-frames sent before the ack
		ack  uint16 // the N(R) the peer sends
		want error
	}{
		{"ack of a frame never sent", 3, 4, ErrInvalidAck},
		{"ack far in the future", 3, 1000, ErrInvalidAck},
		{"ack when nothing is outstanding", 0, 1, ErrInvalidAck},
		{"ack above the 15-bit range", 3, MaxSeq + 1, ErrSeqRange},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var s Sequencer
			for range tc.sent {
				s.NextSend()
			}
			if _, err := s.Acknowledge(tc.ack); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// An acknowledgement that moves the window backwards is caught by the same
// check: modular distance reads "one step back" as "32767 steps forward", which
// is outside any real window.
func TestAcknowledgeRejectsBackwardsMovement(t *testing.T) {
	t.Parallel()

	var s Sequencer
	for range 10 {
		s.NextSend()
	}
	if _, err := s.Acknowledge(6); err != nil {
		t.Fatalf("Acknowledge(6): %v", err)
	}
	if _, err := s.Acknowledge(3); !errors.Is(err, ErrInvalidAck) {
		t.Fatalf("moving the window back to 3 must be rejected, got %v", err)
	}
	// The rejected acknowledgement must not have moved anything.
	if got := s.Outstanding(); got != 4 {
		t.Errorf("outstanding = %d after a rejected ack, want 4", got)
	}
}

// A window that is full right up to the wrap must still reject an ack one past
// the last frame sent. This is the case where a "<=" written against raw values
// silently lets a bogus ack through.
func TestAcknowledgeRejectsFutureAckAcrossTheWrap(t *testing.T) {
	t.Parallel()

	var s Sequencer
	// Walk the counters to just below the wrap.
	for range MaxSeq - 2 {
		ns, _ := s.NextSend()
		if _, err := s.Acknowledge(seqAdd(ns, 1)); err != nil {
			t.Fatalf("priming: %v", err)
		}
	}
	// Send three, straddling the wrap: 32765, 32766, 32767.
	for range 3 {
		s.NextSend()
	}
	if got := s.SendSeq(); got != 0 {
		t.Fatalf("send counter did not wrap: %d", got)
	}
	// Acknowledging all three is fine; one more is a frame never sent.
	if _, err := s.Acknowledge(0); err != nil {
		t.Fatalf("Acknowledge(0) across the wrap: %v", err)
	}
	if _, err := s.Acknowledge(1); !errors.Is(err, ErrInvalidAck) {
		t.Fatalf("ack past the wrap must be rejected, got %v", err)
	}
}

func TestReceiveRejectsUnexpectedSeq(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		prime int    // I-frames received first
		ns    uint16 // the N(S) of the offending frame
		want  error
	}{
		{"a gap", 3, 4, ErrUnexpectedSeq},
		{"a repeat", 3, 2, ErrUnexpectedSeq},
		{"the very first frame numbered 1", 0, 1, ErrUnexpectedSeq},
		{"above the 15-bit range", 0, MaxSeq + 1, ErrSeqRange},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var s Sequencer
			for i := range tc.prime {
				if err := s.Receive(uint16(i), 0); err != nil {
					t.Fatalf("priming: %v", err)
				}
			}
			if err := s.Receive(tc.ns, 0); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// An incoming I-frame carries an acknowledgement too, and a bad one closes the
// connection even though the N(S) was correct.
func TestReceiveRejectsBadPiggybackedAck(t *testing.T) {
	t.Parallel()

	var s Sequencer
	s.NextSend() // one frame outstanding: N(S)=0

	if err := s.Receive(0, 5); !errors.Is(err, ErrInvalidAck) {
		t.Fatalf("got %v, want ErrInvalidAck", err)
	}
	// The receive counter must not have advanced on a doomed connection.
	if got := s.RecvSeq(); got != 0 {
		t.Errorf("RecvSeq = %d after a rejected frame, want 0", got)
	}
}

// --- acknowledgement of the receive direction -------------------------------

// Every frame this side sends carries N(R), so sending anything clears the
// received-but-unacknowledged counter. This is why a busy link almost never
// needs a standalone S-frame.
func TestSendingClearsTheReceivedCounter(t *testing.T) {
	t.Parallel()

	t.Run("an I-frame carries the acknowledgement", func(t *testing.T) {
		t.Parallel()

		var s Sequencer
		for i := range 3 {
			if err := s.Receive(uint16(i), 0); err != nil {
				t.Fatalf("Receive: %v", err)
			}
		}
		if got := s.UnackedReceived(); got != 3 {
			t.Fatalf("unacked received = %d, want 3", got)
		}

		_, nr := s.NextSend()
		if nr != 3 {
			t.Errorf("outgoing N(R) = %d, want 3", nr)
		}
		if got := s.UnackedReceived(); got != 0 {
			t.Errorf("unacked received = %d after sending, want 0", got)
		}
	})

	t.Run("an S-frame does not consume a send number", func(t *testing.T) {
		t.Parallel()

		var s Sequencer
		if err := s.Receive(0, 0); err != nil {
			t.Fatalf("Receive: %v", err)
		}
		before := s.SendSeq()

		if nr := s.SendSupervisory(); nr != 1 {
			t.Errorf("S-frame N(R) = %d, want 1", nr)
		}
		if s.SendSeq() != before {
			t.Errorf("S-frame consumed a send number: %d -> %d", before, s.SendSeq())
		}
		if got := s.UnackedReceived(); got != 0 {
			t.Errorf("unacked received = %d after an S-frame, want 0", got)
		}
	})
}

// --- the zero value ---------------------------------------------------------

func TestZeroSequencerDescribesAFreshConnection(t *testing.T) {
	t.Parallel()

	var s Sequencer
	if got := s.SendSeq(); got != 0 {
		t.Errorf("SendSeq = %d", got)
	}
	if got := s.RecvSeq(); got != 0 {
		t.Errorf("RecvSeq = %d", got)
	}
	if got := s.Outstanding(); got != 0 {
		t.Errorf("Outstanding = %d", got)
	}
	if got := s.UnackedReceived(); got != 0 {
		t.Errorf("UnackedReceived = %d", got)
	}
}

// --- a full exchange --------------------------------------------------------

// Two Sequencers driven against each other must stay in agreement, including
// across the wrap. This is the closest a pure test gets to a real link.
func TestTwoPeersStayInAgreement(t *testing.T) {
	t.Parallel()

	var master, station Sequencer

	send := func(t *testing.T, from, to *Sequencer, label string) {
		t.Helper()
		ns, nr := from.NextSend()
		if err := to.Receive(ns, nr); err != nil {
			t.Fatalf("%s: peer rejected N(S)=%d N(R)=%d: %v", label, ns, nr, err)
		}
	}

	// Enough traffic in both directions to take both counters past the wrap.
	for i := range seqSpace + 100 {
		send(t, &master, &station, "master->station")
		send(t, &station, &master, "station->master")

		if i%1000 == 0 {
			// Standalone acknowledgements in both directions.
			if _, err := master.Acknowledge(station.RecvSeq()); err != nil {
				t.Fatalf("round %d: master ack: %v", i, err)
			}
			if _, err := station.Acknowledge(master.RecvSeq()); err != nil {
				t.Fatalf("round %d: station ack: %v", i, err)
			}
		}
	}

	if master.RecvSeq() != station.SendSeq() {
		t.Errorf("master expects %d, station will send %d", master.RecvSeq(), station.SendSeq())
	}
	if station.RecvSeq() != master.SendSeq() {
		t.Errorf("station expects %d, master will send %d", station.RecvSeq(), master.SendSeq())
	}
}
