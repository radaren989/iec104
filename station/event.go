package station

import "github.com/radaren989/iec104/asdu"

// This file decides which readings are events.
//
// The costliest single mistake available to this service is to treat an
// interrogation response as an event. A general interrogation asks the device
// for every point it has, and it runs on every reconnect: if its answers were
// events, every link flap would replay the whole point list into the event
// stream — one false alarm, and one SMS, per point, per reconnect.
//
// Why here and not in eventsink. The decision is made at the source, on the
// connection's runLoop, before anything is queued. The event plane's buffer is
// bounded and drops its oldest entry when full; a ten-thousand
// point interrogation pushed into it only to be filtered out on the far side
// would evict the genuine events queued ahead of it. The rule costs one
// comparison per point, which is the only kind of work runLoop can afford.

// Event is a reading that is an event, with what the chatter filter made of it.
type Event struct {
	Reading

	// ChatterSuppressed says this contact has changed more often than the
	// station's chatter policy allows (chatter.go). The event is published
	// anyway — the contract's chatter_suppressed — so the alarm system learns
	// the contact is bouncing instead of learning nothing. Always false for
	// anything that is not a contact.
	ChatterSuppressed bool
}

// isEvent reports whether a reading is an event.
//
// Two conditions, both required:
//
//   - The cause is a change report (3, 11 or 12, asdu.Cause.IsChangeReport).
//     Periodic data (1) is not an event: it refreshes the cache and says
//     nothing about whether the value moved. That is the default the task
//     asks for, and it is not configurable yet because nothing asks for
//     anything else.
//   - The T bit is clear. A test frame "must not be acted on", and the contract
//     has no field to carry the bit, so a published test report would reach
//     the alarm system as indistinguishable from a real one.
//
// The decision does not depend on whether an interrogation is open. A value
// that changes while the device is answering one is still a change, and is
// still reported: suppressing events for the duration would lose real alarms
// on every reconnect and on every periodic interrogation.
func isEvent(p asdu.Point) bool {
	return p.Cause.IsChangeReport() && !p.Test
}
