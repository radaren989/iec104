package apci

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// mustHex turns a golden vector's hex text into bytes. Spaces are allowed so the
// vectors can be written the way a capture prints them.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex vector %q: %v", s, err)
	}
	return b
}

// --- golden: every format, parse and encode, byte for byte -------------------

// TestGoldenRoundTrip is the acceptance criterion for T4.1. Each vector is
// decoded and compared field by field, then re-encoded and compared byte for
// byte against the same vector. Checking only one direction would let a
// symmetric mistake — the classic off-by-two on the length field — pass.
func TestGoldenRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		wire string
		want Frame
	}{
		{
			// The handshake that has to happen before an RTU sends anything.
			name: "STARTDT act",
			wire: "68 04 07 00 00 00",
			want: NewU(StartDTAct),
		},
		{
			name: "STARTDT con",
			wire: "68 04 0B 00 00 00",
			want: NewU(StartDTCon),
		},
		{
			name: "STOPDT act",
			wire: "68 04 13 00 00 00",
			want: NewU(StopDTAct),
		},
		{
			name: "STOPDT con",
			wire: "68 04 23 00 00 00",
			want: NewU(StopDTCon),
		},
		{
			// What timer t3 puts on an idle link.
			name: "TESTFR act",
			wire: "68 04 43 00 00 00",
			want: NewU(TestFRAct),
		},
		{
			name: "TESTFR con",
			wire: "68 04 83 00 00 00",
			want: NewU(TestFRCon),
		},
		{
			// S-frame acknowledging everything below N(R)=1.
			name: "S-frame nr=1",
			wire: "68 04 01 00 02 00",
			want: NewS(1),
		},
		{
			// N(R)=128 is the first value that needs the high octet: 128<<1 =
			// 0x0100, so ctrl2=0x00 and ctrl3=0x01. A shift written as >>8
			// instead of >>7 passes every smaller case and fails here.
			name: "S-frame nr=128 crosses the octet boundary",
			wire: "68 04 01 00 00 01",
			want: NewS(128),
		},
		{
			name: "S-frame nr at the wrap boundary",
			wire: "68 04 01 00 FE FF",
			want: NewS(MaxSeq),
		},
		{
			// M_SP_NA_1 (type 1), COT=3 spontaneous, CASDU=1, IOA=1, SIQ=0x01.
			name: "I-frame ns=0 nr=0 with a single-point ASDU",
			wire: "68 0E 00 00 00 00 01 01 03 00 01 00 01 00 00 01",
			want: NewI(0, 0, []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x01}),
		},
		{
			// Both counters past the low octet at once.
			name: "I-frame ns=200 nr=300",
			wire: "68 05 90 01 58 02 07",
			want: NewI(200, 300, []byte{0x07}),
		},
		{
			name: "I-frame with both counters at the wrap boundary",
			wire: "68 05 FE FF FE FF 2A",
			want: NewI(MaxSeq, MaxSeq, []byte{0x2A}),
		},
		{
			// The largest frame the standard allows: length field 253.
			name: "I-frame with a maximum-size ASDU",
			wire: "68 FD 00 00 00 00" + strings.Repeat(" AA", MaxASDULen),
			want: NewI(0, 0, bytes.Repeat([]byte{0xAA}, MaxASDULen)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wire := mustHex(t, tc.wire)

			got, err := Decode(wire)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			assertFrame(t, got, tc.want)

			back, err := got.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.Equal(back, wire) {
				t.Errorf("re-encode mismatch\n got % X\nwant % X", back, wire)
			}

			// Encoding the hand-written expectation must produce the same wire
			// bytes, so the test cannot pass by decoding and re-encoding its own
			// mistake.
			fromWant, err := tc.want.Encode()
			if err != nil {
				t.Fatalf("Encode(want): %v", err)
			}
			if !bytes.Equal(fromWant, wire) {
				t.Errorf("expected frame encodes differently\n got % X\nwant % X", fromWant, wire)
			}
		})
	}
}

