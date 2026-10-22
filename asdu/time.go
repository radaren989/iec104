package asdu

import (
	"encoding/binary"
	"fmt"
	"time"
)

// CP56Time2aLen is the fixed width of a full time tag.
const CP56Time2aLen = 7

// Bit layout of the CP56Time2a octets. The reserved bits are named here because
// they must be ignored on read, not masked into a neighbouring field — a
// mistake that shows up as a clock that is right most of the time.
const (
	cp56MinuteMask = 0x3F
	cp56InvalidBit = 0x80 // IV: the device says its own timestamp is wrong
	cp56HourMask   = 0x1F
	cp56SummerBit  = 0x80 // SU: the local time is summer time
	cp56DayMask    = 0x1F
	cp56DOWShift   = 5
	cp56MonthMask  = 0x0F
	cp56YearMask   = 0x7F
)

// centuryBase is the century the two-digit year is read into.
//
// IEC 60870-5-4 gives the year as two digits and leaves the century to be known
// "by other means", which in practice means assumed. Every 104 deployment is in
// this one, so 0..99 reads as 2000..2099 — a device reporting 99 means 2099, not
// 1999. A sliding window would be cleverer and would make the meaning of a
// stored timestamp depend on when it was read, which is worse.
const centuryBase = 2000

// CP56Time2a is a full seven-octet time tag: the device's own absolute instant.
//
// It is the reading time and must not be replaced with the gateway's clock. The
// difference matters exactly when it is hardest to reconstruct: a burst of
// events buffered in an RTU during a link outage arrives all at once, and only
// these timestamps say when each one actually happened.
//
// The fields are kept as the device sent them rather than normalised into a
// time.Time, because a device with a broken clock sends values that are not a
// date at all, and turning those into "some instant" silently invents data. See
// InRange and Time.
type CP56Time2a struct {
	// Millisecond is the seconds and milliseconds together, 0..59999.
	Millisecond uint16

	Minute uint8 // 0..59
	Hour   uint8 // 0..23
	Day    uint8 // 1..31

	// DayOfWeek is 1..7 for Monday..Sunday, or 0 when the device does not
	// supply it. It is never used to derive the date — it is redundant with it,
	// and when the two disagree the date is the one that was asked for.
	DayOfWeek uint8

	Month uint8 // 1..12
	Year  uint8 // 0..99, see centuryBase

	// Invalid is the IV bit: the device is telling you its own timestamp is
	// wrong, usually because its clock has never been set. The value that came
	// with it is still good — this flag belongs on the timestamp, not on the
	// measurement.
	Invalid bool

	// Summer is the SU bit: the local time being reported is summer time. It
	// does not say what the offset is, only that one is in effect, which is why
	// Time needs a location.
	Summer bool
}

// ParseCP56Time2a reads a time tag.
//
// It fails only on a short buffer. Out-of-range calendar fields are NOT an
// error: a device with an unset clock sends month 0 or day 0 quite happily, and
// dropping the measurement that came with it would lose good data over bad
// metadata. Ask InRange or Time whether the tag can be used.
func ParseCP56Time2a(b []byte) (CP56Time2a, error) {
	if len(b) < CP56Time2aLen {
		return CP56Time2a{}, fmt.Errorf("%w: %d octet(s), a CP56Time2a needs %d",
			ErrTruncated, len(b), CP56Time2aLen)
	}
	return CP56Time2a{
		Millisecond: binary.LittleEndian.Uint16(b[0:2]),
		Minute:      b[2] & cp56MinuteMask,
		Invalid:     b[2]&cp56InvalidBit != 0,
		Hour:        b[3] & cp56HourMask,
		Summer:      b[3]&cp56SummerBit != 0,
		Day:         b[4] & cp56DayMask,
		DayOfWeek:   b[4] >> cp56DOWShift,
		Month:       b[5] & cp56MonthMask,
		Year:        b[6] & cp56YearMask,
	}, nil
}

