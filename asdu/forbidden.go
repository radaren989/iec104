package asdu

import (
	"errors"
	"fmt"
)

// ErrForbiddenType means the ASDU uses a type that IEC 60870-5-104 does not
// allow, whatever the device thinks.
//
// It is deliberately separate from a parse failure. A malformed frame means the
// two sides disagree about the wire format and the connection is finished; a
// forbidden type means the device is speaking a dialect of the standard this
// profile excludes, and the right response is to count it, drop that ASDU, and
// keep the link up. Everything else the station sends is still good.
var ErrForbiddenType = errors.New("type is not permitted on IEC 60870-5-104")

// cp24Types are the eight monitor-direction types that carry a CP24Time2a short
// time tag.
//
// They are forbidden on 104 (they belong to the serial 101 profile) and the ban
// is not a preference to be relaxed for a device that sends them anyway.
// CP24Time2a carries only minutes and milliseconds — no hour, no date — so the
// receiver has to guess the rest from its own clock. On a serial link with a
// known scan cycle that guess is safe. On 104, where an RTU buffers events
// during an outage and delivers an hour of them at once, it is not: the guess
// puts every buffered event at the moment it arrived, which is precisely the
// information the timestamp existed to carry. Accepting them "leniently" would
// produce timestamps that look fine and are wrong by up to an hour.
var cp24Types = map[TypeID]bool{
	2: true, 4: true, 6: true, 8: true, 10: true, 12: true, 14: true, 16: true,
}

// ForbiddenOn104 reports whether this type may not appear on a 104 link.
func (t TypeID) ForbiddenOn104() bool { return cp24Types[t] }

// RejectReason is a short, stable label for why an ASDU was dropped. It is the
// value of the reason dimension on iec104_asdu_rejected_total, so it must stay
// low-cardinality and must not change casually: a renamed label silently breaks
// every dashboard and alert built on it.
type RejectReason string

const (
	// ReasonCP24Time is a type carrying a CP24Time2a short time tag.
	ReasonCP24Time RejectReason = "cp24_time_forbidden"

	// ReasonUnknownType is a type this gateway does not implement. It is not a
	// protocol violation — the station is allowed to send it, we simply have no
	// mapping — so it is counted separately from the forbidden ones.
	ReasonUnknownType RejectReason = "unknown_type"

	// ReasonMalformed is a frame that failed to parse.
	ReasonMalformed RejectReason = "malformed"

	// ReasonUnknownCommonAddress is an ASDU addressed to a station this
	// connection does not serve.
	ReasonUnknownCommonAddress RejectReason = "unknown_common_address"
)

// CheckPermitted returns an error when the type may not appear on 104.
func (t TypeID) CheckPermitted() error {
	if t.ForbiddenOn104() {
		return fmt.Errorf("%w: %s carries a CP24Time2a short time tag, which cannot be placed on a timeline",
			ErrForbiddenType, t)
	}
	return nil
}
