package station

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
	"github.com/radaren989/iec104/command"
	"github.com/radaren989/iec104/conn"
)

// sender puts one ASDU on the wire. It exists so that the interrogation can be
// tested without a socket, and so that gi.go does not need to know that a
// connection is what carries it.
type sender func(ctx context.Context, asdu []byte) error

// Errors a caller can react to.
var (
	// ErrConfig means the station was asked for with settings that cannot work.
	ErrConfig = errors.New("invalid station configuration")

	// ErrClockSyncRefused means the station declined to have its clock set.
	// Some devices will not take a clock from a master at all, and that is a
	// legitimate answer — but it must stop us reporting the setting as in
	// effect.
	ErrClockSyncRefused = errors.New("clock synchronisation refused")

	// ErrClockSyncTimeout means the confirmation never arrived.
	ErrClockSyncTimeout = errors.New("clock synchronisation not confirmed")
)

// Dialer opens the TCP connection. *net.Dialer satisfies it.
//
// It is an interface so that the lifecycle can be tested without a network: a
// station that reconnects a hundred times is a test about backoff and
// goroutines, not about TCP, and running it against real sockets would make it
// slow and flaky for no gain.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Gate rations something scarce across the whole fleet.
//
// It exists because of decision G1: one instance holds a thousand stations, and
// a restart that dials them all at once — or interrogates them all at once — is
// a self-inflicted denial of service against the substation network.
// internal/ratelimit implements it.
//
// It is defined here and injected rather than imported, so that station
// knows nothing about ratelimit.
type Gate interface {
	// Acquire blocks until this station may proceed, or ctx is done.
	//
	// The returned function MUST be called when the work is over, whether it
	// succeeded or not — a gate that counts what is in flight cannot tell the
	// difference between slow and abandoned. It is safe to call more than once.
	// On error it is non-nil and does nothing, so a caller can defer it before
	// checking.
	Acquire(ctx context.Context) (release func(), err error)
}

// NoGate permits everything immediately.
//
// It is the right behaviour for one station and the wrong behaviour for a
// thousand, which is exactly why it is the default: a single station under test
// should not need a rate limiter, and a fleet should never be assembled without
// one.
type NoGate struct{}

func (NoGate) Acquire(context.Context) (func(), error) { return func() {}, nil }

// Observer is told about state changes and about data.
//
// Every method is called from the station's own goroutine and MUST return
// promptly: this is the same goroutine that will run the connection's handler,
// so a slow observer becomes a dropped field connection.
type Observer interface {
	// OnState reports a transition. reason is non-nil only for a move into
	// StateFailed, and it is the reason the operator needs: "it reconnected" is
	// noise, "it reconnected because t1 expired" is a diagnosis.
	OnState(from, to State, reason error)

	// OnASDU is one application payload from the station, already validated at
	// the APCI level. Interpreting it is the caller's business — this package
	// does not know what a type ID means.
	OnASDU(raw []byte)

	// OnEvent is one reading that is an event: the device reports that a value
	// changed. See event.go for which readings qualify and why the decision is
	// made here rather than behind the event plane's buffer.
	//
	// Everything an interrogation brings in reaches the cache and never reaches
	// this method. It runs on the connection's runLoop like OnASDU, so it must
	// hand the event off without blocking.
	OnEvent(e Event)

	// OnGI reports one general interrogation: how long it took, and why it
	// failed if it did. It is the source of iec104_gi_duration_seconds, which
	// is what sizes GI_CONCURRENCY — the cold-start budget is a function of
	// this number and nothing else.
	OnGI(d time.Duration, err error)

	// OnClockSkew reports the device's clock minus ours, and whether it has
	// passed the threshold this station was configured with. It is the source
	// of iec104_clock_skew_seconds and of the warning that goes with it.
	OnClockSkew(skew time.Duration, beyondThreshold bool)

	// OnClockSync reports a failed attempt to set the device's clock. It is
	// never called when synchronisation is disabled, and never called on
	// success — a clock that was set is not news.
	OnClockSync(err error)

	// OnCommand reports one finished command. It is the source of
	// iec104_command_total{outcome} and iec104_command_duration_seconds, and of
	// the audit trail every command has to leave.
	//
	// It is called once per command, on the command queue's consumer goroutine,
	// after the result is settled and before it reaches the caller. Refused
	// commands are reported too: a station whose queue is full or whose write
	// lock is off is exactly the one an operator needs to see.
	OnCommand(cmd asdu.Command, res command.Result)

	// OnAnomaly reports a device breaking the standard in a way the connection
	// survives. See the AnomalyKind documentation for why these are surfaced
	// rather than tolerated in silence.
	//
	// kind is low-cardinality and is meant to be a metric label; detail is for
	// a log line and is not. This is called once per offending ASDU, which on a
	// device stuck in a loop is several times a second per station — so an
	// implementation counts, and does NOT log unconditionally.
	OnAnomaly(kind AnomalyKind, detail string)
}

