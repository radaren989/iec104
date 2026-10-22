package asdu

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

// --- the general interrogation sequence -------------------------------------

// The acceptance criterion for T5.6: the act / actcon / actterm triple, byte for
// byte. This bracket is the cold-start gate — until the termination arrives the
// cache is incomplete — so the three frames have to be told apart reliably.
func TestInterrogationTriple(t *testing.T) {
	t.Parallel()

	p := DefaultParams() // casdu 2, cot 2, ioa 3

	cases := []struct {
		name  string
		cause Cause
		wire  string
	}{
		// 0x64 = 100 = C_IC_NA_1, one object, COT, OA=0, CASDU=1, IOA=0, QOI=20.
		{"activation", CauseActivation, "64 01 06 00 01 00 00 00 00 14"},
		{"activation confirmation", CauseActCon, "64 01 07 00 01 00 00 00 00 14"},
		{"activation termination", CauseActTerm, "64 01 0A 00 01 00 00 00 00 14"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := p.AppendInterrogation(nil, 1, 0, tc.cause, QOIStation)
			if err != nil {
				t.Fatalf("AppendInterrogation: %v", err)
			}
			if want := mustHex(t, tc.wire); !bytes.Equal(got, want) {
				t.Fatalf("\n got % X\nwant % X", got, want)
			}

			// And it must read back, because the confirmation and termination
			// arrive from the station, not from us.
			h, body, err := p.ParseHeader(got)
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.Cause != tc.cause {
				t.Errorf("cause = %s, want %s", h.Cause, tc.cause)
			}
			qoi, err := p.ParseInterrogation(h, body)
			if err != nil {
				t.Fatalf("ParseInterrogation: %v", err)
			}
			if qoi != QOIStation {
				t.Errorf("qoi = %s, want station", qoi)
			}
		})
	}
}

// The station echoes the qualifier, which is how a master with several
// interrogations in flight tells their answers apart.
func TestQOIEncoding(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	if got := byte(QOIStation); got != 20 {
		t.Errorf("station qualifier = %d, want 20", got)
	}

	for n := 1; n <= 16; n++ {
		qoi, err := Group(n)
		if err != nil {
			t.Fatalf("Group(%d): %v", n, err)
		}
		if want := QOI(20 + n); qoi != want {
			t.Errorf("group %d = %d, want %d", n, uint8(qoi), uint8(want))
		}
		if !qoi.Valid() {
			t.Errorf("group %d must be valid", n)
		}
		if got := qoi.String(); got != fmt.Sprintf("group-%d", n) {
			t.Errorf("group %d prints %q", n, got)
		}

		wire, err := p.AppendInterrogation(nil, 1, 0, CauseActivation, qoi)
		if err != nil {
			t.Fatalf("group %d: %v", n, err)
		}
		if got := wire[len(wire)-1]; got != byte(qoi) {
			t.Errorf("group %d: last octet = 0x%02X, want 0x%02X", n, got, byte(qoi))
		}
	}

	for _, n := range []int{0, -1, 17, 100} {
		if _, err := Group(n); !errors.Is(err, ErrSystem) {
			t.Errorf("Group(%d) must be refused, got %v", n, err)
		}
	}
}

func TestInterrogationRejectsUndefinedQualifiers(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	for _, qoi := range []QOI{0, 1, 19, 37, 255} {
		if qoi.Valid() {
			t.Errorf("qoi %d must not be valid", uint8(qoi))
		}
		out, err := p.AppendInterrogation(nil, 1, 0, CauseActivation, qoi)
		if !errors.Is(err, ErrSystem) {
			t.Errorf("qoi %d: got %v, want ErrSystem", uint8(qoi), err)
		}
		if len(out) != 0 {
			t.Errorf("qoi %d: %d octet(s) written on a refused frame", uint8(qoi), len(out))
		}
	}
}

// --- counter interrogation --------------------------------------------------

func TestCounterInterrogation(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	cases := []struct {
		name string
		req  CounterRequest
		frz  FreezeAction
		qcc  byte
	}{
		// FRZ sits in the top two bits, RQT in the low six.
		{"read every counter without freezing", CounterGeneral, FreezeRead, 0x05},
		{"freeze every counter, no reset", CounterGeneral, FreezeNoReset, 0x45},
		{"freeze every counter with reset", CounterGeneral, FreezeWithReset, 0x85},
		{"reset every counter", CounterGeneral, FreezeReset, 0xC5},
		{"freeze group 1", CounterGroup1, FreezeNoReset, 0x41},
		{"freeze group 4", CounterGroup4, FreezeNoReset, 0x44},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := p.AppendCounterInterrogation(nil, 1, 0, CauseActivation, tc.req, tc.frz)
			if err != nil {
				t.Fatalf("AppendCounterInterrogation: %v", err)
			}
			// 0x65 = 101 = C_CI_NA_1.
			want := mustHex(t, "65 01 06 00 01 00 00 00 00 "+fmt.Sprintf("%02X", tc.qcc))
			if !bytes.Equal(got, want) {
				t.Fatalf("\n got % X\nwant % X", got, want)
			}

			h, body, err := p.ParseHeader(got)
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			req, frz, err := p.ParseCounterInterrogation(h, body)
			if err != nil {
				t.Fatalf("ParseCounterInterrogation: %v", err)
			}
			if req != tc.req || frz != tc.frz {
				t.Errorf("round trip: req=%d frz=%d, want %d/%d", req, frz, tc.req, tc.frz)
			}
		})
	}
}

