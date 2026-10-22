// Package conn owns one TCP connection to a station and runs the APCI state
// machine over it.
//
// This is where the pure protocol packages meet the network. Three goroutines per
// connection, each with one job:
//
//	readLoop   socket -> frames -> a channel. Nothing else.
//	runLoop    owns the sequencer and the timers. Validates what arrived,
//	           delivers it, and decides what to send.
//	writeLoop  a channel -> socket. Nothing else. THE SINGLE WRITER.
//
// Why this split rather than the readLoop/writeLoop/timerLoop the design
// document names: the sequencer and the timers are touched by both what arrives
// and what is sent, so they need exactly one owner. Giving a separate timerLoop
// its own view of the timers would mean either a mutex on the per-frame path or
// a channel round trip for every frame. Putting the timer in the control loop's
// select removes both, and keeps the count at three.
//
// The single-writer rule survives intact, which is the point of it: N(S) and
// N(R) are assigned in one goroutine and never need a lock. A separate
// writeLoop earns its place by absorbing a slow socket — without it, a peer that
// stops reading would block the goroutine that also has to answer timers, and
// the connection would fail to acknowledge the very frames that are backing up.
package conn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/clock"
)

// Errors a connection ends with. They are sentinels because the station layer
// reacts differently to each: a protocol error means the peer and this side
// disagree and reconnecting is the only move, while a closed connection is
// routine.
var (
	// ErrClosed means Close was called. It is not a failure.
	ErrClosed = errors.New("connection closed")

	// ErrProtocol means the peer sent something the standard does not allow.
	// There is no resynchronisation in 104: the connection is finished.
	ErrProtocol = errors.New("protocol error")

	// ErrTimeout means t1 expired: something this side sent was never
	// acknowledged.
	ErrTimeout = errors.New("acknowledgement timeout")

	// ErrConfig means the connection was asked for with parameters that cannot
	// work.
	ErrConfig = errors.New("invalid connection configuration")

	// ErrNotStarted means an ASDU was offered before STARTDT was confirmed.
	// Until then the station sends nothing and accepts nothing, so a frame sent
	// now is not early — it is a protocol violation the station may drop the
	// connection over.
	ErrNotStarted = errors.New("data transfer not started")

	// ErrControlBusy means a second STARTDT/STOPDT dialogue was asked for while
	// one was still awaiting its confirmation. The standard allows one at a
	// time, and there is no way to tell two confirmations apart.
	ErrControlBusy = errors.New("a control dialogue is already outstanding")
)

// Handler receives what the connection observes.
//
// Every method is called from runLoop, and every one of them MUST return
// promptly. A handler that blocks stalls the loop that answers
// timers, so a slow handler does not merely delay data — it makes t1 expire and
// drops the connection. Anything slow belongs behind a buffered channel that
// drops rather than blocks.
type Handler interface {
	// OnASDU is called for each I-frame's application payload, after its
	// sequence numbers have been validated. The slice is owned by the callee:
	// apci.Decode already copied it out of the read buffer.
	OnASDU(raw []byte)

	// OnU is called for each U-format frame, after the connection has taken
	// whatever action the standard requires of it.
	OnU(fn apci.UFunction)

	// OnClosed is called exactly once, when the connection has finished and all
	// three goroutines have stopped. err is nil only for a clean Close.
	OnClosed(err error)
}

// Config describes one connection.
type Config struct {
	// Params are the APCI window sizes and timers for this station.
	Params apci.Params

	// Clock is the time source. Required: there is no default, because a
	// connection that silently used the wall clock could not be tested.
	Clock clock.Clock

	// Handler receives frames. Required.
	Handler Handler

	// QueueDepth bounds each of the three internal channels.
	//
	// These are deliberately small. They exist to decouple the loops for a
	// moment, not to store a backlog: a deep queue on the write side would let
	// this side keep accepting work long after the peer stopped reading, and the
	// only thing that would achieve is a larger burst of frames the peer has
	// already given up on.
	QueueDepth int

	// WriteTimeout bounds a single socket write.
	//
	// Without it a peer that stops reading blocks writeLoop forever, the write
	// queue fills, and the connection hangs instead of failing — which is worse,
	// because a hung connection still looks connected.
	WriteTimeout time.Duration
}

