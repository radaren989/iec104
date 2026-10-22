package conn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/clock"
)

// --- the handshake ----------------------------------------------------------

func TestStartSendsTheActivationAndWaitsForTheConfirmation(t *testing.T) {
	t.Parallel()

	c, p := pair(t, testConfig(t, newRecorder()))

	started := make(chan error, 1)
	go func() { started <- c.Start(t.Context()) }()

	f := p.expect(2 * time.Second)
	if f.Format != apci.FormatU || f.Function != apci.StartDTAct {
		t.Fatalf("got %s, want STARTDT act", f)
	}

	// Nothing has confirmed yet, so Start must still be waiting. This is the
	// half of the handshake that is easy to get wrong and impossible to notice:
	// a Start that returned here would report success on a station that has not
	// agreed to send anything.
	select {
	case err := <-started:
		t.Fatalf("Start returned before the confirmation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	p.send(apci.NewU(apci.StartDTCon))
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start never returned")
	}
}

func TestStartOnAStartedConnectionSendsNothing(t *testing.T) {
	t.Parallel()

	c, p := startedPair(t, testConfig(t, newRecorder()))

	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	// A second activation would be answered by a second confirmation, and there
	// is nothing in the frame to say which activation it answers.
	p.expectNothing(150 * time.Millisecond)
}

func TestStopSuspendsDataTransferWithoutClosing(t *testing.T) {
	t.Parallel()

	c, p := startedPair(t, testConfig(t, newRecorder()))

	stopped := make(chan error, 1)
	go func() { stopped <- c.Stop(t.Context()) }()

	if f := p.expect(2 * time.Second); f.Function != apci.StopDTAct {
		t.Fatalf("got %s, want STOPDT act", f)
	}
	p.send(apci.NewU(apci.StopDTCon))

	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Suspended, not closed: the socket is still there.
	if err := c.Err(); err != nil {
		t.Fatalf("STOPDT must not end the connection: %v", err)
	}
	asdu := []byte{0x64, 0x01, 0x06, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x14}
	if err := c.SendASDU(t.Context(), asdu); !errors.Is(err, ErrNotStarted) {
		t.Errorf("SendASDU after STOPDT: got %v, want ErrNotStarted", err)
	}
	if err := c.Err(); err != nil {
		t.Errorf("a rejected send must not end the connection: %v", err)
	}
}

func TestShutdownStopsThenCloses(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := startedPair(t, testConfig(t, rec))

	done := make(chan error, 1)
	go func() { done <- c.Shutdown(t.Context()) }()

	if f := p.expect(2 * time.Second); f.Function != apci.StopDTAct {
		t.Fatalf("got %s, want STOPDT act", f)
	}
	p.send(apci.NewU(apci.StopDTCon))

	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-rec.closed:
		if err == nil {
			t.Fatal("OnClosed fired with nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connection was not closed")
	}
}

// A station that will not confirm STOPDT is exactly the one that must not be
// left holding a half-open connection. Shutdown reports the failure and closes
// anyway.
func TestShutdownClosesEvenWhenTheStationWillNotConfirm(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := startedPair(t, testConfig(t, rec))

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Shutdown(ctx) }()

	if f := p.expect(2 * time.Second); f.Function != apci.StopDTAct {
		t.Fatalf("got %s, want STOPDT act", f)
	}
	// No confirmation, on purpose.

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown never returned")
	}
	select {
	case err := <-rec.closed:
		if err == nil {
			t.Fatal("OnClosed fired with nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown must close even when STOPDT goes unanswered")
	}
}

// --- what the handshake protects --------------------------------------------

// This is the rule the whole file exists for (Study Reference §4): before
// STARTDT con the controlled station sends no I-frame at all. One arriving means
// it is not running the handshake we think it is.
func TestAnIFrameBeforeTheHandshakeEndsTheConnection(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := pair(t, testConfig(t, rec))

	p.send(apci.NewI(0, 0, []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}))

	select {
	case err := <-rec.closed:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("got %v, want ErrProtocol", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connection stayed up")
	}
	if n := rec.asduCount(); n != 0 {
		t.Errorf("%d ASDU(s) were delivered; a frame that should not exist must not reach the handler", n)
	}
	if !errors.Is(c.Err(), ErrProtocol) {
		t.Errorf("Err() = %v", c.Err())
	}
}