func TestCounterInterrogationRejectsBadQualifiers(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	cases := []struct {
		req CounterRequest
		frz FreezeAction
	}{
		{0, FreezeNoReset},  // RQT 0 is not a group
		{6, FreezeNoReset},  // 6..31 are reserved
		{63, FreezeNoReset}, // the top of the field
		{CounterGeneral, 4}, // FRZ is two bits
	}
	for _, tc := range cases {
		out, err := p.AppendCounterInterrogation(nil, 1, 0, CauseActivation, tc.req, tc.frz)
		if !errors.Is(err, ErrSystem) {
			t.Errorf("req=%d frz=%d: got %v, want ErrSystem", tc.req, tc.frz, err)
		}
		if len(out) != 0 {
			t.Errorf("req=%d frz=%d: %d octet(s) written", tc.req, tc.frz, len(out))
		}
	}
}

// --- clock synchronisation --------------------------------------------------

func TestClockSyncGolden(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	tag := theGoldenCP56(t)

	got, err := p.AppendClockSync(nil, 1, 0, CauseActivation, tag)
	if err != nil {
		t.Fatalf("AppendClockSync: %v", err)
	}
	// 0x67 = 103 = C_CS_NA_1.
	want := mustHex(t, "67 01 06 00 01 00 00 00 00 "+theGoldenTag)
	if !bytes.Equal(got, want) {
		t.Fatalf("\n got % X\nwant % X", got, want)
	}

	h, body, err := p.ParseHeader(got)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	back, err := p.ParseClockSync(h, body)
	if err != nil {
		t.Fatalf("ParseClockSync: %v", err)
	}
	instant, ok := back.Time(time.UTC)
	if !ok {
		t.Fatal("the round-tripped tag must convert")
	}
	if want := time.Date(2026, 9, 15, 14, 30, 45, 678*int(time.Millisecond), time.UTC); !instant.Equal(want) {
		t.Errorf("timestamp = %s, want %s", instant, want)
	}
}

// Setting a clock to a time we have ourselves marked invalid is not a
// synchronisation, it is corruption with extra steps.
func TestClockSyncRefusesUnusableTimes(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	invalid := theGoldenCP56(t)
	invalid.Invalid = true

	for _, tc := range []struct {
		name string
		tag  CP56Time2a
	}{
		{"a tag marked invalid", invalid},
		{"a tag that is not a date", CP56Time2a{}},
		{"month thirteen", CP56Time2a{Day: 1, Month: 13}},
	} {
		out, err := p.AppendClockSync(nil, 1, 0, CauseActivation, tc.tag)
		if !errors.Is(err, ErrSystem) {
			t.Errorf("%s: got %v, want ErrSystem", tc.name, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: %d octet(s) written", tc.name, len(out))
		}
	}
}

// --- reset process ----------------------------------------------------------

func TestResetProcess(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	for _, tc := range []struct {
		name string
		qrp  ResetQualifier
		oct  string
	}{
		{"general reset", ResetGeneral, "01"},
		{"reset pending events", ResetPendingEvents, "02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := p.AppendResetProcess(nil, 1, 0, CauseActivation, tc.qrp)
			if err != nil {
				t.Fatalf("AppendResetProcess: %v", err)
			}
			// 0x69 = 105 = C_RP_NA_1.
			if want := mustHex(t, "69 01 06 00 01 00 00 00 00 "+tc.oct); !bytes.Equal(got, want) {
				t.Errorf("\n got % X\nwant % X", got, want)
			}
		})
	}

	for _, qrp := range []ResetQualifier{0, 3, 255} {
		out, err := p.AppendResetProcess(nil, 1, 0, CauseActivation, qrp)
		if !errors.Is(err, ErrSystem) {
			t.Errorf("qrp %d: got %v, want ErrSystem", uint8(qrp), err)
		}
		if len(out) != 0 {
			t.Errorf("qrp %d: %d octet(s) written", uint8(qrp), len(out))
		}
	}
}

// --- end of initialisation --------------------------------------------------

