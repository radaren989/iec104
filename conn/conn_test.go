package conn

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/clock"
)

// --- a peer to talk to ------------------------------------------------------

// peer is the other end of the connection: a plain socket the test drives by
// hand, so that what the code under test sees is real bytes from a real socket
// rather than a mock of one.
type peer struct {
	t    *testing.T
	sock net.Conn
	buf  []byte
}

// pair returns a connection and the peer socket on the other end of it.
//
// A real TCP pair rather than net.Pipe: net.Pipe is unbuffered and synchronous,
// so a test that writes two frames before reading deadlocks in a way that has
// nothing to do with the code being tested.
func pair(t *testing.T, cfg Config) (*Conn, *peer) {
	t.Helper()

	ctx := t.Context()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type accepted struct {
		sock net.Conn
		err  error
	}
	acceptCh := make(chan accepted, 1)
	go func() {
		s, err := ln.Accept()
		acceptCh <- accepted{s, err}
	}()

	var d net.Dialer
	client, err := d.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	a := <-acceptCh
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}

	c, err := New(client, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := &peer{t: t, sock: a.sock}

	t.Cleanup(func() {
		_ = c.Close()
		_ = p.sock.Close()
	})
	return c, p
}

// startedPair returns a connection that has completed the STARTDT handshake,
// which is what every test about data transfer needs.
//
// It exists as a separate helper rather than being folded into pair because the
// handshake is not a setup detail to be hidden: a connection that has not run it
// carries no I-frames in either direction, and the tests below that DO use the
// raw pair are testing exactly that.
func startedPair(t *testing.T, cfg Config) (*Conn, *peer) {
	t.Helper()

	c, p := pair(t, cfg)

	// Start blocks until the confirmation arrives, and the confirmation cannot
	// arrive until this test sends it, so the call has to run alongside.
	started := make(chan error, 1)
	go func() { started <- c.Start(t.Context()) }()

	if f := p.expect(2 * time.Second); f.Format != apci.FormatU || f.Function != apci.StartDTAct {
		t.Fatalf("expected STARTDT act, got %s", f)
	}
	p.send(apci.NewU(apci.StartDTCon))

	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	return c, p
}