// Config describes one station.
type Config struct {
	// Address is host:port. Validated by the gRPC layer (S3/S4) before it gets
	// here; this package dials what it is given.
	Address string

	// Params are the APCI window sizes and timers. T0 is the dial timeout and
	// is applied here — that is the whole reason apci.Timers does not have it.
	Params apci.Params

	// ASDUParams are the octet widths this station uses. They are per station
	// because the standard allows a choice and devices disagree; getting them
	// wrong shifts every field in every frame.
	ASDUParams asdu.Params

	// CommonAddress is the station address carried in every ASDU. Under
	// decision G4 there is exactly one per connection.
	CommonAddress uint16

	// Originator is the originator address, carried only when COTOctets is 2.
	Originator uint8

	// GI is the interrogation policy. Zero means DefaultGI.
	GI GI

	// ClockSync is what to do about the device's clock: which zone it is set
	// to, when the skew is worth complaining about, and whether to set it.
	// Setting it is off by default (V10).
	ClockSync ClockSync

	// Clock is the time source. Required.
	Clock clock.Clock

	// Observer receives state changes and data. Required.
	Observer Observer

	// Dialer opens sockets. Defaults to a *net.Dialer.
	Dialer Dialer

	// DialGate rations dialling across the fleet: how fast, and how many at
	// once. Defaults to NoGate.
	DialGate Gate

	// GIGate rations general interrogations across the fleet. A GI is the most
	// expensive thing a station does — it sends every point it has — so this is
	// the limit that decides how long a cold start takes (design §5.5).
	// Defaults to NoGate.
	GIGate Gate

	// Backoff is the retry schedule. Zero means DefaultBackoff.
	Backoff Backoff

	// Commands is the command policy. Disabled by default: a station that only
	// needs reading has no business being able to drive an output.
	Commands Commands

	// Chatter is when a bouncing contact's events are flagged. Zero means
	// DefaultChatter.
	Chatter Chatter
}

func (cfg *Config) applyDefaults() error {
	if cfg.Address == "" {
		return fmt.Errorf("%w: no address", ErrConfig)
	}
	if cfg.Clock == nil {
		return fmt.Errorf("%w: no clock", ErrConfig)
	}
	if cfg.Observer == nil {
		return fmt.Errorf("%w: no observer", ErrConfig)
	}
	if err := cfg.Params.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if err := cfg.ASDUParams.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}

	if cfg.GI.Timeout == 0 {
		cfg.GI = DefaultGI()
	}
	if err := cfg.GI.Validate(); err != nil {
		return err
	}
	if err := cfg.ClockSync.Validate(); err != nil {
		return err
	}
	if cfg.Chatter == (Chatter{}) {
		cfg.Chatter = DefaultChatter()
	}
	if err := cfg.Chatter.Validate(); err != nil {
		return err
	}

	if cfg.Dialer == nil {
		cfg.Dialer = &net.Dialer{Timeout: cfg.Params.T0}
	}
	if cfg.DialGate == nil {
		cfg.DialGate = NoGate{}
	}
	if cfg.GIGate == nil {
		cfg.GIGate = NoGate{}
	}
	// Base is the field that decides: a caller who set anything meaningful set
	// this, and Validate rejects a partial schedule rather than quietly filling
	// half of it in. Comparing the whole struct is not possible — it holds a
	// func — and would be the wrong question anyway.
	if cfg.Backoff.Base == 0 {
		cfg.Backoff = DefaultBackoff()
	}
	if cfg.Backoff.Rand == nil {
 	// Injected here rather than reached for inside Backoff, so that the
		// package has no global mutable state and a test can
		// replace it without replacing the schedule.
		cfg.Backoff.Rand = rand.Float64
	}
	if err := cfg.Backoff.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	return nil
}

