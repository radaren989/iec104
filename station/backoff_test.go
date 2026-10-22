package station

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestBackoffValidateRejectsWhatCannotWork(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		b    Backoff
		want bool // want an error
	}{
		{"the defaults", DefaultBackoff(), false},
		{"zero base", Backoff{Base: 0, Cap: time.Minute, Factor: 2}, true},
		{"negative base", Backoff{Base: -time.Second, Cap: time.Minute, Factor: 2}, true},
		{"cap below base", Backoff{Base: 10 * time.Second, Cap: time.Second, Factor: 2}, true},
		{"cap equal to base", Backoff{Base: time.Second, Cap: time.Second, Factor: 2}, false},
		{"factor below one", Backoff{Base: time.Second, Cap: time.Minute, Factor: 0.5}, true},
		{"factor exactly one", Backoff{Base: time.Second, Cap: time.Minute, Factor: 1}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.b.Validate()
			if (err != nil) != tc.want {
				t.Fatalf("Validate() = %v, want error: %v", err, tc.want)
			}
			if err != nil && !errors.Is(err, ErrBackoff) {
				t.Errorf("%v does not wrap ErrBackoff", err)
			}
		})
	}
}

func TestBackoffGrowsAndStopsAtTheCap(t *testing.T) {
	t.Parallel()

	b := DefaultBackoff() // 1s, cap 60s, factor 2; no jitter

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // 64s would exceed the cap
		{7, 60 * time.Second},
		{50, 60 * time.Second},
	}

	for _, tc := range cases {
		if got := b.Delay(tc.attempt); got != tc.want {
			t.Errorf("Delay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

// A station that has failed for a week is exactly the one still being retried
// when base * 2^attempt overflows an int64 nanosecond count. A negative delay
// turns the retry loop into a busy loop against a device that is already down.
func TestBackoffDoesNotOverflowAtAbsurdAttemptCounts(t *testing.T) {
	t.Parallel()

	b := DefaultBackoff()
	for _, attempt := range []int{62, 63, 64, 1000, math.MaxInt32} {
		got := b.Delay(attempt)
		if got != b.Cap {
			t.Errorf("Delay(%d) = %s, want the cap %s", attempt, got, b.Cap)
		}
	}
}

func TestBackoffTreatsANegativeAttemptAsTheFirst(t *testing.T) {
	t.Parallel()

	b := DefaultBackoff()
	if got, want := b.Delay(-1), b.Base; got != want {
		t.Errorf("Delay(-1) = %s, want %s", got, want)
	}
}

// Jitter is what stops a substation-wide blip from becoming a herd: a thousand
// connections drop at the same instant, and without it all thousand retry at
// the same instant too, forever.
func TestJitterSpreadsRetriesButKeepsAFloor(t *testing.T) {
	t.Parallel()

	b := DefaultBackoff()

	cases := []struct {
		name    string
		draw    float64
		attempt int
		want    time.Duration
	}{
		// Half the delay is fixed. Full jitter — a uniform draw over the whole
		// interval — spreads marginally better but can return nearly zero, and
		// a permanently dead station would then be retried in a tight loop on
		// its unlucky draws.
		{"the unluckiest draw still waits half", 0, 3, 4 * time.Second},
		{"the middle draw", 0.5, 3, 6 * time.Second},
		{"just under the luckiest", 0.999999, 3, 8 * time.Second},
		{"at the cap the floor is 30s", 0, 10, 30 * time.Second},
		{"at the cap the ceiling is 60s", 0.999999, 10, 60 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := b
			b.Rand = func() float64 { return tc.draw }

			got := b.Delay(tc.attempt)
			plateau := b.plateau(tc.attempt)

			if got < plateau/2 || got > plateau {
				t.Errorf("Delay(%d) = %s, outside [%s, %s]", tc.attempt, got, plateau/2, plateau)
			}
			// A millisecond of slack, because a draw of 0.999999 lands that far
			// short of the top of a thirty-second range. Still tight enough to
			// catch a floor or a range computed wrongly, which is what this
			// assertion is for.
			if diff := got - tc.want; diff > time.Millisecond || diff < -time.Millisecond {
				t.Errorf("Delay(%d) = %s, want about %s", tc.attempt, got, tc.want)
			}
			if got > b.Cap {
				t.Errorf("Delay(%d) = %s exceeds the cap %s", tc.attempt, got, b.Cap)
			}
		})
	}
}

// The ceiling is the promise the design makes (§5.4). It has to hold for every
// attempt and every draw, not just the ones a table happens to list.
func TestNoDelayEverExceedsTheCap(t *testing.T) {
	t.Parallel()

	b := DefaultBackoff()
	draws := []float64{0, 0.25, 0.5, 0.75, 0.9999999}

	for _, draw := range draws {
		b := b
		b.Rand = func() float64 { return draw }
		for attempt := range 200 {
			if d := b.Delay(attempt); d > b.Cap || d <= 0 {
				t.Fatalf("Delay(%d) with draw %v = %s; must be in (0, %s]", attempt, draw, d, b.Cap)
			}
		}
	}
}