func assertFrame(t *testing.T, got, want Frame) {
	t.Helper()
	if got.Format != want.Format {
		t.Fatalf("format: got %s, want %s", got.Format, want.Format)
	}
	switch want.Format {
	case FormatI:
		if got.SendSeq != want.SendSeq {
			t.Errorf("N(S): got %d, want %d", got.SendSeq, want.SendSeq)
		}
		if got.RecvSeq != want.RecvSeq {
			t.Errorf("N(R): got %d, want %d", got.RecvSeq, want.RecvSeq)
		}
		if !bytes.Equal(got.ASDU, want.ASDU) {
			t.Errorf("ASDU:\n got % X\nwant % X", got.ASDU, want.ASDU)
		}
	case FormatS:
		if got.RecvSeq != want.RecvSeq {
			t.Errorf("N(R): got %d, want %d", got.RecvSeq, want.RecvSeq)
		}
		if got.SendSeq != 0 || len(got.ASDU) != 0 {
			t.Errorf("S-frame must leave N(S) and ASDU zero, got %+v", got)
		}
	case FormatU:
		if got.Function != want.Function {
			t.Errorf("function: got %s, want %s", got.Function, want.Function)
		}
		if got.SendSeq != 0 || got.RecvSeq != 0 || len(got.ASDU) != 0 {
			t.Errorf("U-frame must leave the numbered fields zero, got %+v", got)
		}
	}
}

// --- rejection: S8 says field input is untrusted -----------------------------

func TestDecodeRejectsMalformedFrames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		wire string
		want error
	}{
		{"empty buffer", "", ErrIncomplete},
		{"start byte only", "68", ErrIncomplete},
		{"header promises more than is there", "68 04 07 00", ErrIncomplete},

		{"wrong start byte", "69 04 07 00 00 00", ErrStartByte},
		{"start byte that looks like ASCII", "00 04 07 00 00 00", ErrStartByte},

		{"length below the control field", "68 03 07 00 00", ErrLength},
		{"length zero", "68 00", ErrLength},
		{"length above 253", "68 FE" + strings.Repeat(" 00", 254), ErrLength},
		{"trailing bytes after a complete frame", "68 04 07 00 00 00 68", ErrLength},

		{"I-frame with no ASDU", "68 04 00 00 00 00", ErrEmptyASDU},
		{"reserved bit set in N(S)", "68 05 01 00 00 00 07", ErrFormat},
		{"reserved bit set in N(R)", "68 05 00 00 01 00 07", ErrFormat},

		{"S-frame with a payload", "68 05 01 00 02 00 07", ErrFormat},
		{"S-frame with a non-zero second octet", "68 04 01 01 02 00", ErrFormat},
		{"ctrl0 that is neither I, S nor U", "68 04 05 00 00 00", ErrFormat},

		{"U-frame with no function bit", "68 04 03 00 00 00", ErrFormat},
		{"U-frame with two function bits", "68 04 0F 00 00 00", ErrFormat},
		{"U-frame with a payload", "68 05 07 00 00 00 07", ErrFormat},
		{"U-frame with a non-zero second octet", "68 04 07 01 00 00", ErrFormat},
		{"U-frame with a non-zero fourth octet", "68 04 07 00 00 01", ErrFormat},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode(mustHex(t, tc.wire))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// --- NextFrameLen -----------------------------------------------------------

func TestNextFrameLen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		wire string
		want int
		err  error
	}{
		{"one byte is not enough to tell", "68", 0, ErrIncomplete},
		{"header alone is enough to tell", "68 04", HeaderLen + 4, nil},
		{"length is read before the body arrives", "68 FD 00", HeaderLen + MaxAPDULen, nil},
		{"a longer buffer reports only this frame", "68 04 07 00 00 00 68 04 43 00 00 00", 6, nil},
		{"bad start byte is reported immediately", "69 04", 0, ErrStartByte},
		{"bad length is reported immediately", "68 02", 0, ErrLength},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := NextFrameLen(mustHex(t, tc.wire))
			if !errors.Is(err, tc.err) {
				t.Fatalf("error: got %v, want %v", err, tc.err)
			}
			if got != tc.want {
				t.Errorf("length: got %d, want %d", got, tc.want)
			}
		})
	}
}

// A stream is read one frame at a time; NextFrameLen plus Decode must walk a
// concatenation of frames without drifting.
func TestStreamWalk(t *testing.T) {
	t.Parallel()

	var stream []byte
	want := []Frame{
		NewU(StartDTAct),
		NewU(StartDTCon),
		NewI(0, 0, []byte{0x01, 0x02, 0x03}),
		NewS(1),
		NewI(1, 1, bytes.Repeat([]byte{0xFF}, MaxASDULen)),
		NewU(TestFRAct),
	}
	for _, f := range want {
		b, err := f.Encode()
		if err != nil {
			t.Fatalf("encode %s: %v", f, err)
		}
		stream = append(stream, b...)
	}

	for i, expected := range want {
		n, err := NextFrameLen(stream)
		if err != nil {
			t.Fatalf("frame %d: NextFrameLen: %v", i, err)
		}
		got, err := Decode(stream[:n])
		if err != nil {
			t.Fatalf("frame %d: Decode: %v", i, err)
		}
		assertFrame(t, got, expected)
		stream = stream[n:]
	}
	if len(stream) != 0 {
		t.Errorf("%d byte(s) left over after walking the stream", len(stream))
	}
}