// Station keeps one station connected.
//
// One goroutine owns the lifecycle: dial, handshake, hold, fail, wait, repeat.
// State lives in that goroutine and is published through a mutex for readers,
// which are gRPC handlers asking "is this topical" and nothing more.
type Station struct {
	cfg Config

	mu    sync.Mutex
	state State
	conn  *conn.Conn

	// broken carries the reason a connection ended, from the connection's own
	// reporting goroutine to the lifecycle. Buffered so that reporting never
	// blocks; a second failure on a connection that already failed is the same
	// news twice.
	broken chan error

	// giFrames carries C_IC_NA_1 headers from the connection's goroutine to
	// whichever interrogation is waiting for its bracket to close.
	giFrames chan asdu.Header

	// reinit is pulsed when the station announces it has restarted (M_EI_NA_1).
	reinit chan struct{}

	// clockFrames carries C_CS_NA_1 headers to whichever synchronisation is
	// waiting for its confirmation.
	clockFrames chan asdu.Header

	// giOpen says an interrogation is outstanding: the activation has gone out
	// and the bracket has not closed.
	//
	// It is atomic because it is written by the station's own goroutine, in
	// interrogate, and read by the connection's goroutine, in inspect. Anything
	// arriving with an interrogation cause while this is false is traffic for a
	// request nobody made, which is worth recording (AnomalyUnsolicitedInterrogation).
	giOpen atomic.Bool

	// commands serialises this station's commands, and operator carries out
	// each one. Both are nil when commands are disabled, which is the cheapest
	// possible guarantee that a read-only station cannot drive anything.
	//
	// They live as long as the station rather than as long as a connection: a
	// queue rebuilt on every reconnect would lose its depth limit exactly when
	// a flapping link makes that limit matter.
	commands   *command.Queue
	operator   *command.Operator
	cmdReplies chan command.Reply

	// cache is the last value seen for each point. Written by the connection's
	// handler goroutine, read by gRPC handlers through a copy.
	cache *cache

	// chatter is touched only by that same handler goroutine; see
	// chatterFilter for why it needs no lock.
	chatter *chatterFilter

	// topical is closed while the station is topical and replaced with a fresh
	// open channel when it stops being. A waiter takes it under the same lock
	// as the state, so it cannot observe "not topical" and then wait on a
	// channel belonging to a topicality that has already come and gone.
	topical chan struct{}

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New starts a station and begins connecting.
func New(cfg Config) (*Station, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Station{
		cfg:    cfg,
		state:  StateDisconnected,
		broken: make(chan error, 1),
		// Four deep: a station may repeat a confirmation, and a bracket that
		// missed one because the buffer was full would wait out its whole
		// timeout for news that had already arrived.
		giFrames:    make(chan asdu.Header, 4),
		reinit:      make(chan struct{}, 1),
		clockFrames: make(chan asdu.Header, 2),
		cache:       newCache(),
		chatter:     newChatterFilter(cfg.Chatter, cfg.Clock.Now()),
		topical:     make(chan struct{}),
		cancel:      cancel,
	}

	if cfg.Commands.Enabled {
		if err := s.startCommands(); err != nil {
			cancel()
			return nil, err
		}
	}

	s.wg.Add(1)
	go s.run(ctx)
	return s, nil
}

// State reports where the station stands.
func (s *Station) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Release gives the station up for good and waits for it to stop.
//
// It is terminal by design: a released station does not reconnect. That is what
// makes it different from a connection dropping, and the lease sweeper in the
// registry depends on the difference.
func (s *Station) Release() error {
	s.cancel()
	s.wg.Wait()

	// After the run goroutine, not before: closing the queue first would let a
	// command still in flight finish against a connection that is being torn
	// down underneath it.
	if s.commands != nil {
		return s.commands.Close()
	}
	return nil
}

// --- the lifecycle ----------------------------------------------------------

func (s *Station) run(ctx context.Context) {
	defer s.wg.Done()
	defer s.setState(StateReleased, nil)

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}

		reason := s.attempt(ctx)
		if ctx.Err() != nil {
			return
		}
		s.setState(StateFailed, reason)

		// Back to Disconnected through the wait, never straight to another
		// dial: the backoff sits on the single path out of Failed so that no
		// future shortcut can skip it.
		wait := s.cfg.Backoff.Delay(attempt)
		s.setState(StateDisconnected, nil)

		timer := s.cfg.Clock.NewTimer(wait)
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// attempt runs one connection from dial to failure. It returns the reason the
// connection ended, or nil if the context ended it.
func (s *Station) attempt(ctx context.Context) error {
	s.setState(StateQueued, nil)
	releaseDial, err := s.cfg.DialGate.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("waiting for a dial slot: %w", err)
	}

	s.setState(StateConnecting, nil)
	sock, err := s.cfg.Dialer.DialContext(ctx, "tcp", s.cfg.Address)

	// The slot covers the DIAL, not the connection. t0 is what bounds it, which
	// is the whole reason dialling needs a concurrency limit of its own: at
	// thirty seconds a slot, a rate limit alone would let unanswered dials pile
	// up without bound.
	releaseDial()

	if err != nil {
		return fmt.Errorf("dialling %s: %w", s.cfg.Address, err)
	}

	// Drain any stale report before the new connection can produce one.
	// Otherwise a failure from the previous attempt would be read as this
	// attempt's, and the operator would be told the wrong reason.
	//nolint:errcheck // drain only; the value is intentionally discarded
	select {
	case <-s.broken:
	default:
	}

	c, err := conn.New(sock, conn.Config{
		Params:  s.cfg.Params,
		Clock:   s.cfg.Clock,
		Handler: stationHandler{s},
	})
	if err != nil {
		_ = sock.Close()
		return fmt.Errorf("starting the connection: %w", err)
	}

	s.mu.Lock()
	s.conn = c
	s.mu.Unlock()

	defer func() {
		_ = c.Close()
		s.mu.Lock()
		s.conn = nil
		s.mu.Unlock()
	}()

	// The handshake is not optional and not a formality: until STARTDT is
	// confirmed the station sends nothing at all (Study Reference §4), so a
	// connection that skips it looks healthy and delivers nothing.
	if err := c.Start(ctx); err != nil {
		return fmt.Errorf("starting data transfer: %w", err)
	}
	s.setState(StateStarted, nil)

	return s.serve(ctx, c.SendASDU)
}