func TestSendingBeforeTheHandshakeIsRejectedButKeepsTheConnection(t *testing.T) {
	t.Parallel()

	c, p := pair(t, testConfig(t, newRecorder()))

	asdu := []byte{0x64, 0x01, 0x06, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x14}
	if err := c.SendASDU(t.Context(), asdu); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v, want ErrNotStarted", err)
	}

	// The caller made a mistake; the link did not. Collapsing the two would mean
	// one early send from the station layer dropped a healthy connection.
	if err := c.Err(); err != nil {
		t.Errorf("the connection ended: %v", err)
	}
	p.expectNothing(150 * time.Millisecond)
}

// Frames already on the wire when our STOPDT act reaches the station are not a
// violation — they are the speed of light. Rejecting them would turn an orderly
// shutdown into a protocol error on a link that did nothing wrong.
func TestIFramesStillArriveWhileStopping(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := startedPair(t, testConfig(t, rec))

	stopped := make(chan error, 1)
	go func() { stopped <- c.Stop(t.Context()) }()

	if f := p.expect(2 * time.Second); f.Function != apci.StopDTAct {
		t.Fatalf("got %s, want STOPDT act", f)
	}

	// In flight before the station saw the activation.
	p.send(apci.NewI(0, 0, []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}))
	rec.waitASDUs(t, 1)

	p.send(apci.NewU(apci.StopDTCon))
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After the confirmation the same frame is a violation.
	p.send(apci.NewI(1, 0, []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}))
	select {
	case err := <-rec.closed:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("got %v, want ErrProtocol", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an I-frame after STOPDT con must end the connection")
	}
}

// --- who may say what -------------------------------------------------------