// send writes one frame from the peer to the connection.
func (p *peer) send(f apci.Frame) {
	p.t.Helper()
	raw, err := f.Encode()
	if err != nil {
		p.t.Fatalf("encoding %s: %v", f, err)
	}
	if _, err := p.sock.Write(raw); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

// socketDeadline is the wall-clock instant a socket operation must finish by.
//
// It is the one place in this file that reads the real clock, and the reason is
// the same one conn.deadline() gives: SetReadDeadline is enforced by the kernel,
// which has never heard of cfg.Clock. Handing it a simulated instant would set a
// deadline decades out and the read would either never time out or time out at
// once. This is not timer logic being hidden from tests — it is the timeout on
// the test's own socket.
//
//nolint:forbidigo // §2.3 bans time.Now so that timer LOGIC stays testable.
func socketDeadline(within time.Duration) time.Time {
	return time.Now().Add(within)
}

// expect reads one frame the connection sent, failing if none arrives.
func (p *peer) expect(within time.Duration) apci.Frame {
	p.t.Helper()

	if err := p.sock.SetReadDeadline(socketDeadline(within)); err != nil {
		p.t.Fatalf("peer deadline: %v", err)
	}

	for {
		n, err := apci.NextFrameLen(p.buf)
		if err == nil && len(p.buf) >= n {
			f, err := apci.Decode(p.buf[:n])
			if err != nil {
				p.t.Fatalf("peer decode: %v", err)
			}
			p.buf = append(p.buf[:0], p.buf[n:]...)
			return f
		}
		if err != nil && !errors.Is(err, apci.ErrIncomplete) {
			p.t.Fatalf("peer framing: %v (% X)", err, p.buf)
		}

		chunk := make([]byte, 4*apci.MaxFrameLen)
		got, err := p.sock.Read(chunk)
		if got > 0 {
			p.buf = append(p.buf, chunk[:got]...)
			continue
		}
		p.t.Fatalf("peer expected a frame within %s: %v", within, err)
	}
}

// expectNothing fails if the connection sends anything within the window.
func (p *peer) expectNothing(within time.Duration) {
	p.t.Helper()

	if err := p.sock.SetReadDeadline(socketDeadline(within)); err != nil {
		p.t.Fatalf("peer deadline: %v", err)
	}
	chunk := make([]byte, 64)
	n, err := p.sock.Read(chunk)
	if n > 0 {
		p.t.Fatalf("expected silence, got % X", chunk[:n])
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		p.t.Fatalf("expected a read timeout, got %v", err)
	}
}

// --- a handler that records ------------------------------------------------

type recorder struct {
	mu    sync.Mutex
	asdus [][]byte
	us    []apci.UFunction

	closed chan error

	// arrived is pulsed after every delivery. It is what lets waitASDUs block
	// on the event instead of polling for it: a test that sleeps in a loop
	// passes on a fast machine and flakes on a loaded one.
	arrived chan struct{}
}

func newRecorder() *recorder {
	return &recorder{
		closed:  make(chan error, 1),
		arrived: make(chan struct{}, 1),
	}
}

func (r *recorder) OnASDU(raw []byte) {
	r.mu.Lock()
	r.asdus = append(r.asdus, raw)
	r.mu.Unlock()

	// Non-blocking: OnASDU runs on runLoop, and a handler that blocks there
	// stops the goroutine that answers t1 (§3.2). A pulse that nobody is
	// waiting for is dropped; the waiter re-reads the count either way, so a
	// dropped pulse costs nothing.
	select {
	case r.arrived <- struct{}{}:
	default:
	}
}

func (r *recorder) OnU(fn apci.UFunction) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.us = append(r.us, fn)
}

func (r *recorder) OnClosed(err error) { r.closed <- err }

func (r *recorder) asduCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asdus)
}

// waitASDUs blocks until n ASDUs have been delivered, or fails.
//
// The timeout is a failure bound, not a synchronisation step: on a machine that
// works, this returns as soon as the nth delivery pulses arrived.
func (r *recorder) waitASDUs(t *testing.T, n int) [][]byte {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for {
		if r.asduCount() >= n {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.asdus
		}
		select {
		case <-r.arrived:
		case <-timeout.C:
			t.Fatalf("only %d of %d ASDUs arrived", r.asduCount(), n)
			return nil
		}
	}
}

func testConfig(t *testing.T, h Handler) Config {
	t.Helper()
	return Config{
		Params:  apci.DefaultParams(),
		Clock:   clock.System(),
		Handler: h,
	}
}

// --- the basics -------------------------------------------------------------

func TestNewRejectsAnUnusableConfig(t *testing.T) {
	t.Parallel()

	sock, other := net.Pipe()
	defer func() { _ = sock.Close(); _ = other.Close() }()

	cases := []struct {
		name string
		cfg  Config
	}{
		{"no clock", Config{Params: apci.DefaultParams(), Handler: newRecorder()}},
		{"no handler", Config{Params: apci.DefaultParams(), Clock: clock.System()}},
		{"parameters that cannot work", Config{
			Params:  apci.Params{K: 4, W: 4, T0: time.Second, T1: time.Second, T2: 2 * time.Second, T3: time.Second},
			Clock:   clock.System(),
			Handler: newRecorder(),
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := New(sock, tc.cfg); err == nil {
				t.Fatal("must be refused")
			}
		})
	}

	if _, err := New(nil, testConfig(t, newRecorder())); !errors.Is(err, ErrConfig) {
		t.Errorf("a nil socket: got %v, want ErrConfig", err)
	}
}

func TestReceivesAnASDU(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := startedPair(t, testConfig(t, rec))

	want := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}
	p.send(apci.NewI(0, 0, want))

	got := rec.waitASDUs(t, 1)
	if string(got[0]) != string(want) {
		t.Errorf("ASDU:\n got % X\nwant % X", got[0], want)
	}
	if err := c.Err(); err != nil {
		t.Errorf("the connection should still be healthy: %v", err)
	}
}