// AppendTo renders the time tag. Reserved bits are written as zero.
func (t CP56Time2a) AppendTo(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, t.Millisecond)

	minute := t.Minute & cp56MinuteMask
	if t.Invalid {
		minute |= cp56InvalidBit
	}
	hour := t.Hour & cp56HourMask
	if t.Summer {
		hour |= cp56SummerBit
	}

	return append(dst,
		minute,
		hour,
		t.Day&cp56DayMask|t.DayOfWeek<<cp56DOWShift,
		t.Month&cp56MonthMask,
		t.Year&cp56YearMask,
	)
}

// InRange reports whether every calendar field is a legal value, regardless of
// the IV bit. A tag can be in range and still marked invalid (the clock is
// running but was never set), or out of range with IV clear (a device that does
// not set IV when it should — which is common enough to be worth checking for).
func (t CP56Time2a) InRange() bool {
	return t.Millisecond < 60000 &&
		t.Minute < 60 &&
		t.Hour < 24 &&
		t.Day >= 1 && t.Day <= 31 &&
		t.Month >= 1 && t.Month <= 12 &&
		t.Year < 100 &&
		t.DayOfWeek <= 7
}

// Usable reports whether the tag can be trusted as a reading time: in range and
// not marked invalid by the device.
func (t CP56Time2a) Usable() bool { return !t.Invalid && t.InRange() }

// Time converts the tag to an instant in loc, which must be the station's local
// time zone. The second result is false when the calendar fields are not a date.
//
// A location is required rather than assumed because CP56Time2a carries local
// time with no offset: the SU bit says summer time is in effect but not by how
// much. Passing time.UTC is right only when the station's clock is set to UTC,
// and that is a per-station fact the gateway is told, not one it can work out.
//
// A nil loc is treated as UTC rather than panicking. time.Date panics on nil,
// and this function sits on the read path: a station whose configured zone
// failed to load, and whose nil Location was carried along instead of checked,
// would take down the read loop — and with it every other station on this
// gateway. A wrong time zone is a bad timestamp on one station; a panic is an
// outage on all of them, so the cheap guard wins.
//
// Note that a valid-looking tag can still be a date that does not exist, such as
// 31 February. time.Date normalises those forward rather than failing, and this
// deliberately does not second-guess it: a device reporting an impossible date
// has a broken clock, and the IV bit, not this function, is where that is said.
func (t CP56Time2a) Time(loc *time.Location) (time.Time, bool) {
	if !t.InRange() {
		return time.Time{}, false
	}
	if loc == nil {
		loc = time.UTC
	}
	sec := int(t.Millisecond / 1000)
	nsec := int(t.Millisecond%1000) * int(time.Millisecond)
	return time.Date(
		centuryBase+int(t.Year), time.Month(t.Month), int(t.Day),
		int(t.Hour), int(t.Minute), sec, nsec, loc,
	), true
}

// CP56Time2aFrom builds a time tag from an instant. The instant is used as it
// stands, so pass it already in the station's local zone.
//
// The SU bit is set from the zone's own knowledge of whether daylight saving is
// in effect, so a round trip through a real location preserves it.
func CP56Time2aFrom(ts time.Time) CP56Time2a {
	_, offset := ts.Zone()
	_, stdOffset := time.Date(ts.Year(), 1, 1, 0, 0, 0, 0, ts.Location()).Zone()

	return CP56Time2a{
		Millisecond: uint16(ts.Second()*1000 + ts.Nanosecond()/int(time.Millisecond)),
		Minute:      uint8(ts.Minute()),
		Hour:        uint8(ts.Hour()),
		Day:         uint8(ts.Day()),
		// Go counts Sunday as 0; the standard counts Monday as 1 and Sunday
		// as 7.
		DayOfWeek: uint8((int(ts.Weekday())+6)%7 + 1),
		Month:     uint8(ts.Month()),
		Year:      uint8(ts.Year() % 100),
		Summer:    offset != stdOffset,
	}
}

func (t CP56Time2a) String() string {
	s := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%03d",
		centuryBase+int(t.Year), t.Month, t.Day, t.Hour, t.Minute,
		t.Millisecond/1000, t.Millisecond%1000)
	if t.Summer {
		s += " SU"
	}
	if t.Invalid {
		s += " IV"
	}
	if !t.InRange() {
		s += " (out of range)"
	}
	return s
}