// --- encoding ---------------------------------------------------------------

func TestEncodeRejectsImpossibleFrames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		frame Frame
		want  error
	}{
		{"N(S) above the 15-bit range", NewI(MaxSeq+1, 0, []byte{1}), ErrSeqRange},
		{"N(R) above the 15-bit range", NewI(0, MaxSeq+1, []byte{1}), ErrSeqRange},
		{"S-frame N(R) above the range", NewS(MaxSeq + 1), ErrSeqRange},
		{"I-frame with no ASDU", NewI(0, 0, nil), ErrEmptyASDU},
		{"ASDU one byte too long", NewI(0, 0, make([]byte, MaxASDULen+1)), ErrASDUTooLong},
		{"unknown U function", NewU(UFunction(0x04 | 0x08)), ErrFormat},
		{"zero U function", NewU(0), ErrFormat},
		{"unknown format", Frame{Format: Format(9)}, ErrFormat},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := tc.frame.Encode(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// AppendTo exists so the write loop can reuse a buffer. It must leave what is
// already in dst alone and must produce exactly what Encode produces.
func TestAppendToReusesTheBuffer(t *testing.T) {
	t.Parallel()

	prefix := []byte{0xDE, 0xAD}
	buf := make([]byte, 0, MaxFrameLen*2)
	buf = append(buf, prefix...)

	f := NewI(7, 9, []byte{0x01, 0x02})
	buf, err := f.AppendTo(buf)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	if !bytes.HasPrefix(buf, prefix) {
		t.Fatalf("AppendTo overwrote the existing content: % X", buf)
	}

	alone, err := f.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(buf[len(prefix):], alone) {
		t.Errorf("AppendTo and Encode disagree\n got % X\nwant % X", buf[len(prefix):], alone)
	}
}

// --- the ASDU copy ----------------------------------------------------------

// Decode must not hand back a window into the caller's read buffer: that buffer
// is reused on the next read, and the bug shows up as one station reporting
// another's values.
func TestDecodedASDUSurvivesBufferReuse(t *testing.T) {
	t.Parallel()

	buf := mustHex(t, "68 05 00 00 00 00 2A")
	f, err := Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	for i := range buf {
		buf[i] = 0xFF
	}

	if want := []byte{0x2A}; !bytes.Equal(f.ASDU, want) {
		t.Errorf("ASDU changed when the read buffer was reused: got % X, want % X", f.ASDU, want)
	}
}

// --- U-function helpers -----------------------------------------------------

func TestUFunctionConfirm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		act   UFunction
		con   UFunction
		isCon bool
	}{
		{StartDTAct, StartDTCon, true},
		{StopDTAct, StopDTCon, true},
		{TestFRAct, TestFRCon, true},
	}
	for _, tc := range cases {
		t.Run(tc.act.String(), func(t *testing.T) {
			t.Parallel()

			got, ok := tc.act.Confirm()
			if !ok || got != tc.con {
				t.Errorf("Confirm: got (%s, %v), want (%s, true)", got, ok, tc.con)
			}
			if !tc.act.IsAct() {
				t.Errorf("%s must report IsAct", tc.act)
			}
			if _, ok := tc.con.Confirm(); ok {
				t.Errorf("%s is already a confirmation", tc.con)
			}
			if tc.con.IsAct() {
				t.Errorf("%s must not report IsAct", tc.con)
			}
		})
	}
}

// --- fuzz: S8, no input may panic -------------------------------------------

func FuzzDecode(f *testing.F) {
	for _, seed := range []string{
		"68 04 07 00 00 00",
		"68 04 01 00 02 00",
		"68 0E 00 00 00 00 01 01 03 00 01 00 01 00 00 01",
		"68 FD 00 00 00 00",
		"",
		"68",
		"6804070000",
	} {
		b, err := hex.DecodeString(strings.ReplaceAll(seed, " ", ""))
		if err != nil {
			f.Fatalf("bad seed %q: %v", seed, err)
		}
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		frame, err := Decode(b)
		if err != nil {
			return
		}
		// Whatever decoded must re-encode to exactly the input. A decoder that
		// accepts a frame it cannot reproduce has silently reinterpreted it.
		back, err := frame.Encode()
		if err != nil {
			t.Fatalf("decoded %v but could not re-encode: %v", frame, err)
		}
		if !bytes.Equal(back, b) {
			t.Fatalf("round trip changed the bytes\n in % X\nout % X", b, back)
		}
	})
}
