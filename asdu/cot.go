package asdu

// This file is about what a cause of transmission MEANS, as opposed to what
// number it is. The constants live with the other identifiers in typeid.go; the
// classification lives here because it is the part that other packages reason
// with, and because it is where a misreading is expensive rather than merely
// wrong.

// IsNegativeConfirmation reports whether the cause is one of the four the
// station uses to say "I did not understand that": 44 unknown type, 45 unknown
// cause, 46 unknown common address, 47 unknown information object address.
//
// These four are the ONLY signal that the data map is wrong. Nothing else
// reports it: a point configured with a type the device does not have, or an
// address that does not exist, produces no error anywhere else in the system —
// the command is simply never carried out, or the value simply never arrives,
// and both look like a quiet field. Swallowing these turns a five-minute
// configuration fix into a site visit.
func (c Cause) IsNegativeConfirmation() bool {
	return c >= CauseUnknownType && c <= CauseUnknownIOA
}

// IsConfirmation reports whether the cause is an answer to something this side
// asked for, rather than data arriving on the station's own initiative.
func (c Cause) IsConfirmation() bool {
	switch c {
	case CauseActCon, CauseDeactCon, CauseActTerm:
		return true
	default:
		return c.IsNegativeConfirmation()
	}
}

// IsCommandFeedback reports whether the cause says a monitored value changed
// BECAUSE of a command: 11 by a remote command, 12 by a local operator.
//
// This is the difference between "the breaker opened" and "the breaker opened
// because someone opened it", and it is the whole reason an alarm can explain
// itself. DeviceManager builds _Cause from it. Dropping it does not lose the
// value — it loses the answer to "why".
func (c Cause) IsCommandFeedback() bool {
	return c == CauseRetRem || c == CauseRetLoc
}

// IsChangeReport reports whether the cause says "this value changed": by itself
// (3), because of a remote command (11) or because of a local operator (12).
//
// Every other cause on monitor-direction data describes a value that was sent for
// some other reason — the cycle came round (1), a background scan (2), somebody
// read it (5), an interrogation asked for everything (20, 21–36) — and says
// nothing about whether it moved. The event plane is built on this distinction:
// a reading with one of these three causes is an event, everything else only
// refreshes the cache.
//
// The list is closed on purpose. A cause that is not on it is not an event, so a
// cause nobody has thought about costs a missing entry in a stream rather than an
// SMS for every point in the station.
func (c Cause) IsChangeReport() bool {
	return c == CauseSpontaneous || c.IsCommandFeedback()
}

// Station-side reject reasons. They are separate from the gateway-side ones in
// forbidden.go on purpose, and the distinction is the point of the metric:
//
//	unknown_type          we cannot decode what the station sent
//	station_unknown_type  the station cannot understand what we sent
//
// The first is a gap in this gateway. The second is a data map that does not
// match the device — a different problem, a different owner, and a different
// fix. One label for both would make the dashboard useless exactly when it
// matters.
const (
	ReasonStationUnknownType          RejectReason = "station_unknown_type"
	ReasonStationUnknownCause         RejectReason = "station_unknown_cause"
	ReasonStationUnknownCommonAddress RejectReason = "station_unknown_common_address"
	ReasonStationUnknownIOA           RejectReason = "station_unknown_ioa"
)

// RejectReason returns the metric label for a negative confirmation, and whether
// the cause was one.
func (c Cause) RejectReason() (RejectReason, bool) {
	switch c {
	case CauseUnknownType:
		return ReasonStationUnknownType, true
	case CauseUnknownCause:
		return ReasonStationUnknownCause, true
	case CauseUnknownCommonAd:
		return ReasonStationUnknownCommonAddress, true
	case CauseUnknownIOA:
		return ReasonStationUnknownIOA, true
	default:
		return "", false
	}
}

// Explain returns a sentence an operator can act on, for the four negative
// confirmations. It is written for whoever reads the log at three in the
// morning, which is the only audience these messages ever have.
func (c Cause) Explain() (string, bool) {
	switch c {
	case CauseUnknownType:
		return "the station does not support this ASDU type; check the data map's type id against the device's interrogation list", true
	case CauseUnknownCause:
		return "the station does not accept this cause of transmission for this type; check whether the command is being sent as an activation", true
	case CauseUnknownCommonAd:
		return "the station does not answer to this common address; check the station's configured CASDU and the casdu_octets width", true
	case CauseUnknownIOA:
		return "the station has no point at this information object address; check the data map's IOA against the device's point list", true
	default:
		return "", false
	}
}
