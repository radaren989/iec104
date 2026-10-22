// Package station owns the life of one station: dialling it, running the
// handshake, holding the connection, and deciding what to do when it breaks.
//
// The unit here is one station, because decision G4 makes one station one
// connection. Everything below this package is about frames; everything above
// is about a fleet. This is the layer that turns "the socket died" into "wait,
// then try again", which at a thousand stations is the difference between a
// restart and an outage.
package station

import "fmt"

// State is where a station stands. It follows the state machine in the design
// document §5.4.
//
// The states are not decoration: DeviceManager reads them. StateStarted in
// particular means connected but NOT yet topical — the cache is empty or stale
// until the general interrogation finishes, and a reading taken in that window
// has to say so rather than look like data.
type State uint8

const (
	// StateDisconnected is the resting state: no socket, nothing scheduled.
	// A station is born here and returns here after each backoff.
	StateDisconnected State = iota

	// StateQueued means this station wants to dial and is waiting for
	// permission.
	//
	// This state does not exist in the protocol and was not in the earlier
	// design. It is what decision G1 forced: one instance holding a thousand
	// stations cannot dial them all at once, so the wish to connect and the act
	// of connecting became two different things.
	StateQueued

	// StateConnecting is the TCP dial, bounded by t0.
	StateConnecting

	// StateStarted means the connection is up and STARTDT is confirmed. Data
	// may flow, but nothing has been interrogated yet, so nothing is topical.
	StateStarted

	// StateInterrogating means a general interrogation is in progress: the
	// station is answering, the cache is filling, and readings are still not
	// topical until the terminating confirmation arrives.
	StateInterrogating

	// StateTopical means the cache reflects the station. This is the only state
	// in which a reading stands on its own.
	StateTopical

	// StateFailed is the moment after something broke and before the backoff.
	// It is a real state rather than a transient because the reason a station
	// failed is what the operator needs, and a state that is never observed
	// cannot be reported.
	StateFailed

	// StateReleased is terminal: the station was given up, either explicitly or
	// because its lease expired. Nothing restarts from here.
	StateReleased
)

func (s State) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateQueued:
		return "queued"
	case StateConnecting:
		return "connecting"
	case StateStarted:
		return "started"
	case StateInterrogating:
		return "interrogating"
	case StateTopical:
		return "topical"
	case StateFailed:
		return "failed"
	case StateReleased:
		return "released"
	default:
		return fmt.Sprintf("invalid state %d", uint8(s))
	}
}

// Topical reports whether readings taken now stand on their own.
//
// It is one state today and a method rather than a comparison on purpose: this
// is the question the gRPC layer actually asks, and the answer is a property of
// the station, not something every caller should re-derive from the enum.
func (s State) Topical() bool { return s == StateTopical }

// Connected reports whether a socket is up. Distinct from Topical: a connected
// station that has not finished interrogating serves stale data, and the two
// questions have different answers for a window on every single connection.
func (s State) Connected() bool {
	return s == StateStarted || s == StateInterrogating || s == StateTopical
}

// Terminal reports whether nothing further will happen.
func (s State) Terminal() bool { return s == StateReleased }

// transitions is the state machine of design §5.4, written out.
//
// It is a table rather than a set of if-statements because the interesting
// property is the whole graph, not any one edge: what must never happen is
// Connecting jumping straight to Topical, or Failed leading anywhere but back
// to Disconnected through the backoff. A table can be read, tested
// exhaustively, and compared against the diagram by eye.
var transitions = map[State][]State{
	StateDisconnected:  {StateQueued, StateReleased},
	StateQueued:        {StateConnecting, StateFailed, StateReleased},
	StateConnecting:    {StateStarted, StateFailed, StateReleased},
	StateStarted:       {StateInterrogating, StateFailed, StateReleased},
	StateInterrogating: {StateTopical, StateFailed, StateReleased},

	// Topical -> Interrogating is the periodic re-interrogation, and
	// Topical -> Topical is deliberately absent: staying put is not a
	// transition, and allowing it would make "the state changed" untestable.
	StateTopical: {StateInterrogating, StateFailed, StateReleased},

	// Failed leads one way only. Every retry goes back through Disconnected so
	// that the backoff is on the single path out, and cannot be skipped by a
	// future shortcut from Failed straight to Connecting.
	StateFailed: {StateDisconnected, StateReleased},

	// Terminal.
	StateReleased: nil,
}

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to State) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
