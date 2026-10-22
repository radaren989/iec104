package station

import (
	"slices"
	"strings"
	"testing"
)

// allStates is every state, and the tests below lean on it being complete: a
// state added without being listed here escapes the exhaustive checks, which is
// the one way this file could pass while saying nothing.
var allStates = []State{
	StateDisconnected,
	StateQueued,
	StateConnecting,
	StateStarted,
	StateInterrogating,
	StateTopical,
	StateFailed,
	StateReleased,
}

func TestAllStatesAreNamed(t *testing.T) {
	t.Parallel()

	seen := map[string]State{}
	for _, s := range allStates {
		name := s.String()
		if strings.HasPrefix(name, "invalid") {
			t.Errorf("%d has no name", uint8(s))
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%d and %d are both called %q", uint8(other), uint8(s), name)
		}
		seen[name] = s
	}

	// The list itself has to be complete. States are consecutive from zero, so
	// the one after the last named state must be unnamed.
	if next := State(len(allStates)); !strings.HasPrefix(next.String(), "invalid") {
		t.Errorf("allStates is missing %s; the exhaustive tests below are not exhaustive", next)
	}
}

// The transition table is the design document §5.4 written as code, so the
// interesting assertions are about the shape of the whole graph rather than any
// one edge.
func TestTheTransitionGraphMatchesTheDesign(t *testing.T) {
	t.Parallel()

	t.Run("every state can be released", func(t *testing.T) {
		t.Parallel()
		for _, s := range allStates {
			if s == StateReleased {
				continue
			}
			if !CanTransition(s, StateReleased) {
				t.Errorf("%s cannot be released; a lease expiring must be able to end any station", s)
			}
		}
	})

	t.Run("released is terminal", func(t *testing.T) {
		t.Parallel()
		for _, s := range allStates {
			if CanTransition(StateReleased, s) {
				t.Errorf("released -> %s is allowed; released must mean released", s)
			}
		}
		if !StateReleased.Terminal() {
			t.Error("StateReleased.Terminal() is false")
		}
	})

	t.Run("failure leads only back through disconnected", func(t *testing.T) {
		t.Parallel()
		want := []State{StateDisconnected, StateReleased}
		got := slices.Clone(transitions[StateFailed])
		slices.Sort(got)
		if !slices.Equal(got, want) {
			// The backoff sits on the path out of Failed. A shortcut from
			// Failed to Connecting would skip it, and a station that failed
			// because the RTU is rebooting would be dialled in a tight loop.
			t.Errorf("failed -> %v, want %v; the backoff must be unavoidable", got, want)
		}
	})

	t.Run("nothing reaches topical without interrogating", func(t *testing.T) {
		t.Parallel()
		for _, s := range allStates {
			if s == StateInterrogating {
				continue
			}
			if CanTransition(s, StateTopical) {
				// Topical means the cache reflects the device. The only thing
				// that can make that true is a completed interrogation.
				t.Errorf("%s -> topical is allowed; only a finished interrogation may make a station topical", s)
			}
		}
	})

	t.Run("no state transitions to itself", func(t *testing.T) {
		t.Parallel()
		for _, s := range allStates {
			if CanTransition(s, s) {
				t.Errorf("%s -> %s is allowed; staying put is not a transition and would make "+
					"'the state changed' untestable", s, s)
			}
		}
	})

	t.Run("the happy path is walkable", func(t *testing.T) {
		t.Parallel()
		path := []State{
			StateDisconnected, StateQueued, StateConnecting,
			StateStarted, StateInterrogating, StateTopical,
		}
		for i := 0; i+1 < len(path); i++ {
			if !CanTransition(path[i], path[i+1]) {
				t.Errorf("%s -> %s is not allowed; that is the connect path", path[i], path[i+1])
			}
		}
	})

	t.Run("every connected state can fail", func(t *testing.T) {
		t.Parallel()
		for _, s := range []State{StateQueued, StateConnecting, StateStarted, StateInterrogating, StateTopical} {
			if !CanTransition(s, StateFailed) {
				t.Errorf("%s cannot fail; a socket can break at any moment", s)
			}
		}
	})
}

// Connected and Topical answer different questions, and the window where they
// disagree exists on every single connection: between STARTDT con and the end
// of the interrogation the station is up and its data is stale.
func TestConnectedAndTopicalAreNotTheSameQuestion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state     State
		connected bool
		topical   bool
	}{
		{StateDisconnected, false, false},
		{StateQueued, false, false},
		{StateConnecting, false, false},
		{StateStarted, true, false},
		{StateInterrogating, true, false},
		{StateTopical, true, true},
		{StateFailed, false, false},
		{StateReleased, false, false},
	}

	if len(cases) != len(allStates) {
		t.Fatalf("%d cases for %d states", len(cases), len(allStates))
	}

	for _, tc := range cases {
		if got := tc.state.Connected(); got != tc.connected {
			t.Errorf("%s.Connected() = %v, want %v", tc.state, got, tc.connected)
		}
		if got := tc.state.Topical(); got != tc.topical {
			t.Errorf("%s.Topical() = %v, want %v", tc.state, got, tc.topical)
		}
	}
}