// We are the controlling station (G2), so STARTDT and STOPDT activations travel
// one way only. One arriving means the peer is not what we think it is: a
// controlled station misconfigured as a master, or two masters pointed at each
// other.
func TestActivationsFromThePeerEndTheConnection(t *testing.T) {
	t.Parallel()

	for _, fn := range []apci.UFunction{apci.StartDTAct, apci.StopDTAct} {
		t.Run(fn.String(), func(t *testing.T) {
			t.Parallel()

			rec := newRecorder()
			c, p := pair(t, testConfig(t, rec))
			p.send(apci.NewU(fn))

			select {
			case err := <-rec.closed:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("got %v, want ErrProtocol", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the connection stayed up")
			}
			if !errors.Is(c.Err(), ErrProtocol) {
				t.Errorf("Err() = %v", c.Err())
			}
		})
	}
}

// A confirmation must answer something. An unsolicited one means the two sides
// disagree about what is outstanding, and 104 has no way to resynchronise.
func TestAnUnsolicitedConfirmationEndsTheConnection(t *testing.T) {
	t.Parallel()

	for _, fn := range []apci.UFunction{apci.StartDTCon, apci.StopDTCon} {
		t.Run(fn.String(), func(t *testing.T) {
			t.Parallel()

			rec := newRecorder()
			_, p := pair(t, testConfig(t, rec))
			p.send(apci.NewU(fn))

			select {
			case err := <-rec.closed:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("got %v, want ErrProtocol", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the connection stayed up")
			}
		})
	}
}

// The wrong confirmation is as bad as none: answering STARTDT with STOPDT con
// would leave the two sides with opposite ideas of whether data may flow.
func TestAMismatchedConfirmationEndsTheConnection(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := pair(t, testConfig(t, rec))

	go func() { _ = c.Start(t.Context()) }()
	if f := p.expect(2 * time.Second); f.Function != apci.StartDTAct {
		t.Fatalf("got %s, want STARTDT act", f)
	}

	p.send(apci.NewU(apci.StopDTCon))

	select {
	case err := <-rec.closed:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("got %v, want ErrProtocol", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connection stayed up")
	}
}

// Our own TESTFR act comes from the t3 path, which registers no dialogue, so its
// confirmation arrives with nothing pending. That is the normal case and must
// not be mistaken for the unsolicited one above.
func TestATestFRConfirmationIsNotUnsolicited(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := startedPair(t, testConfig(t, rec))

	p.send(apci.NewU(apci.TestFRCon))

	// Still alive, and the handler heard about it.
	select {
	case err := <-rec.closed:
		t.Fatalf("the connection ended: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := c.Err(); err != nil {
		t.Errorf("Err() = %v", err)
	}
}

func TestASecondControlDialogueIsRejected(t *testing.T) {
	t.Parallel()

	c, p := pair(t, testConfig(t, newRecorder()))

	go func() { _ = c.Start(t.Context()) }()
	if f := p.expect(2 * time.Second); f.Function != apci.StartDTAct {
		t.Fatalf("got %s, want STARTDT act", f)
	}

	// The first is still outstanding. A second activation would be answered by
	// a confirmation that names neither of them.
	if err := c.Stop(t.Context()); !errors.Is(err, ErrControlBusy) {
		t.Fatalf("got %v, want ErrControlBusy", err)
	}
	if err := c.Err(); err != nil {
		t.Errorf("a rejected dialogue must not end the connection: %v", err)
	}
	p.expectNothing(150 * time.Millisecond)
}

// --- t1 ---------------------------------------------------------------------

// armed waits until runLoop's timer is registered with the fake clock, and must
// be called before every Advance.
//
// The reason is a race that is invisible when it works and total when it does
// not. runLoop computes its wait from Clock.Now() and then arms the timer with
// it. A fake clock only moves when a test says so, so an Advance landing between
// those two steps produces a deadline measured from the NEW instant — the loop
// parks past the moment the test was trying to reach, nothing ever wakes it, and
// the test hangs until its socket read times out.
//
// Waiting for the waiter to exist closes it: rearm removes the waiter before
// re-adding it, so a count of one means either the loop is parked with a current
// deadline or it has just finished arming one. Neither can be invalidated by the
// Advance that follows. And if the Advance lands before the loop's first Check,
// that Check simply sees the later instant and acts at once.
func armed(f *clock.Fake) { f.BlockUntil(1) }

// An unanswered activation is covered by t1, which is the whole reason the
// handshake does not carry a timeout of its own. Without this the connection
// would sit in "starting" forever: TCP up, no data, nothing in the log.
func TestAnUnansweredActivationClosesTheConnectionAtT1(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	fake := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	cfg := testConfig(t, rec)
	cfg.Clock = fake

	c, p := pair(t, cfg)

	go func() { _ = c.Start(t.Context()) }()
	if f := p.expect(2 * time.Second); f.Function != apci.StartDTAct {
		t.Fatalf("got %s, want STARTDT act", f)
	}

	// No confirmation. t1 is the only thing standing between this and a
	// connection that never reports anything.
	armed(fake)
	fake.Advance(cfg.Params.T1 + time.Second)

	select {
	case err := <-rec.closed:
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("got %v, want ErrTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("t1 did not close the connection")
	}
}

// The same probe, from the other trigger: t3 fires on an idle link, t1 covers
// the TESTFR it sends, and a station that has silently died is closed one t1
// later instead of lingering.
func TestAnUnansweredTestFRClosesTheConnectionAtT1(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	fake := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	cfg := testConfig(t, rec)
	cfg.Clock = fake

	c, p := pair(t, cfg)

	armed(fake)
	fake.Advance(cfg.Params.T3 + time.Millisecond)
	if f := p.expect(2 * time.Second); f.Function != apci.TestFRAct {
		t.Fatalf("got %s, want TESTFR act", f)
	}

	armed(fake)
	fake.Advance(cfg.Params.T1 + time.Second)
	select {
	case err := <-rec.closed:
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("got %v, want ErrTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("t1 did not close the connection")
	}
	if err := c.Err(); !errors.Is(err, ErrTimeout) {
		t.Errorf("Err() = %v", err)
	}
}
