package station

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// This file is the cold-start gate.
//
// IEC 104 has no reliable "read this point now". What it has is the general
// interrogation: the master asks once, and the station replies with every point
// it has, bracketed between a confirmation and a termination (Study Reference
// §10):
//
//	master -> station   C_IC_NA_1  COT=6  (act), QOI=20
//	station -> master   C_IC_NA_1  COT=7  (actcon)
//	station -> master   ...        COT=20 (inrogen)   every point
//	station -> master   C_IC_NA_1  COT=10 (actterm)
//
// Everything downstream rests on that bracket. Until the actterm arrives the
// cache holds whatever subset has turned up so far, and a reading taken from it
// is not wrong in a way anyone can see — it is a plausible number that happens
// to be missing its neighbours, or stale, or absent. That is why this bracket,
// and not the TCP connection, is what makes a station topical.

// GI errors. They are separate sentinels because the station layer reacts to
// each one the same way — drop the connection — but the operator does not: a
// refused interrogation is a configuration problem and a timed-out one is a
// device problem.
var (
	// ErrGIRefused means the station answered the interrogation with P/N set.
	// It knows what was asked and is declining, which almost always means the
	// common address is wrong.
	ErrGIRefused = errors.New("interrogation refused by the station")

	// ErrGITimeout means the bracket never closed. Some points may have
	// arrived; the cache is a subset of unknown size, which is worse than an
	// empty one because it looks complete.
	ErrGITimeout = errors.New("interrogation did not terminate")
)

// Defaults for the interrogation, from the contract's validation rules.
const (
	// DefaultGIOnConnect: interrogate as soon as data transfer starts. Off
	// would mean serving an empty cache until something happens to change
	// spontaneously, which on a quiet substation can be hours.
	DefaultGIOnConnect = true

	// DefaultGITimeout bounds the whole bracket. It is generous because a
	// station with a few hundred points answers over many frames, and the k
	// window paces them.
	DefaultGITimeout = 60 * time.Second
)

// GI is the interrogation policy for one station.
type GI struct {
	// OnConnect runs an interrogation as soon as data transfer starts.
	OnConnect bool

	// Period re-interrogates on a timer. Zero disables it.
	//
	// It is off by default and should stay off unless a station is known to
	// drop spontaneous updates: a periodic GI on a thousand stations is a
	// thousand full snapshots, and the point of 104 is that you do not need
	// them.
	Period time.Duration

	// Timeout bounds one interrogation, activation to termination.
	Timeout time.Duration
}

// DefaultGI returns the policy a station gets when it specifies none.
func DefaultGI() GI {
	return GI{OnConnect: DefaultGIOnConnect, Timeout: DefaultGITimeout}
}

// Validate reports why the policy cannot work, if it cannot.
func (g GI) Validate() error {
	if g.Timeout <= 0 {
		return fmt.Errorf("%w: gi timeout=%s must be positive", ErrConfig, g.Timeout)
	}
	if g.Period < 0 {
		return fmt.Errorf("%w: gi period=%s must not be negative", ErrConfig, g.Period)
	}
	if g.Period > 0 && g.Period <= g.Timeout {
		// A period inside the timeout means the next interrogation is due
		// before the previous one has been given up on, and the two would
		// overlap on a station that is merely slow.
		return fmt.Errorf("%w: gi period=%s must exceed the timeout=%s", ErrConfig, g.Period, g.Timeout)
	}
	return nil
}