// Defaults for the fields that have sensible ones.
const (
	DefaultQueueDepth   = 16
	DefaultWriteTimeout = 10 * time.Second
)

func (cfg *Config) applyDefaults() error {
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.Clock == nil {
		return fmt.Errorf("%w: no clock", ErrConfig)
	}
	if cfg.Handler == nil {
		return fmt.Errorf("%w: no handler", ErrConfig)
	}
	if err := cfg.Params.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	return nil
}

// sendRequest is one thing a caller wants put on the wire.
type sendRequest struct {
	// asdu is set for an I-frame; fn is set for a U-frame. Exactly one of them.
	asdu []byte
	fn   apci.UFunction

	// done receives the outcome once the frame has been handed to writeLoop,
	// or the reason it never will be.
	done chan error

	// confirm is non-nil for a control activation whose caller is waiting for
	// the station's confirmation, not merely for the frame to go out. It is
	// buffered, so runLoop never blocks resolving it.
	confirm chan error
}

// Conn is one connection. It is safe for concurrent use by callers; internally
// each piece of state has exactly one owning goroutine.
type Conn struct {
	cfg  Config
	sock net.Conn

	// Owned by runLoop. Nothing else may touch them.
	seq      apci.Sequencer
	timers   *apci.Timers
	transfer dataTransfer
	pending  *sendRequest // the control activation awaiting its confirmation

	recvCh  chan apci.Frame  // readLoop -> runLoop
	sendCh  chan sendRequest // callers  -> runLoop
	writeCh chan []byte      // runLoop  -> writeLoop

	// done is closed once, by whichever goroutine fails first. It is the stop
	// signal for all three.
	done      chan struct{}
	closeOnce sync.Once

	errMu sync.Mutex
	err   error

	wg sync.WaitGroup
}

// New starts a connection over an already-established socket.
//
// The socket becomes the connection's: it is closed when the connection ends,
// and the caller must not read or write it afterwards.
func New(sock net.Conn, cfg Config) (*Conn, error) {
	if sock == nil {
		return nil, fmt.Errorf("%w: no socket", ErrConfig)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	c := &Conn{
		cfg:     cfg,
		sock:    sock,
		timers:  apci.NewTimers(cfg.Params),
		recvCh:  make(chan apci.Frame, cfg.QueueDepth),
		sendCh:  make(chan sendRequest),
		writeCh: make(chan []byte, cfg.QueueDepth),
		done:    make(chan struct{}),
	}

	c.timers.Start(cfg.Clock.Now())

	c.wg.Add(3)
	go c.readLoop()
	go c.runLoop()
	go c.writeLoop()

	// A fourth goroutine, owned by New rather than by the connection: it exists
	// only to report the ending once, after the three have stopped. Doing this
	// inside any of the three would mean calling the handler from a goroutine
	// that is still supposed to be draining.
	go func() {
		c.wg.Wait()
		_ = c.sock.Close()
		cfg.Handler.OnClosed(c.Err())
	}()

	return c, nil
}

// SendASDU queues an I-frame carrying asdu.
//
// It blocks until runLoop has accepted the frame, which it will not do while
// the k window is full — that back pressure is the point. It returns when the
// frame has been handed to writeLoop, not when the peer has acknowledged it.
//
// It returns ErrNotStarted if data transfer has not been started; see Start.
func (c *Conn) SendASDU(ctx context.Context, asdu []byte) error {
	if len(asdu) == 0 {
		return fmt.Errorf("%w: empty ASDU", apci.ErrEmptyASDU)
	}
	return c.submit(ctx, sendRequest{asdu: asdu})
}

// SendU queues a U-format frame and returns once it has been handed to
// writeLoop. It does NOT wait for a confirmation — Start and Stop do that.
//
// Reach for those instead of this for STARTDT and STOPDT: sending the
// activation without tracking its answer leaves the connection's idea of the
// transfer state and the station's disagreeing, which is the one thing the
// state machine exists to prevent.
func (c *Conn) SendU(ctx context.Context, fn apci.UFunction) error {
	if !fn.Valid() {
		return fmt.Errorf("%w: %s", ErrProtocol, fn)
	}
	return c.submit(ctx, sendRequest{fn: fn})
}

func (c *Conn) submit(ctx context.Context, r sendRequest) error {
	r.done = make(chan error, 1)

	select {
	case c.sendCh <- r:
	case <-c.done:
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-r.done:
		return err
	case <-c.done:
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the connection. It is idempotent and safe to call from anywhere.
//
// It does not send STOPDT: a graceful shutdown is the station layer's business
// (T6.2), and Close is what happens when there is no time for one.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	c.wg.Wait()
	if err := c.Err(); err != nil && !errors.Is(err, ErrClosed) {
		return err
	}
	return nil
}

// Done is closed when the connection has stopped accepting work. It may be
// selected on.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, or nil while it is running.
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// fail records the first reason the connection ended and stops everything.
//
// Only the first call wins: the reason a connection died is the first thing
// that went wrong, and the errors that follow are consequences of it. Reporting
// the last one would name the symptom instead of the cause.
func (c *Conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()

		close(c.done)

		// Unblock a goroutine parked in Read or Write. Closing the socket is
		// the only way to interrupt those; a deadline would work too but would
		// need to be set from the goroutine that owns the side being unblocked,
		// and there is no such goroutine once things are going wrong.
		_ = c.sock.Close()
	})
}