// Every M_EI means the cached snapshot is void: the station restarted, so its
// process image was rebuilt. The parameter-change flag is worth logging
// differently but changes nothing about that.
func TestEndOfInit(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	cases := []struct {
		name  string
		coi   string
		want  EndOfInit
		print string
	}{
		{"local power on", "00", EndOfInit{Cause: InitLocalPowerOn}, "local-power-on"},
		{"local manual reset", "01", EndOfInit{Cause: InitLocalManual}, "local-manual-reset"},
		{"remote reset", "02", EndOfInit{Cause: InitRemoteReset}, "remote-reset"},
		{"power on after a parameter change", "80",
			EndOfInit{Cause: InitLocalPowerOn, AfterParameterChange: true},
			"local-power-on (after parameter change)"},
		{"remote reset after a parameter change", "82",
			EndOfInit{Cause: InitRemoteReset, AfterParameterChange: true},
			"remote-reset (after parameter change)"},
		// Seven bits, and a vendor may use a value this table does not list.
		// What matters is that the station initialised, and that is true
		// whatever the reason.
		{"an unlisted cause", "7F", EndOfInit{Cause: InitCause(0x7F)}, "InitCause(127)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// 0x46 = 70 = M_EI_NA_1, COT=4 initialised.
			wire := mustHex(t, "46 01 04 00 01 00 00 00 00 "+tc.coi)
			h, body, err := p.ParseHeader(wire)
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.Cause != CauseInitialised {
				t.Errorf("cause = %s, want initialised", h.Cause)
			}

			got, err := p.ParseEndOfInit(h, body)
			if err != nil {
				t.Fatalf("ParseEndOfInit: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got.String() != tc.print {
				t.Errorf("String = %q, want %q", got.String(), tc.print)
			}
		})
	}
}

// --- the shape every station message must have ------------------------------

// These messages address the station, not a point, and 101/104 spells that as
// address 0. A station message at a point address is a device doing something
// the standard does not describe.
func TestSystemMessagesMustAddressTheStation(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	// C_IC_NA_1 at IOA 5 instead of 0.
	wire := mustHex(t, "64 01 07 00 01 00 05 00 00 14")
	h, body, err := p.ParseHeader(wire)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if _, err := p.ParseInterrogation(h, body); !errors.Is(err, ErrSystem) {
		t.Fatalf("got %v, want ErrSystem", err)
	}
}

func TestSystemMessagesCarryExactlyOneObject(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	cases := []struct {
		name string
		wire string
	}{
		{"two objects", "64 02 07 00 01 00 00 00 00 14 00 00 00 14"},
		{"a sequence", "64 81 07 00 01 00 00 00 00 14"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, body, err := p.ParseHeader(mustHex(t, tc.wire))
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if _, err := p.ParseInterrogation(h, body); !errors.Is(err, ErrSystem) {
				t.Fatalf("got %v, want ErrSystem", err)
			}
		})
	}
}

// Asking one parser for another parser's type must fail loudly rather than
// decoding whatever happens to be there.
func TestSystemParsersCheckTheType(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	wire := mustHex(t, "64 01 07 00 01 00 00 00 00 14") // C_IC_NA_1
	h, body, err := p.ParseHeader(wire)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}

	if _, _, err := p.ParseCounterInterrogation(h, body); !errors.Is(err, ErrSystem) {
		t.Errorf("counter parser accepted a C_IC: %v", err)
	}
	if _, err := p.ParseEndOfInit(h, body); !errors.Is(err, ErrSystem) {
		t.Errorf("end-of-init parser accepted a C_IC: %v", err)
	}
	if _, err := p.ParseClockSync(h, body); err == nil {
		t.Error("clock parser accepted a C_IC")
	}
}

// --- narrower widths --------------------------------------------------------

func TestSystemMessagesAcrossWidths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		p    Params
		wire string
	}{
		{Params{CASDUOctets: 1, COTOctets: 1, IOAOctets: 1}, "64 01 06 01 00 14"},
		{Params{CASDUOctets: 1, COTOctets: 2, IOAOctets: 2}, "64 01 06 00 01 00 00 14"},
		{Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: 3}, "64 01 06 00 01 00 00 00 00 14"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("casdu=%d cot=%d ioa=%d", tc.p.CASDUOctets, tc.p.COTOctets, tc.p.IOAOctets), func(t *testing.T) {
			t.Parallel()

			got, err := tc.p.AppendInterrogation(nil, 1, 0, CauseActivation, QOIStation)
			if err != nil {
				t.Fatalf("AppendInterrogation: %v", err)
			}
			if want := mustHex(t, tc.wire); !bytes.Equal(got, want) {
				t.Errorf("\n got % X\nwant % X", got, want)
			}
		})
	}
}