func TestSendsAnASDU(t *testing.T) {
	t.Parallel()

	c, p := startedPair(t, testConfig(t, newRecorder()))

	asdu := []byte{0x64, 0x01, 0x06, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x14}
	if err := c.SendASDU(context.Background(), asdu); err != nil {
		t.Fatalf("SendASDU: %v", err)
	}

	f := p.expect(2 * time.Second)
	if f.Format != apci.FormatI {
		t.Fatalf("format = %s, want I", f.Format)
	}
	if f.SendSeq != 0 || f.RecvSeq != 0 {
		t.Errorf("N(S)=%d N(R)=%d, want 0/0", f.SendSeq, f.RecvSeq)
	}
	if string(f.ASDU) != string(asdu) {
		t.Errorf("ASDU:\n got % X\nwant % X", f.ASDU, asdu)
	}
}

// Sequence numbers must advance across sends, and the acknowledgement this side
// carries must reflect what it has received.
func TestSequenceNumbersAdvance(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := startedPair(t, testConfig(t, rec))
	ctx := context.Background()
	asdu := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}

	// Three inbound frames, so our N(R) has something to say. Wait for them to
	// be delivered before sending: N(R) is allowed to lag what has arrived —
	// acknowledging later than the earliest possible moment is legal — so
	// asserting an exact value only means anything once the arrivals are known
	// to have been processed.
	for i := range 3 {
		p.send(apci.NewI(uint16(i), 0, asdu))
	}
	rec.waitASDUs(t, 3)

	for i := range 3 {
		if err := c.SendASDU(ctx, asdu); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	for i := range 3 {
		f := p.expect(2 * time.Second)
		if f.Format != apci.FormatI {
			// An S-frame may legitimately appear first if w were reached; with
			// w=8 and three frames it must not.
			t.Fatalf("frame %d: format %s, want I", i, f.Format)
		}
		if f.SendSeq != uint16(i) {
			t.Errorf("frame %d: N(S) = %d, want %d", i, f.SendSeq, i)
		}
		if f.RecvSeq != 3 {
			t.Errorf("frame %d: N(R) = %d, want 3 — three frames were received", i, f.RecvSeq)
		}
	}
}

// --- the rules --------------------------------------------------------------

// A TESTFR activation is answered by the connection itself, not by the handler:
// the answer is required, carries no information, and must go out even if
// whatever owns the handler is busy or broken.
func TestAnsweersTestFRWithoutTheHandler(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	_, p := pair(t, testConfig(t, rec))

	p.send(apci.NewU(apci.TestFRAct))

	f := p.expect(2 * time.Second)
	if f.Format != apci.FormatU || f.Function != apci.TestFRCon {
		t.Fatalf("got %s, want TESTFR con", f)
	}
}

// The w window: after w received I-frames with nothing going the other way, an
// S-frame must be sent even though no timer has expired.
func TestSupervisoryFrameAtTheWWindow(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, newRecorder())
	c, p := startedPair(t, cfg)

	asdu := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}
	for i := range int(cfg.Params.W) - 1 {
		p.send(apci.NewI(uint16(i), 0, asdu))
	}
	p.expectNothing(150 * time.Millisecond)

	p.send(apci.NewI(cfg.Params.W-1, 0, asdu))

	f := p.expect(2 * time.Second)
	if f.Format != apci.FormatS {
		t.Fatalf("got %s, want an S-frame", f)
	}
	if f.RecvSeq != cfg.Params.W {
		t.Errorf("N(R) = %d, want %d", f.RecvSeq, cfg.Params.W)
	}
	if err := c.Err(); err != nil {
		t.Errorf("connection: %v", err)
	}
}

