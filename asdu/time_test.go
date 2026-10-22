package asdu

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// --- golden vectors ---------------------------------------------------------

// A hand-written tag: 2026-09-15 14:30:45.678, Tuesday, no flags.
//
//	ms    = 45678 = 0xB26E -> 6E B2
//	min   = 30    = 0x1E
//	hour  = 14    = 0x0E
//	day   = 15, dow = 2 (Tuesday) -> 15 | 2<<5 = 0x4F
//	month = 9     = 0x09
//	year  = 26    = 0x1A
const goldenTagHex = "6E B2 1E 0E 4F 09 1A"

func TestCP56Time2aGoldenRoundTrip(t *testing.T) {
	t.Parallel()

	wire := mustHex(t, goldenTagHex)

	got, err := ParseCP56Time2a(wire)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := CP56Time2a{
		Millisecond: 45678, Minute: 30, Hour: 14, Day: 15, DayOfWeek: 2, Month: 9, Year: 26,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if back := got.AppendTo(nil); !bytes.Equal(back, wire) {
		t.Errorf("re-encode\n got % X\nwant % X", back, wire)
	}

	instant, ok := got.Time(time.UTC)
	if !ok {
		t.Fatal("a well-formed tag must convert")
	}
	expected := time.Date(2026, 9, 15, 14, 30, 45, 678*int(time.Millisecond), time.UTC)
	if !instant.Equal(expected) {
		t.Errorf("Time = %s, want %s", instant, expected)
	}
}

// The milliseconds field carries the seconds too, little-endian like every
// multi-octet field in the standard.
func TestMillisecondsCarrySecondsAndAreLittleEndian(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		wire   string
		ms     uint16
		second int
		nanos  int
	}{
		{"zero", "00 00 00 00 01 01 00", 0, 0, 0},
		{"one millisecond", "01 00 00 00 01 01 00", 1, 0, int(time.Millisecond)},
		{"one second", "E8 03 00 00 01 01 00", 1000, 1, 0},
		{"the last representable moment of a minute", "5F EA 00 00 01 01 00", 59999, 59, 999 * int(time.Millisecond)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tag, err := ParseCP56Time2a(mustHex(t, tc.wire))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if tag.Millisecond != tc.ms {
				t.Fatalf("Millisecond = %d, want %d", tag.Millisecond, tc.ms)
			}
			instant, ok := tag.Time(time.UTC)
			if !ok {
				t.Fatal("must convert")
			}
			if instant.Second() != tc.second || instant.Nanosecond() != tc.nanos {
				t.Errorf("second/nanos = %d/%d, want %d/%d",
					instant.Second(), instant.Nanosecond(), tc.second, tc.nanos)
			}
		})
	}
}

// --- IV and SU, asserted separately -----------------------------------------

