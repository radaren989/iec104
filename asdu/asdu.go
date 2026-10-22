// Package asdu implements the IEC 60870-5-101/104 application layer: the ASDU
// header, the information objects it carries, their quality descriptors and
// their timestamps.
//
// Like apci this package is pure — bytes in, values out, and
// nothing else. purity_test.go enforces that.
//
// This is the part of the service where being wrong is quietest. A misparsed
// frame usually crashes or produces obvious nonsense; a misread quality bit
// produces a plausible number with the wrong metadata, and the alarm that should
// have explained itself simply does not. DeviceManager derives _Quality, _Cause
// and _TsSuspect from what comes out of here.
package asdu

import (
	"errors"
	"fmt"
)

// Field-width limits. The three widths below are per-device configuration, not
// constants of the protocol, and getting them wrong is the classic field
// problem: a master set to 1-octet COT talking to a device using 2 misparses
// every single frame, from the first one, in a way that often still looks like
// data.
const (
	MinCASDUOctets = 1
	MaxCASDUOctets = 2
	MinCOTOctets   = 1
	MaxCOTOctets   = 2
	MinIOAOctets   = 1
	MaxIOAOctets   = 3

	// MaxObjects is the largest count the variable structure qualifier can
	// express: seven bits.
	MaxObjects = 0x7F
)

// Errors. They are sentinels because the layers above react differently to a
// frame that is malformed (close the connection) and one that is merely
// unsupported (count it, drop it, keep the connection).
var (
	// ErrParams means the ASDU field widths are not a workable set.
	ErrParams = errors.New("invalid ASDU parameters")

	// ErrTruncated means the ASDU ended in the middle of something.
	ErrTruncated = errors.New("ASDU truncated")

	// ErrLength means the ASDU's length does not match what its header
	// promises. It is kept apart from ErrTruncated because the two point at
	// different mistakes: truncation is a short read, a length mismatch is a
	// header that disagrees with its own body.
	ErrLength = errors.New("ASDU length does not match its header")

	// ErrObjectCount means the variable structure qualifier says zero objects.
	// An ASDU exists to carry information objects; one with none is malformed.
	ErrObjectCount = errors.New("ASDU with no information objects")

	// ErrIOARange means an information object address does not fit the
	// configured width — on encode, or on the implied addresses of a sequence.
	ErrIOARange = errors.New("information object address out of range")
)

// IOA is an Information Object Address: the per-point address within a station.
// Together with the common address and the type ID it identifies one point.
type IOA uint32

// maxIOA is the largest address each width can carry.
func maxIOA(octets uint8) IOA {
	return IOA(1)<<(8*IOA(octets)) - 1
}

// Params is the per-device ASDU field configuration.
//
// These are carried per station rather than assumed because the standard allows
// all three widths to vary and devices genuinely do vary. The defaults below are
// what 104 normally uses, and what the reference RTU uses.
type Params struct {
	// CASDUOctets is the width of the common address of the ASDU — the
	// "station address". 1 or 2; 2 is normal for 104.
	CASDUOctets uint8

	// COTOctets is the width of the cause of transmission. 1 or 2; when it is
	// 2, the second octet is the originator address.
	COTOctets uint8

	// IOAOctets is the width of an information object address. 1, 2 or 3;
	// 3 is standard for 104.
	IOAOctets uint8
}

// DefaultParams returns the widths 104 normally uses.
func DefaultParams() Params {
	return Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: 3}
}

// Validate reports why the widths cannot work, if they cannot.
func (p Params) Validate() error {
	switch {
	case p.CASDUOctets < MinCASDUOctets || p.CASDUOctets > MaxCASDUOctets:
		return fmt.Errorf("%w: casdu_octets=%d is outside %d..%d",
			ErrParams, p.CASDUOctets, MinCASDUOctets, MaxCASDUOctets)
	case p.COTOctets < MinCOTOctets || p.COTOctets > MaxCOTOctets:
		return fmt.Errorf("%w: cot_octets=%d is outside %d..%d",
			ErrParams, p.COTOctets, MinCOTOctets, MaxCOTOctets)
	case p.IOAOctets < MinIOAOctets || p.IOAOctets > MaxIOAOctets:
		return fmt.Errorf("%w: ioa_octets=%d is outside %d..%d",
			ErrParams, p.IOAOctets, MinIOAOctets, MaxIOAOctets)
	}
	return nil
}

// HeaderLen is how many octets the fixed part of an ASDU occupies: type ID,
// variable structure qualifier, cause of transmission and common address.
func (p Params) HeaderLen() int {
	return 2 + int(p.COTOctets) + int(p.CASDUOctets)
}

// MaxCommonAddr is the largest common address the configured width can carry.
//
// Note the reserved values: 0 addresses no station, and the all-ones value
// (255 or 65535) is the broadcast address. Range checking for those belongs to
// the layer that assigns addresses, not to the parser, which must be able to
// read whatever arrives.
func (p Params) MaxCommonAddr() uint16 {
	if p.CASDUOctets == 1 {
		return 0xFF
	}
	return 0xFFFF
}

// MaxIOA is the largest information object address the configured width can
// carry.
func (p Params) MaxIOA() IOA { return maxIOA(p.IOAOctets) }