// The k window is back pressure, and it is implemented by runLoop simply not
// accepting more work. The k+1st send must block until an acknowledgement
// arrives.
func TestSendBlocksWhenTheKWindowIsFull(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, newRecorder())
	c, p := startedPair(t, cfg)
	ctx := context.Background()
	asdu := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}

	for i := range int(cfg.Params.K) {
		if err := c.SendASDU(ctx, asdu); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	for range int(cfg.Params.K) {
		p.expect(2 * time.Second)
	}

	blocked := make(chan error, 1)
	go func() { blocked <- c.SendASDU(ctx, asdu) }()

	select {
	case err := <-blocked:
		t.Fatalf("the %dth send must not be accepted with k=%d: %v", cfg.Params.K+1, cfg.Params.K, err)
	case <-time.After(150 * time.Millisecond):
	}

	// One acknowledgement frees exactly one slot.
	p.send(apci.NewS(1))

	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("after the acknowledgement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the window did not reopen")
	}
	if f := p.expect(2 * time.Second); f.SendSeq != cfg.Params.K {
		t.Errorf("N(S) = %d, want %d", f.SendSeq, cfg.Params.K)
	}
}

// --- failure ----------------------------------------------------------------

func TestProtocolErrorsEndTheConnection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// send drives the peer into whatever mistake is under test.
		send func(p *peer)
	}{
		{
			// There is no resynchronisation in 104.
			name: "a frame that does not start with 0x68",
			send: func(p *peer) {
				if _, err := p.sock.Write([]byte{0x69, 0x04, 0x07, 0x00, 0x00, 0x00}); err != nil {
					p.t.Fatalf("peer write: %v", err)
				}
			},
		},
		{
			name: "an I-frame with the wrong N(S)",
			send: func(p *peer) {
				p.send(apci.NewI(5, 0, []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}))
			},
		},
		{
			name: "an acknowledgement of something never sent",
			send: func(p *peer) { p.send(apci.NewS(9)) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := newRecorder()
			// Started, so that each case fails for the reason it names. On a
			// stopped connection the I-frame case would be rejected for
			// arriving before STARTDT con, still return ErrProtocol, and the
			// test would pass while proving nothing about N(S).
			c, p := startedPair(t, testConfig(t, rec))
			tc.send(p)

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

// The first thing that went wrong is the reason the connection died; what
// follows are consequences of it.
func TestOnlyTheFirstErrorIsKept(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, p := pair(t, testConfig(t, rec))

	p.send(apci.NewS(9)) // an invalid acknowledgement

	select {
	case err := <-rec.closed:
		if err == nil {
			t.Fatal("OnClosed fired with nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the connection stayed up")
	}
	first := c.Err()

	// Closing afterwards must not overwrite the diagnosis with "closed".
	_ = c.Close()
	if got := c.Err(); !errors.Is(got, first) {
		t.Errorf("the error changed after Close: %v -> %v", first, got)
	}
}

func TestCloseIsIdempotentAndReportsNoError(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, _ := pair(t, testConfig(t, rec))

	for i := range 3 {
		if err := c.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}

	select {
	case err := <-rec.closed:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("OnClosed got %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnClosed was never called")
	}
}

func TestSendAfterCloseFails(t *testing.T) {
	t.Parallel()

	c, _ := pair(t, testConfig(t, newRecorder()))
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := c.SendASDU(context.Background(), []byte{0x01})
	if !errors.Is(err, ErrClosed) {
		t.Errorf("got %v, want ErrClosed", err)
	}
}

func TestSendRespectsItsContext(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, newRecorder())
	c, p := startedPair(t, cfg)
	asdu := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}

	// Fill the window so the next send has to wait.
	for range int(cfg.Params.K) {
		if err := c.SendASDU(context.Background(), asdu); err != nil {
			t.Fatalf("priming: %v", err)
		}
	}
	for range int(cfg.Params.K) {
		p.expect(2 * time.Second)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := c.SendASDU(ctx, asdu); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want DeadlineExceeded", err)
	}
	if err := c.Err(); err != nil {
		t.Errorf("a cancelled send must not kill the connection: %v", err)
	}
}

// --- goroutine ownership ----------------------------------------------------

// Every goroutine a connection starts must be gone once it has closed. A leak
// here is per connection, so at a thousand stations with reconnects it is not a
// leak, it is an outage.
func TestNoGoroutinesSurviveClose(t *testing.T) {
	// Not parallel: it counts goroutines process-wide.

	// settle waits for goroutines that have already been told to stop to
	// actually leave the runtime's count.
	//
	// This is the one place a sleep is the right tool, and §2.6 is worth
	// re-reading before accepting that: the ban exists because sleeping to wait
	// for an EVENT is a race dressed up as a test. Here there is no event to
	// wait on. The signal we do have — OnClosed — is taken below, and it only
	// proves the reporting goroutine reached its last statement, not that the
	// scheduler has reaped it. goleak, which this stands in for, solves the same
	// problem the same way: retry with a delay. Two facts keep it honest — the
	// retry stops early when the count has come back down, so a working run
	// never pays the full wait; and a genuine leak does not disappear with more
	// waiting, so a longer bound cannot turn a real failure green.
	settle := func(target int) int {
		n := runtime.NumGoroutine()
		for range 50 {
			if n <= target {
				return n
			}
			runtime.Gosched()
			//nolint:forbidigo // see the comment above: a retry bound, not a
			// synchronisation step. There is no channel to wait on for "the
			// scheduler has finished reaping a goroutine".
			time.Sleep(2 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		return n
	}

	before := settle(0) // no target yet: this is the baseline being measured

	for range 20 {
		rec := newRecorder()
		c, p := startedPair(t, testConfig(t, rec))

		p.send(apci.NewI(0, 0, []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}))
		if err := c.SendASDU(context.Background(), []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}); err != nil {
			t.Fatalf("send: %v", err)
		}
		p.expect(2 * time.Second)

		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case err := <-rec.closed:
			if err == nil {
				t.Fatal("OnClosed fired with nil error")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OnClosed was never called")
		}
		_ = p.sock.Close()
	}

	after := settle(before)

	// A few goroutines belong to the runtime and the test framework and come
	// and go; twenty connections leaking three each would be sixty.
	if after > before+5 {
		t.Errorf("goroutines went from %d to %d after 20 connections", before, after)
	}
}

// Close must return only once every goroutine has stopped, so that a caller
// that closes and then tears down shared state cannot race one of them.
func TestCloseWaitsForTheGoroutines(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	c, _ := pair(t, testConfig(t, rec))

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done is not closed after Close returned")
	}
}

