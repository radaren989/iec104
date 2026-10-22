package station

import (
	"context"
	"fmt"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// This file is about the device's clock, and about the fact that we cannot
// simply believe it.
//
// A CP56Time2a carries LOCAL time with no offset. Its SU bit says daylight
// saving is in effect but not by how much, so turning one into an absolute
// instant needs to know which zone the device's clock is set to. Guessing costs
// whole hours, silently, on every timestamp the station ever sends — and the
// values still look entirely reasonable afterwards.
//
// And even with the zone right, the device's clock may be wrong. A skewed RTU
// clock produces alarms with the wrong time, which is worse than a missing
// alarm: a missing alarm is noticed, while a mistimed one quietly corrupts the
// ordering that operators reason with when they reconstruct what happened.

// ClockSync is the per-station clock policy.
type ClockSync struct {
	// Enabled sends C_CS_NA_1 to set the device's clock.
	//
	// OFF by default and deliberately opt-in (assumption V10, question A8):
	// many sites already synchronise their RTUs from their own SCADA, and two
	// masters setting the same clock is worse than none — they fight, and the
	// timestamps jitter between two sources instead of drifting predictably
	// from one.
	Enabled bool

	// Period is how often to send it. Ignored when Enabled is false.
	Period time.Duration

	// WarnAbove is the skew at which the station is worth complaining about.
	// Zero disables the warning; the measurement still happens, because the
	// metric is useful even where nobody set a threshold.
	WarnAbove time.Duration

	// Zone is the IANA name of the zone the DEVICE's clock is set to, e.g.
	// "Europe/Istanbul". Empty means UTC.
	//
	// It is a property of the device and not of the site: an RTU whose clock is
	// set to UTC inside a plant that runs on local time is configured empty.
	Zone string

	// loc is Zone resolved. Filled by Validate.
	loc *time.Location
}

// Defaults for the clock policy.
const (
	// DefaultClockSyncPeriod is how often a station with sync enabled is set.
	// An RTU's clock drifts by seconds a day at worst, so hourly is far more
	// often than drift requires — and that is the point: it is frequent enough
	// that a device which has been power-cycled and come back with a wrong
	// clock is corrected within the hour rather than within the day.
	DefaultClockSyncPeriod = time.Hour

	// DefaultClockSkewWarn is the threshold at which skew is worth a warning.
	// Two seconds is far beyond what a working setup produces and far below
	// what breaks event ordering.
	DefaultClockSkewWarn = 2 * time.Second
)

// DefaultClockSync returns the policy a station gets when it specifies none:
// measure, warn, and do NOT set the clock.
func DefaultClockSync() ClockSync {
	return ClockSync{WarnAbove: DefaultClockSkewWarn}
}

// Validate resolves the zone and rejects a policy that cannot work.
func (c *ClockSync) Validate() error {
	if c.Zone == "" {
		c.loc = time.UTC
	} else {
		loc, err := time.LoadLocation(c.Zone)
		if err != nil {
			// Not a fallback to UTC, deliberately. Falling back would produce
			// exactly the silent hour-sized error the zone field exists to
			// prevent, on every timestamp, forever. Refusing the station is
			// loud and fixable.
			return fmt.Errorf("%w: time zone %q: %w", ErrConfig, c.Zone, err)
		}
		c.loc = loc
	}

	if c.WarnAbove < 0 {
		return fmt.Errorf("%w: clock skew warning threshold %s is negative", ErrConfig, c.WarnAbove)
	}
	if !c.Enabled {
		return nil
	}
	if c.Period <= 0 {
		c.Period = DefaultClockSyncPeriod
	}
	return nil
}

// location is the zone the device's clock is set to.
func (c ClockSync) location() *time.Location {
	if c.loc == nil {
		return time.UTC
	}
	return c.loc
}

// reading turns a parsed point into what the cache holds, resolving its
// timestamp and measuring the device's clock against ours.
//
// now is passed in rather than read here so that every point in one frame is
// measured against the same instant: two points from the same ASDU differing by
// a microsecond of processing would be a difference in our clock, not in theirs.
func (s *Station) reading(p asdu.Point, now time.Time) Reading {
	r := Reading{Point: p}

	if !p.HasTime {
		return r
	}
	// Usable covers both the device's own IV bit and a tag whose calendar
	// fields are out of range. Either way the instant is not something to
	// measure against, and pretending otherwise would put a fabricated skew
	// into the metric.
	if !p.Time.Usable() {
		return r
	}

	at, ok := p.Time.Time(s.cfg.ClockSync.location())
	if !ok {
		return r
	}

	r.TSValid = true
	r.TSUTCMillis = at.UnixMilli()
	r.SkewMillis = at.Sub(now).Milliseconds()
	return r
}

// noteSkew reports the device's clock offset.
//
// It is called for every timestamped reading rather than once per frame: the
// metric is a gauge of the current offset, and a station whose clock jumps
// mid-frame is exactly the station worth seeing jump.
func (s *Station) noteSkew(r Reading) {
	if !r.TSValid {
		return
	}
	skew := time.Duration(r.SkewMillis) * time.Millisecond

	beyond := false
	if w := s.cfg.ClockSync.WarnAbove; w > 0 {
		beyond = skew > w || skew < -w
	}
	s.cfg.Observer.OnClockSkew(skew, beyond)
}

// syncClock sends one C_CS_NA_1 and waits for the station to confirm it.
//
// The confirmation is waited for rather than ignored because a device that
// silently drops clock synchronisation is indistinguishable, in the data, from
// one that applied it — and the difference is whether every timestamp from it is
// trustworthy.
func (s *Station) syncClock(ctx context.Context, send sender) error {
	s.drainClock()

	frame, err := s.cfg.ASDUParams.AppendClockSync(
		nil, s.cfg.CommonAddress, s.cfg.Originator, asdu.CauseActivation,
		asdu.CP56Time2aFrom(s.cfg.Clock.Now().In(s.cfg.ClockSync.location())))
	if err != nil {
		return fmt.Errorf("encoding the clock synchronisation: %w", err)
	}
	if err := send(ctx, frame); err != nil {
		return fmt.Errorf("sending the clock synchronisation: %w", err)
	}

	// t1 already bounds an unanswered activation at the APCI layer, and the
	// connection dies if it expires. This wait only has to outlive that.
	timeout := s.cfg.Clock.NewTimer(s.cfg.Params.T1 * 2)
	defer timeout.Stop()

	for {
		select {
		case h := <-s.clockFrames:
			if h.Negative {
				// Refusing is a legitimate answer — some devices will not take
				// a clock from a master at all — and it is not a reason to drop
				// a connection that is otherwise delivering data. It IS a
				// reason to stop pretending the setting is in effect.
				return fmt.Errorf("%w: the station refused clock synchronisation", ErrClockSyncRefused)
			}
			if h.Cause == asdu.CauseActCon {
				return nil
			}

		case reason := <-s.broken:
			return fmt.Errorf("connection ended during clock synchronisation: %w", reason)

		case <-timeout.C():
			return ErrClockSyncTimeout

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// drainClock empties the confirmation channel before a new dialogue.
func (s *Station) drainClock() {
	for {
		select {
		case <-s.clockFrames:
		default:
			return
		}
	}
}