// interrogate runs one general interrogation and returns when the bracket
// closes.
//
// The station is left in StateTopical on success. On any failure the caller
// drops the connection: there is no way to cancel an interrogation in flight,
// so a fresh connection is the only clean restart.
func (s *Station) interrogate(ctx context.Context, send sender) error {
	s.setState(StateInterrogating, nil)

	// The semaphore is taken BEFORE the clock starts, so a station that spent
	// four minutes queued behind nine others does not report a four-minute
	// interrogation. iec104_gi_duration_seconds is what sizes GI_CONCURRENCY
	// (design §5.5), and a number that includes the waiting caused by that very
	// limit would make the setting tune itself in a circle.
	release, err := s.cfg.GIGate.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("waiting for an interrogation slot: %w", err)
	}
	defer release()

	started := s.cfg.Clock.Now()

	// Drain a stale reply before asking. A confirmation left over from the
	// previous interrogation would close this one instantly, and the cache
	// would be declared complete while the station was still filling it.
	s.drainGI()

	// Open a new generation. Everything the station reports from here belongs
	// to this snapshot, and whatever is not refreshed by the end of it is not
	// on the device any more.
	s.cache.beginInterrogation()

	frame, err := s.cfg.ASDUParams.AppendInterrogation(
		nil, s.cfg.CommonAddress, s.cfg.Originator, asdu.CauseActivation, asdu.QOIStation)
	if err != nil {
		return fmt.Errorf("encoding the interrogation: %w", err)
	}
	// From here until the bracket closes, interrogation traffic is expected.
	// Outside this window it is not, and inspect says so.
	s.giOpen.Store(true)
	defer s.giOpen.Store(false)

	if err := send(ctx, frame); err != nil {
		return fmt.Errorf("sending the interrogation: %w", err)
	}

	timeout := s.cfg.Clock.NewTimer(s.cfg.GI.Timeout)
	defer timeout.Stop()

	// The two halves of the bracket, in order. Waiting for the termination
	// without first seeing the confirmation would accept a stray actterm from
	// an interrogation somebody else started.
	if err := s.awaitGI(ctx, asdu.CauseActCon, timeout.C()); err != nil {
		return err
	}
	if err := s.awaitGI(ctx, asdu.CauseActTerm, timeout.C()); err != nil {
		return err
	}

	// The bracket closed, so the snapshot is complete and anything it did not
	// mention is gone. A timed-out interrogation deliberately does NOT reach
	// here: what it collected is a subset of unknown size, and treating the
	// points it happened to miss as deleted would turn a slow station into one
	// that appears to have lost half its configuration.
	s.cache.endInterrogation()

	s.cfg.Observer.OnGI(s.cfg.Clock.Now().Sub(started), nil)
	s.setState(StateTopical, nil)
	return nil
}