// --- t2 -----------------------------------------------------------------------

// A quiet link acknowledges on t2 and then stops. It is the "and then stops"
// that this test is for.
//
// Until 2026-09-28 the t2 path emitted an S-frame without telling the timers it
// had, so t2 stayed armed, Check reported the same action on the very next pass,
// and the connection poured S-frames onto the wire in a tight loop for as long
// as it lived — a busy goroutine and a flooded link, on every station too quiet
// to reach the w window. Nothing else caught it: the existing tests either used
// the system clock and finished before t2, or reached w first.
func TestTheT2AcknowledgementIsSentOnceNotInALoop(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	cfg := testConfig(t, newRecorder())
	cfg.Clock = fake

	rec := newRecorder()
	cfg.Handler = rec
	c, p := startedPair(t, cfg)

	// One frame: below the w window, so only t2 can acknowledge it.
	p.send(apci.NewI(0, 0, []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x64, 0x00, 0x00, 0x01}))
	rec.waitASDUs(t, 1)

	armed(fake)
	fake.Advance(cfg.Params.T2 + time.Second)

	if f := p.expect(2 * time.Second); f.Format != apci.FormatS {
		t.Fatalf("got %s, want an S-frame", f)
	}
	// And nothing more. A second one here means t2 was never cleared.
	p.expectNothing(200 * time.Millisecond)

	if err := c.Err(); err != nil {
		t.Errorf("the connection ended: %v", err)
	}
}