// The IV bit says the device's own clock is wrong. The value that arrived with
// it is still good, so the flag belongs on the timestamp and nowhere else.
func TestInvalidBit(t *testing.T) {
	t.Parallel()

	// The golden tag with bit 7 of the minute octet set: 0x1E | 0x80 = 0x9E.
	tag, err := ParseCP56Time2a(mustHex(t, "6E B2 9E 0E 4F 09 1A"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !tag.Invalid {
		t.Fatal("the IV bit must be read")
	}
	// The minute must not have been polluted by the flag.
	if tag.Minute != 30 {
		t.Errorf("Minute = %d, want 30 — the IV bit leaked into the field", tag.Minute)
	}
	// In range but not usable: the clock runs, it was never set.
	if !tag.InRange() {
		t.Error("the calendar fields are in range")
	}
	if tag.Usable() {
		t.Error("a tag the device marked invalid must not be usable")
	}
	// It still converts on request — the caller may want it for diagnosis.
	if _, ok := tag.Time(time.UTC); !ok {
		t.Error("an in-range tag converts even when IV is set")
	}
	if back := tag.AppendTo(nil); back[2] != 0x9E {
		t.Errorf("re-encoded minute octet = 0x%02X, want 0x9E", back[2])
	}
}

func TestSummerBit(t *testing.T) {
	t.Parallel()

	// The golden tag with bit 7 of the hour octet set: 0x0E | 0x80 = 0x8E.
	tag, err := ParseCP56Time2a(mustHex(t, "6E B2 1E 8E 4F 09 1A"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !tag.Summer {
		t.Fatal("the SU bit must be read")
	}
	if tag.Hour != 14 {
		t.Errorf("Hour = %d, want 14 — the SU bit leaked into the field", tag.Hour)
	}
	if back := tag.AppendTo(nil); back[3] != 0x8E {
		t.Errorf("re-encoded hour octet = 0x%02X, want 0x8E", back[3])
	}
}

func TestBothFlagsTogether(t *testing.T) {
	t.Parallel()

	tag, err := ParseCP56Time2a(mustHex(t, "6E B2 9E 8E 4F 09 1A"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !tag.Invalid || !tag.Summer {
		t.Fatalf("IV=%v SU=%v, want both set", tag.Invalid, tag.Summer)
	}
	if tag.Minute != 30 || tag.Hour != 14 {
		t.Errorf("fields polluted: minute=%d hour=%d", tag.Minute, tag.Hour)
	}
}

// Reserved bits must be ignored on read, not masked into a neighbouring field —
// a mistake that shows up as a clock that is right most of the time.
func TestReservedBitsAreIgnored(t *testing.T) {
	t.Parallel()

	// RES1 (minute bit 6), RES2 (hour bits 5-6), RES3 (month bits 4-7),
	// RES4 (year bit 7) all set, on top of the golden tag.
	tag, err := ParseCP56Time2a(mustHex(t, "6E B2 5E 6E 4F F9 9A"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := CP56Time2a{
		Millisecond: 45678, Minute: 30, Hour: 14, Day: 15, DayOfWeek: 2, Month: 9, Year: 26,
	}
	if tag != want {
		t.Errorf("reserved bits leaked into the fields\n got %+v\nwant %+v", tag, want)
	}
}

// --- the century rule -------------------------------------------------------

// The year is two digits and the standard leaves the century "to be known by
// other means", which in practice means assumed.
func TestTwoDigitYearReadsIntoThisCentury(t *testing.T) {
	t.Parallel()

	cases := []struct {
		field uint8
		want  int
	}{
		{0, 2000},
		{26, 2026},
		{70, 2070}, // a sliding window would call this 1970; it does not
		{99, 2099}, // and this 1999
	}

	for _, tc := range cases {
		tag := CP56Time2a{Millisecond: 0, Minute: 0, Hour: 0, Day: 1, Month: 1, Year: tc.field}
		instant, ok := tag.Time(time.UTC)
		if !ok {
			t.Fatalf("year %d: must convert", tc.field)
		}
		if instant.Year() != tc.want {
			t.Errorf("year field %d -> %d, want %d", tc.field, instant.Year(), tc.want)
		}
	}
}

// --- a broken clock must not cost the measurement ---------------------------

// A device with an unset clock sends month 0 or day 0 quite happily. Dropping
// the measurement that came with it would lose good data over bad metadata, so
// parsing succeeds and the tag reports itself unusable.
func TestOutOfRangeFieldsAreNotAParseError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		wire string
	}{
		{"month zero", "00 00 00 00 01 00 1A"},
		{"day zero", "00 00 00 00 00 01 1A"},
		{"month thirteen", "00 00 00 00 01 0D 1A"},
		{"minute sixty", "00 00 3C 00 01 01 1A"},
		{"hour twenty-four", "00 00 00 18 01 01 1A"},
		{"milliseconds past a minute", "60 EA 00 00 01 01 1A"},
		{"an all-zero tag, which an unset clock sends", "00 00 00 00 00 00 00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tag, err := ParseCP56Time2a(mustHex(t, tc.wire))
			if err != nil {
				t.Fatalf("out-of-range fields must not fail the parse: %v", err)
			}
			if tag.InRange() {
				t.Error("InRange must be false")
			}
			if tag.Usable() {
				t.Error("Usable must be false")
			}
			if _, ok := tag.Time(time.UTC); ok {
				t.Error("Time must refuse to invent an instant")
			}
		})
	}
}

func TestParseRejectsShortBuffer(t *testing.T) {
	t.Parallel()

	for n := range CP56Time2aLen {
		if _, err := ParseCP56Time2a(make([]byte, n)); !errors.Is(err, ErrTruncated) {
			t.Errorf("%d octet(s): got %v, want ErrTruncated", n, err)
		}
	}
	if _, err := ParseCP56Time2a(make([]byte, CP56Time2aLen)); err != nil {
		t.Errorf("exactly %d octets must parse: %v", CP56Time2aLen, err)
	}
}

// --- building a tag ---------------------------------------------------------

func TestCP56Time2aFromRoundTrips(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, 9, 15, 14, 30, 45, 678*int(time.Millisecond), time.UTC)
	tag := CP56Time2aFrom(instant)

	if got := tag.AppendTo(nil); !bytes.Equal(got, mustHex(t, goldenTagHex)) {
		t.Errorf("encoded\n got % X\nwant %s", got, goldenTagHex)
	}
	back, ok := tag.Time(time.UTC)
	if !ok {
		t.Fatal("must convert back")
	}
	if !back.Equal(instant) {
		t.Errorf("round trip: %s -> %s", instant, back)
	}
}

// Go counts Sunday as 0; the standard counts Monday as 1 and Sunday as 7.
func TestDayOfWeekFollowsTheStandardNotGo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		date time.Time
		want uint8
	}{
		{time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), 1}, // Monday
		{time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), 2},
		{time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC), 6}, // Saturday
		{time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 7}, // Sunday, not 0
	}
	for _, tc := range cases {
		if got := CP56Time2aFrom(tc.date).DayOfWeek; got != tc.want {
			t.Errorf("%s: DayOfWeek = %d, want %d", tc.date.Weekday(), got, tc.want)
		}
	}
}