// awaitGI waits for one half of the bracket.
func (s *Station) awaitGI(ctx context.Context, want asdu.Cause, timeout <-chan time.Time) error {
	for {
		select {
		case h := <-s.giFrames:
			if h.Negative {
				return fmt.Errorf("%w: %s", ErrGIRefused, h.Cause)
			}
			if h.Cause == want {
				return nil
			}
			if want == asdu.CauseActCon && h.Cause == asdu.CauseActTerm {
				// The termination cannot precede the confirmation: the
				// confirmation is what says the device accepted the request at
				// all. Tolerated, because a device that does this is usually
				// also repeating itself and a matching confirmation will turn
				// up — but recorded, because a device that does it ONCE leaves
				// this interrogation to time out, and the operator needs to be
				// told why rather than watching it reconnect for ever.
				s.cfg.Observer.OnAnomaly(AnomalyBracketOutOfOrder,
					"act-term arrived before act-con")
				continue
			}
			// Some other cause on C_IC_NA_1. Not an error in itself — a station
			// may repeat a confirmation — so keep waiting rather than failing on
			// something harmless.

		case reason := <-s.broken:
			// The connection died mid-bracket. Without this case the
			// interrogation would sit out its whole timeout waiting for frames
			// from a socket that is already gone — a minute of a station
			// reporting "interrogating" when it is not even connected, and a
			// minute added to every reconnection.
			return fmt.Errorf("connection ended during the interrogation: %w", reason)

		case <-timeout:
			err := fmt.Errorf("%w: no %s within %s", ErrGITimeout, want, s.cfg.GI.Timeout)
			s.cfg.Observer.OnGI(s.cfg.GI.Timeout, err)
			return err

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// drainGI empties the interrogation channel.
func (s *Station) drainGI() {
	for {
		select {
		case <-s.giFrames:
		default:
			return
		}
	}
}

// inspect looks at an incoming ASDU for the two things the lifecycle cares
// about, and passes it on unchanged.
//
// It runs on the connection's runLoop, so it parses the fixed
// header and nothing else, and every signal it sends is non-blocking. The
// interpretation of the objects — what a type ID means, which point moved — is
// not this service's business at all.
func (s *Station) inspect(raw []byte) {
	h, body, err := s.cfg.ASDUParams.ParseHeader(raw)
	if err != nil {
		// Malformed, or a type this header parser does not cover. The observer
		// still gets the bytes and decides; the lifecycle has nothing to do.
		return
	}
	if h.CommonAddr != s.cfg.CommonAddress {
		// Not this station. Under decision G4 one connection carries one
		// station, so this means the device is configured for a different
		// common address than we were told — the observer will see it, and
		// acting on it here would be acting on somebody else's data.
		return
	}

	if h.Type.IsMonitor() {
		s.absorb(h, body)
		return
	}

	if asdu.SupportsControlType(h.Type) {
		s.routeCommandReply(h, body)
		return
	}

	switch h.Type {
	case asdu.CIcNa1:
		if !s.giOpen.Load() {
			s.cfg.Observer.OnAnomaly(AnomalyUnsolicitedInterrogation,
				"C_IC_NA_1 "+h.Cause.String()+" with no interrogation outstanding")
		}
		// A dropped signal would stall the bracket, so the channel is buffered
		// and a full one means a confirmation is already waiting to be read —
		// the same news twice.
		select {
		case s.giFrames <- h:
		default:
		}

	case asdu.CCsNa1:
		select {
		case s.clockFrames <- h:
		default:
		}

	case asdu.MEiNa1:
		// Study Reference §10: the station announces it has restarted, which
		// means everything cached from before it is from a different life of
		// the device. Re-interrogate.
		select {
		case s.reinit <- struct{}{}:
		default:
		}
	}
}

// absorb decodes a monitor-direction ASDU into the cache.
//
// It runs on the connection's runLoop, so the cost has to be bounded by the
// frame, and it is: one parse and one map write per information object. What it
// must not do is anything that can block — a Redis write here would stall the
// goroutine that answers t1 and turn a cache slowdown into a lost field
// connection.
//
// A type this package cannot decode is not an error here. The observer still
// receives the raw bytes and can count the rejection; refusing to cache the rest
// of a frame because one object was unfamiliar would lose good data over an
// unknown neighbour.
func (s *Station) absorb(h asdu.Header, body []byte) {
	points, err := s.cfg.ASDUParams.ParseMonitor(h, body)
	if err != nil {
		return
	}

	// Reported once per ASDU rather than once per point: the offence is the
	// frame, and a sixteen-object frame is not sixteen times as wrong.
	//
	// The data is still cached. It is real, current data from the device, and
	// discarding it to make a point about the specification would lose readings
	// an operator needs; what the device is doing wrong is announcing an
	// interrogation nobody asked for, not measuring badly.
	if h.Cause.IsInterrogation() && !s.giOpen.Load() {
		s.cfg.Observer.OnAnomaly(AnomalyUnsolicitedInterrogation,
			h.Type.String()+" with cause "+h.Cause.String()+" and no interrogation outstanding")
	}

	// One instant for the whole frame: two points from the same ASDU differing
	// by a microsecond of processing would be a difference in OUR clock, not in
	// the device's, and the skew is supposed to measure theirs.
	now := s.cfg.Clock.Now()

	for _, p := range points {
		r := s.reading(p, now)
		s.cache.store(r)
		s.noteSkew(r)
		if isEvent(p) {
			e := Event{Reading: r}
			if p.Type.IsContact() {
				e.ChatterSuppressed = s.chatter.observe(r, now)
			}
			s.cfg.Observer.OnEvent(e)
		}
	}
}
