package station

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// This file is the answer to a question 104 does not answer: "what is this point
// right now?"
//
// The protocol has no reliable read primitive. C_RD exists and every vendor
// discourages it; what actually happens is that a station reports a value when
// it changes and when it is interrogated, and the master remembers. So the
// master's memory IS the answer, and DeviceManager samples it (design §1).
//
// Two properties make that memory trustworthy, and both are easy to lose:
// readings are COPIES, so a caller cannot reach back into the cache through the
// value it was given; and a point that no longer exists on the device does not
// survive the next interrogation.

// ErrNotTopical means the station's cache does not yet reflect the device.
var ErrNotTopical = errors.New("station is not topical")

// cache is the last value seen for each information object address.
//
// Only the connection's handler goroutine writes it; readers are gRPC handlers,
// which take the read lock and leave with a copy. That asymmetry is why it is an
// RWMutex rather than a channel: a thousand stations times a few hundred points
// is a lot of memory to hand between goroutines for what is a map lookup.
type cache struct {
	mu     sync.RWMutex
	points map[asdu.IOA]entry

	// generation counts interrogations. Every write stamps the current one, and
	// the sweep after an interrogation drops whatever it did not touch.
	//
	// This is what stops a ghost: a point removed from the device's
	// configuration would otherwise sit in the cache forever, be served as part
	// of a topical station, and look exactly like a real reading that happens
	// not to have changed in a while. A general interrogation is a COMPLETE
	// snapshot — we always ask with QOI=20, the whole station — so anything
	// missing from it is missing from the device.
	generation uint64
}

// Reading is a cached point plus what could only be worked out when it arrived.
//
// The two derived fields are here rather than computed on read because the
// contract asks for the skew "at the time of reception", and because the
// conversion to an absolute instant needs the station's time zone — which is a
// property of the device and has no business travelling up to the gRPC layer.
// Computing either of them later would give a different, and wrong, answer: the
// skew would grow with the age of the reading.
type Reading struct {
	Point asdu.Point

	// TSUTCMillis is the device's timestamp as epoch milliseconds, which is
	// what the contract's ts_utc carries. Zero when the type had no timestamp
	// or the device marked it unusable — ask TSValid rather than comparing.
	TSUTCMillis int64

	// TSValid says the device sent a timestamp we could resolve. A false here
	// with a non-zero TSUTCMillis is impossible by construction.
	TSValid bool

	// SkewMillis is the device clock minus ours at the instant the frame
	// arrived. It is the input to the clock-skew metric and to the .NET side's
	// _TsSuspect flag.
	SkewMillis int64
}

type entry struct {
	reading    Reading
	generation uint64
}

func newCache() *cache {
	return &cache{points: make(map[asdu.IOA]entry)}
}

// store records one reading.
func (c *cache) store(r Reading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.points[r.Point.IOA] = entry{reading: r, generation: c.generation}
}

// beginInterrogation opens a new generation. Everything written from now on is
// part of this snapshot.
func (c *cache) beginInterrogation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
}

// endInterrogation drops every point the interrogation did not refresh.
//
// It is called only when the bracket closed properly. A timed-out interrogation
// must NOT sweep: what it collected is a subset of unknown size, and treating
// the points it happened to miss as deleted would turn a slow station into a
// station that appears to have lost half its configuration.
func (c *cache) endInterrogation() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	dropped := 0
	for ioa, e := range c.points {
		if e.generation < c.generation {
			delete(c.points, ioa)
			dropped++
		}
	}
	return dropped
}

// read returns one reading.
//
// It is returned by value, and Reading is deliberately all value types, so this
// is a real copy rather than a promise of one. TestAReadingCanBeCopiedByAssignment
// is what keeps that true.
func (c *cache) read(ioa asdu.IOA) (Reading, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.points[ioa]
	return e.reading, ok
}

// readMany returns the points that are known and the addresses that are not.
//
// The split is the point: a caller asking for two hundred addresses needs to
// know which of them the station has never reported, and an answer that silently
// returned a hundred and ninety would be read as "the other ten are fine".
func (c *cache) readMany(ioas []asdu.IOA) (found []Reading, missing []asdu.IOA) {
	if len(ioas) == 0 {
		return nil, nil
	}

	// Sized once. A read of a two-hundred-point station is two allocations, not
	// two hundred: this is the allocation budget the task asks about, and it is
	// the kind of thing that only stays true if it is written down.
	found = make([]Reading, 0, len(ioas))

	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, ioa := range ioas {
		e, ok := c.points[ioa]
		if !ok {
			if missing == nil {
				missing = make([]asdu.IOA, 0, len(ioas)-len(found))
			}
			missing = append(missing, ioa)
			continue
		}
		found = append(found, e.reading)
	}
	return found, missing
}

// size is how many points are held. For tests and for a fleet gauge.
func (c *cache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.points)
}

// --- the station's side -----------------------------------------------------

// ReadPoint returns the last value seen for one address.
func (s *Station) ReadPoint(ioa asdu.IOA) (Reading, bool) { return s.cache.read(ioa) }

// ReadPoints returns the last values for the addresses given, and the ones the
// station has never reported.
//
// It does NOT check topicality: whether a non-topical reading is acceptable is
// the caller's decision, and a caller that wants to wait says so with
// WaitTopical. Refusing here would make the common case — an operator looking at
// a screen while a station reconnects — return nothing at all rather than the
// last known values with an honest "not current" beside them.
func (s *Station) ReadPoints(ioas []asdu.IOA) (found []Reading, missing []asdu.IOA) {
	return s.cache.readMany(ioas)
}

// WaitTopical blocks until the station's cache reflects the device, or the
// deadline passes.
//
// This is the contract's wait_topical_ms. A zero or negative wait checks once
// and returns, which is what a caller who does not want to wait means.
func (s *Station) WaitTopical(ctx context.Context, wait time.Duration) error {
	s.mu.Lock()
	state, ready := s.state, s.topical
	s.mu.Unlock()

	if state.Topical() {
		return nil
	}
	if wait <= 0 {
		return ErrNotTopical
	}

	timer := s.cfg.Clock.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ready:
		return nil
	case <-timer.C():
		return ErrNotTopical
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- keeping the copy honest ------------------------------------------------

// pointHasOnlyValueFields reports whether asdu.Point can be copied by
// assignment.
//
// The cache's central promise is that a reader cannot reach back into it through
// the value it was handed. That promise rests entirely on Point being made of
// value types: the day somebody adds a []byte or a map to it, every read starts
// sharing memory with the cache, and nothing fails — the copies simply stop
// being copies, and a caller that modifies what it was given corrupts the
// station's view of the field. This is checked rather than trusted.
func pointHasOnlyValueFields(t reflect.Type) (bad string, ok bool) {
	switch t.Kind() {
	case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Interface:
		return t.String(), false
	case reflect.Array:
		return pointHasOnlyValueFields(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			if name, fine := pointHasOnlyValueFields(f.Type); !fine {
				return f.Name + " (" + name + ")", false
			}
		}
	}
	return "", true
}