// The tag carries local time with no offset, so the location is an input. This
// is also the open question in the contract: the gateway has to be told what
// zone a station's clock is in.
func TestTimeUsesTheSuppliedLocation(t *testing.T) {
	t.Parallel()

	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Skipf("no zone database in this environment: %v", err)
	}

	tag := CP56Time2a{Millisecond: 0, Minute: 0, Hour: 12, Day: 15, Month: 9, Year: 26}

	utc, _ := tag.Time(time.UTC)
	local, _ := tag.Time(istanbul)
	if utc.Equal(local) {
		t.Fatal("the same fields in two zones must be two different instants")
	}
	if diff := utc.Sub(local); diff != 3*time.Hour {
		t.Errorf("difference = %s, want 3h", diff)
	}
}

// --- CP24Time2a is forbidden ------------------------------------------------

// The eight short-time-tag types belong to the serial profile. Accepting them
// "leniently" would produce timestamps that look fine and are wrong by up to an
// hour, because CP24Time2a has no hour and no date.
func TestCP24TypesAreRefused(t *testing.T) {
	t.Parallel()

	for _, id := range []TypeID{2, 4, 6, 8, 10, 12, 14, 16} {
		if !id.ForbiddenOn104() {
			t.Errorf("type %d carries a CP24Time2a and must be forbidden", id)
		}
		if err := id.CheckPermitted(); !errors.Is(err, ErrForbiddenType) {
			t.Errorf("type %d: got %v, want ErrForbiddenType", id, err)
		}
	}
}

// The types on either side of the forbidden ones must stay permitted; an
// off-by-one in the ban would silently drop the most common frames on the link.
func TestPermittedTypesAreNotCaughtByTheBan(t *testing.T) {
	t.Parallel()

	permitted := []TypeID{
		MSpNa1, MDpNa1, MStNa1, MBoNa1, MMeNa1, MMeNb1, MMeNc1, MItNa1,
		MSpTb1, MDpTb1, MStTb1, MMeTd1, MMeTe1, MMeTf1, MItTb1,
		CScNa1, CDcNa1, CIcNa1, CCsNa1,
		// 17, 18, 19 are protection-equipment events with CP24-era tags in the
		// 101 profile but are not in the forbidden list; the ban covers exactly
		// the eight the standard names.
		17, 18, 19,
	}
	for _, id := range permitted {
		if id.ForbiddenOn104() {
			t.Errorf("%s must not be caught by the CP24 ban", id)
		}
		if err := id.CheckPermitted(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

// The reject reasons are metric label values. Renaming one silently breaks every
// dashboard built on it, so the strings are pinned here.
func TestRejectReasonsArePinned(t *testing.T) {
	t.Parallel()

	cases := map[RejectReason]string{
		ReasonCP24Time:             "cp24_time_forbidden",
		ReasonUnknownType:          "unknown_type",
		ReasonMalformed:            "malformed",
		ReasonUnknownCommonAddress: "unknown_common_address",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("reject reason changed: %q, want %q", got, want)
		}
	}
}

// A nil location must not panic. time.Date does, and this function sits on the
// read path: a station whose configured zone failed to load, and whose nil
// Location was carried along instead of checked, would take down the read loop
// and with it every other station on the gateway.
//
// Found by the real-capture replay in test/capture, which called Time(nil) and
// panicked. The hand-written tests had all passed time.UTC, and the fuzzer only
// reached the nil path through out-of-range tags, which return before the
// conversion — so nothing here would ever have caught it.
func TestTimeToleratesANilLocation(t *testing.T) {
	t.Parallel()

	tag := CP56Time2a{Millisecond: 45678, Minute: 30, Hour: 14, Day: 15, Month: 9, Year: 26}

	got, ok := tag.Time(nil)
	if !ok {
		t.Fatal("an in-range tag must convert")
	}
	if want, _ := tag.Time(time.UTC); !got.Equal(want) {
		t.Errorf("a nil location must behave as UTC: got %s, want %s", got, want)
	}

	// And the out-of-range path must still refuse rather than panic.
	if _, ok := (CP56Time2a{}).Time(nil); ok {
		t.Error("an out-of-range tag must not convert, nil location or not")
	}
}