// serve holds an established connection: interrogate, then stay until something
// ends it.
//
// It is split out from attempt so that the interrogation and the re-interrogation
// rules can be tested against a plain sender, without a socket.
func (s *Station) serve(ctx context.Context, send sender) error {
	if s.cfg.GI.OnConnect {
		if err := s.interrogate(ctx, send); err != nil {
			return err
		}
	}

	// A period of zero disables re-interrogation. The timer is still created,
	// armed far out, because a nil channel in the select below would be one
	// more branch to get wrong for a case that costs nothing.
	period := s.cfg.GI.Period
	if period <= 0 {
		period = neverPeriod
	}
	periodic := s.cfg.Clock.NewTimer(period)
	defer periodic.Stop()

	// The clock is set once on connect and then on its own timer. Once on
	// connect because a device that has just been power-cycled is exactly the
	// one whose clock is wrong, and it is also exactly the one that has just
	// reconnected.
	syncPeriod := s.cfg.ClockSync.Period
	if !s.cfg.ClockSync.Enabled || syncPeriod <= 0 {
		syncPeriod = neverPeriod
	}
	if s.cfg.ClockSync.Enabled {
		if err := s.syncClock(ctx, send); err != nil {
			// Not fatal. A station that will not take a clock still delivers
			// data, and dropping the connection over it would turn a policy
			// disagreement into an outage. The observer hears about it.
			s.cfg.Observer.OnClockSync(err)
		}
	}
	clockTimer := s.cfg.Clock.NewTimer(syncPeriod)
	defer clockTimer.Stop()

	for {
		select {
		case reason := <-s.broken:
			return reason

		case <-ctx.Done():
			return nil

		case <-clockTimer.C():
			if s.cfg.ClockSync.Enabled {
				if err := s.syncClock(ctx, send); err != nil {
					s.cfg.Observer.OnClockSync(err)
				}
			}
			clockTimer.Reset(syncPeriod)

		case <-s.reinit:
			// M_EI_NA_1: the station has restarted, so everything cached from
			// before belongs to a different life of the device. Anything kept
			// from it would be a value that looks current and describes a
			// machine that has since been power-cycled.
			if err := s.interrogate(ctx, send); err != nil {
				return err
			}
			periodic.Reset(period)

		case <-periodic.C():
			if s.cfg.GI.Period <= 0 {
				// The far-out placeholder fired, which means this process has
				// been up for years. Re-arm and carry on.
				periodic.Reset(period)
				continue
			}
			if err := s.interrogate(ctx, send); err != nil {
				return err
			}
			periodic.Reset(period)
		}
	}
}