// --- readLoop ---------------------------------------------------------------

// readLoop turns the socket into frames and does nothing else.
//
// It is the one goroutine that can be parked in a blocking syscall, so it holds
// no state anyone else needs and makes no decisions. Everything it can say is
// said by putting a frame on recvCh or by failing the connection.
func (c *Conn) readLoop() {
	defer c.wg.Done()

	// Four frames' worth: enough that a busy link fills the buffer with whole
	// frames most of the time, small enough to be uninteresting per connection
	// at a thousand stations.
	buf := make([]byte, 0, 4*apci.MaxFrameLen)
	chunk := make([]byte, 4*apci.MaxFrameLen)

	for {
		n, readErr := c.sock.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)

			consumed := 0
			for {
				rest := buf[consumed:]
				need, err := apci.NextFrameLen(rest)
				if errors.Is(err, apci.ErrIncomplete) {
					break
				}
				if err != nil {
					// There is no resynchronisation: 0x68 occurs inside ASDU
					// payloads, so hunting for the next one locks onto a false
					// boundary and delivers plausible nonsense.
					c.fail(fmt.Errorf("%w: %w", ErrProtocol, err))
					return
				}
				if len(rest) < need {
					break
				}

				f, err := apci.Decode(rest[:need])
				if err != nil {
					c.fail(fmt.Errorf("%w: %w", ErrProtocol, err))
					return
				}
				consumed += need

				select {
				case c.recvCh <- f:
				case <-c.done:
					return
				}
			}

			// Copy the tail down rather than reslicing forward, so the backing
			// array is reused instead of growing without bound on a long-lived
			// connection.
			buf = append(buf[:0], buf[consumed:]...)
		}

		if readErr != nil {
			select {
			case <-c.done:
				// Already ending; the read failed because the socket was closed
				// to unblock us, which is not news.
			default:
				c.fail(fmt.Errorf("read: %w", readErr))
			}
			return
		}
	}
}

// --- runLoop ----------------------------------------------------------------

