package station

// Anomaly is a device doing something the standard does not allow, in a way the
// connection survives.
//
// It is deliberately separate from an error. An error ends the interrogation or
// the connection; an anomaly is something this service tolerates in order to
// keep talking to equipment that is already installed, already commissioned and
// not going to be replaced because of a specification argument.
//
// Tolerating it silently is the thing to avoid. On 2026-09-28 a simulator was
// found sending its interrogation bracket in the wrong order — the termination
// before the confirmation — and repeating the whole response unbidden several
// times a second. The station coped, because the repetition meant a matching
// confirmation always turned up eventually; the acceptance was accidental. A
// device that made the same ordering mistake ONCE, without repeating, left the
// station stuck in a sixty-second timeout and then reconnecting for ever, with
// nothing anywhere saying why.
//
// That is the failure this type exists to prevent: not the anomaly itself, but
// the silence around it.
type AnomalyKind string

const (
	// AnomalyBracketOutOfOrder is an interrogation termination that arrived
	// while the confirmation it is supposed to follow was still outstanding.
	//
	// A repeated confirmation is harmless and is not reported. This is the
	// other thing: act-term cannot precede act-con, because the confirmation is
	// what says the device accepted the request at all.
	AnomalyBracketOutOfOrder AnomalyKind = "gi_bracket_out_of_order"

	// AnomalyUnsolicitedInterrogation is interrogation traffic — a C_IC_NA_1
	// confirmation or termination, or monitor data with an interrogation cause
	// — arriving when this station has no interrogation outstanding.
	//
	// The data itself is still cached: it is real, current data from the
	// device, and throwing it away to make a point about the specification
	// would lose readings an operator needs. What is recorded is that the
	// device is talking about an interrogation nobody asked for, which is how a
	// device stuck in a loop becomes visible instead of merely expensive.
	AnomalyUnsolicitedInterrogation AnomalyKind = "gi_unsolicited"
)

// String makes AnomalyKind printable without a conversion at every call site.
func (k AnomalyKind) String() string { return string(k) }

// anomalyKinds is every kind, for the tests that keep the metric label set and
// this list from drifting apart.
var anomalyKinds = []AnomalyKind{
	AnomalyBracketOutOfOrder,
	AnomalyUnsolicitedInterrogation,
}