// neverPeriod is the placeholder interval for a station with re-interrogation
// disabled: long enough never to arrive in the life of a process, short enough
// not to overflow a duration.
const neverPeriod = 100 * 365 * 24 * time.Hour

// setState moves the station and tells the observer, refusing a move the state
// machine does not allow.
//
// A refused transition is a bug in this file, not bad input, so it is reported
// to the observer as a failure rather than silently applied. Applying it would
// mean the published state no longer matches the diagram anyone is reading.
func (s *Station) setState(to State, reason error) {
	s.mu.Lock()
	from := s.state
	if from == to || from.Terminal() {
		s.mu.Unlock()
		return
	}
	legal := CanTransition(from, to)
	if legal {
		s.state = to

		// The topicality signal moves with the state and under the same lock,
		// so a waiter can never read "not topical" and then block on a channel
		// belonging to a window that has already closed.
		switch {
		case to.Topical():
			close(s.topical)
		case from.Topical():
			s.topical = make(chan struct{})
		}
	}
	s.mu.Unlock()

	if !legal {
		s.cfg.Observer.OnState(from, from,
			fmt.Errorf("refused illegal transition %s -> %s; the state machine and the code disagree", from, to))
		return
	}
	s.cfg.Observer.OnState(from, to, reason)
}

// stationHandler adapts the station to conn.Handler.
//
// It is a separate type rather than methods on Station so that OnASDU and
// OnClosed cannot be called on a station by anything but its own connection.
type stationHandler struct{ s *Station }

func (h stationHandler) OnASDU(raw []byte) {
	h.s.inspect(raw)
	h.s.cfg.Observer.OnASDU(raw)
}

func (stationHandler) OnU(apci.UFunction) {
	// Nothing to do: the connection already answered whatever needed
	// answering, and the station layer has no use for keep-alives.
}

func (h stationHandler) OnClosed(err error) {
	if err == nil {
		err = conn.ErrClosed
	}
	select {
	case h.s.broken <- err:
	default:
		// Already reported. The first reason is the one that matters; the
		// lifecycle is on its way to the backoff either way.
	}
}