// runLoop owns the sequencer and the timers, and is the only goroutine that
// touches them.
//
// Its shape is the design: everything that can change protocol state arrives on
// one of four cases, so the state needs no lock and the ordering between "a
// frame arrived", "a caller wants to send" and "a timer expired" is explicit
// rather than emergent.
func (c *Conn) runLoop() {
	defer c.wg.Done()

	timer := c.cfg.Clock.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		// Timers first, and to exhaustion: an expired t1 must close the
		// connection before anything else is attempted on it, and Check reports
		// one action at a time.
		for {
			action, wait := c.timers.Check(c.cfg.Clock.Now())
			if action == apci.ActionNone {
				rearm(timer, wait)
				break
			}
			if !c.handleTimer(action) {
				return
			}
		}

		// The k window as back pressure: while it is full, the send case is a
		// nil channel and this loop simply does not accept more work. That is
		// the whole implementation of "stop sending at k" — no counter to get
		// wrong, no queue to overflow.
		var sendCh chan sendRequest
		if c.cfg.Params.CanSend(c.seq.Outstanding()) {
			sendCh = c.sendCh
		}

		select {
		case <-c.done:
			return

		case f := <-c.recvCh:
			if !c.handleFrame(f) {
				return
			}

		case r := <-sendCh:
			// Two different errors: one the caller is told about and the
			// connection survives (asking too early, asking twice), one that
			// ends the connection. Collapsing them would mean a caller's
			// mistake took the link down with it.
			callerErr, fatal := c.handleSend(r)
			r.done <- callerErr
			if fatal {
				return
			}

		case <-timer.C():
			// Loop round; the timer block at the top decides what is due.
		}
	}
}

// handleTimer acts on one expired timer. It reports whether the loop continues.
func (c *Conn) handleTimer(action apci.Action) bool {
	switch action {
	case apci.ActionClose:
		c.fail(fmt.Errorf("%w: t1 expired with %d frame(s) unacknowledged",
			ErrTimeout, c.seq.Outstanding()))
		return false

	case apci.ActionSendSupervisory:
		return c.sendSupervisory()

	case apci.ActionSendTestFR:
		if !c.emit(apci.NewU(apci.TestFRAct)) {
			return false
		}
		c.timers.SentU(c.cfg.Clock.Now(), apci.TestFRAct)
		return true

	default:
		return true
	}
}

// handleFrame applies one received frame. It reports whether the loop continues.
func (c *Conn) handleFrame(f apci.Frame) bool {
	now := c.cfg.Clock.Now()

	switch f.Format {
	case apci.FormatI:
		// Study Reference §4: the controlled station sends no I-frame until it
		// has answered STARTDT con. One arriving before that means it is not
		// running the handshake we think it is, and everything downstream —
		// sequence numbers, the cache, topicality — is built on that
		// assumption.
		if !c.transfer.acceptsIncomingData() {
			c.fail(fmt.Errorf("%w: I-frame while transfer is %s", ErrProtocol, c.transfer))
			return false
		}
		if err := c.seq.Receive(f.SendSeq, f.RecvSeq); err != nil {
			c.fail(fmt.Errorf("%w: %w", ErrProtocol, err))
			return false
		}
		c.timers.ReceivedI(now, c.seq.Outstanding())

		// Deliver before checking the w window, so that a handler sees frames
		// in arrival order regardless of when acknowledgements go out.
		c.cfg.Handler.OnASDU(f.ASDU)

		if c.cfg.Params.AckDue(c.seq.UnackedReceived()) {
			return c.sendSupervisory()
		}
		return true

	case apci.FormatS:
		if _, err := c.seq.Acknowledge(f.RecvSeq); err != nil {
			c.fail(fmt.Errorf("%w: %w", ErrProtocol, err))
			return false
		}
		c.timers.ReceivedS(now, c.seq.Outstanding())
		return true

	default: // U
		c.timers.ReceivedU(now, f.Function)

		// State first, then the answer: a frame the peer is not allowed to send
		// ends the connection, and answering it before deciding that would put
		// a reply on a link already known to be wrong.
		if !c.controlReceived(f.Function) {
			return false
		}

		// A TESTFR activation is answered here rather than by the handler: the
		// answer is required by the standard, carries no information, and must
		// go out even if whatever owns the handler is busy or broken.
		if f.Function == apci.TestFRAct {
			if !c.emit(apci.NewU(apci.TestFRCon)) {
				return false
			}
		}
		c.cfg.Handler.OnU(f.Function)
		return true
	}
}

