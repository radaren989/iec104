package station

import (
	"fmt"
	"math"
	"time"
)

// Backoff defaults. They come from design §5.4 rather than from configuration,
// because a per-station knob here would let one misconfigured station hammer a
// field device that a thousand others are politely waiting for.
const (
	// DefaultBackoffBase is the wait after the first failure. Short enough that
	// a station which dropped for a moment comes back quickly.
	DefaultBackoffBase = 1 * time.Second

	// DefaultBackoffCap is the ceiling (design §5.4). A permanently dead RTU is
	// retried once a minute forever: often enough to notice it coming back,
	// rarely enough that a thousand dead ones cost nothing.
	DefaultBackoffCap = 60 * time.Second

	// DefaultBackoffFactor is the growth per attempt.
	DefaultBackoffFactor = 2.0
)

// ErrBackoff means the backoff parameters cannot work.
var ErrBackoff = fmt.Errorf("invalid backoff")

// Backoff computes how long to wait before the next dial.
//
// Two things matter and only one of them is obvious. The exponential part
// protects the station: a device that is rebooting should not be dialled fifty
// times while it boots. The jitter protects US: a substation-wide power blip
// drops a thousand connections at the same instant, and without jitter all
// thousand retry at the same instant too, forever, in a herd. The herd is the
// failure mode that takes the gateway down rather than the station.
type Backoff struct {
	// Base is the delay after the first failure.
	Base time.Duration

	// Cap is the ceiling the delay never exceeds, jitter included.
	Cap time.Duration

	// Factor is the multiplier per attempt. Must be at least 1.
	Factor float64

	// Rand returns a value in [0,1). Injected so the tests are a table rather
	// than a statistical argument; nil means math/rand via the caller, which is
	// why Station supplies one instead of Backoff reaching for a global.
	Rand func() float64
}

// DefaultBackoff returns the design's parameters. Rand is left nil; Station
// fills it in.
func DefaultBackoff() Backoff {
	return Backoff{
		Base:   DefaultBackoffBase,
		Cap:    DefaultBackoffCap,
		Factor: DefaultBackoffFactor,
	}
}

// Validate reports why the parameters cannot work, if they cannot.
func (b Backoff) Validate() error {
	switch {
	case b.Base <= 0:
		return fmt.Errorf("%w: base=%s must be positive", ErrBackoff, b.Base)
	case b.Cap < b.Base:
		return fmt.Errorf("%w: cap=%s is below base=%s", ErrBackoff, b.Cap, b.Base)
	case b.Factor < 1:
		return fmt.Errorf("%w: factor=%v would shrink the delay", ErrBackoff, b.Factor)
	}
	return nil
}

// Delay returns how long to wait before attempt n, counting from zero: Delay(0)
// is the wait after the first failure.
//
// The result is always in [Cap/2 bounded by the un-jittered delay, delay]. Half
// the delay is kept fixed on purpose. Full jitter — a uniform draw over the
// whole interval — spreads a herd slightly better, but it can return nearly
// zero, and a permanently failed station would then be retried in a tight loop
// on its unlucky draws. Keeping a floor costs a little spread and removes that
// entirely.
func (b Backoff) Delay(attempt int) time.Duration {
	d := b.plateau(attempt)

	if b.Rand == nil {
		return d
	}
	half := d / 2
	return half + time.Duration(b.Rand()*float64(d-half))
}

// plateau is the un-jittered delay for an attempt: base * factor^attempt,
// clamped to Cap.
//
// It computes in float64 and clamps before converting, because base * 2^attempt
// overflows an int64 nanosecond count somewhere around the sixtieth attempt —
// and a station that has failed sixty times is exactly the one still being
// retried when the overflow turns a one-minute wait into a negative one and the
// retry loop becomes a busy loop.
func (b Backoff) plateau(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	grown := float64(b.Base) * math.Pow(b.Factor, float64(attempt))
	if math.IsInf(grown, 0) || grown >= float64(b.Cap) {
		return b.Cap
	}
	return time.Duration(grown)
}