// handleSend puts one caller's frame on the wire.
//
// It returns the error the caller is given, and whether the connection is
// finished. Those are not the same thing: a caller that sends too early gets an
// error and the link carries on.
func (c *Conn) handleSend(r sendRequest) (callerErr error, fatal bool) {
	now := c.cfg.Clock.Now()

	if r.asdu != nil {
		if c.transfer != transferStarted {
			return fmt.Errorf("%w: transfer is %s", ErrNotStarted, c.transfer), false
		}
		sendSeq, recvSeq := c.seq.NextSend()
		if !c.emit(apci.NewI(sendSeq, recvSeq, r.asdu)) {
			return c.Err(), true
		}
		c.timers.SentI(now)
		return nil, false
	}

	if r.confirm != nil {
		send, err := c.beginControl(r)
		if err != nil {
			return err, false
		}
		if !send {
			return nil, false
		}
	}

	if !c.emit(apci.NewU(r.fn)) {
		return c.Err(), true
	}
	c.timers.SentU(now, r.fn)
	c.controlSent(r)
	return nil, false
}

// sendSupervisory acknowledges what has been received.
//
// Every S-frame goes through here, and that is the point rather than tidiness.
// An S-frame is the only thing that clears t2, so emitting one without telling
// the timers leaves t2 armed, Check reports the same action again at once, and
// the connection pours S-frames onto the wire in a tight loop for as long as it
// lives. It did exactly that until 2026-09-28: two call sites, one of which
// recorded the send and one of which did not. One function cannot disagree with
// itself.
func (c *Conn) sendSupervisory() bool {
	if !c.emit(apci.NewS(c.seq.SendSupervisory())) {
		return false
	}
	c.timers.SentS(c.cfg.Clock.Now())
	return true
}

// emit encodes a frame and hands it to writeLoop. It reports whether the loop
// continues.
func (c *Conn) emit(f apci.Frame) bool {
	raw, err := f.Encode()
	if err != nil {
		// Encoding a frame this package built cannot fail unless the state
		// machine is wrong, so this is a bug report, not a peer problem.
		c.fail(fmt.Errorf("encoding %s: %w", f, err))
		return false
	}

	select {
	case c.writeCh <- raw:
		return true
	case <-c.done:
		return false
	}
}

// --- writeLoop --------------------------------------------------------------

// writeLoop is the single writer. It owns no protocol state and makes no
// decisions: everything it puts on the socket was decided by runLoop.
//
// With one writer, N(S) and N(R) are
// assigned in one goroutine and never need a lock, and the order frames reach
// the wire is the order they were numbered.
func (c *Conn) writeLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.done:
			return

		case raw := <-c.writeCh:
			if err := c.sock.SetWriteDeadline(c.deadline()); err != nil {
				c.fail(fmt.Errorf("setting the write deadline: %w", err))
				return
			}
			if _, err := c.sock.Write(raw); err != nil {
				select {
				case <-c.done:
					// Already ending; the write failed because the socket was
					// closed to unblock us.
				default:
					c.fail(fmt.Errorf("write: %w", err))
				}
				return
			}
		}
	}
}

// deadline is the wall-clock instant a write must finish by.
//
// It deliberately uses the real clock rather than cfg.Clock: SetWriteDeadline
// is enforced by the operating system, which has never heard of a fake clock.
// Handing it a simulated instant would set a deadline decades in the past or
// future.
func (c *Conn) deadline() time.Time {
	//nolint:forbidigo // time.Now is banned so that timer LOGIC is testable.
	// This is not timer logic: it is an operating-system deadline, and the
	// kernel has never heard of cfg.Clock. Handing it a simulated instant would
	// set a deadline decades in the past and fail every write immediately.
	return time.Now().Add(c.cfg.WriteTimeout)
}

// rearm points a timer at the next deadline.
//
// A non-positive duration means nothing is armed, which only happens before the
// connection starts; an hour is then a placeholder, because any event wakes the
// loop anyway.
func rearm(t clock.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C():
		default:
		}
	}
	if d <= 0 {
		d = time.Hour
	}
	t.Reset(d)
}
