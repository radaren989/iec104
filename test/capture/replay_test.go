package capture

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/asdu"
)

var update = flag.Bool("update", false, "rewrite the golden summary from the capture")

// captureFile is one capture in the corpus.
type captureFile struct {
	Name   string
	Path   string
	Golden string

	// Why it is here, in one line. Two captures of the same thing would be
	// weight without coverage; each entry should be able to say what it adds.
	Adds string
}

// captures is the corpus. Adding a capture from another vendor's device is the
// single most valuable contribution anyone can make to internal/apci and
// internal/asdu — see test/golden/README.md.
var captures = []captureFile{
	{
		Name:   "rtu",
		Path:   "../pcap/iec104.pcap",
		Golden: "../golden/capture-summary.txt",
		Adds: "a real RTU: cold start, a general interrogation over every measurand " +
			"type, and a full round of commands with their confirmations and terminations",
	},
	{
		Name:   "sim",
		Path:   "../pcap/sim-cap.pcapng",
		Golden: "../golden/sim-capture-summary.txt",
		Adds: "a simulator: clock synchronisation and counter interrogation, which the " +
			"RTU capture never exercises. Its type coverage is now a subset of sim2's, " +
			"but it is kept for its shape: several TCP sessions in one file, including " +
			"one that is not 104 at all, which is what the connection handling in walk " +
			"exists to survive",
	},
	{
		Name:   "sim2",
		Path:   "../pcap/cap2.pcapng",
		Golden: "../golden/cap2-capture-summary.txt",
		Adds: "the same simulator with counters configured: M_IT_NA_1 and M_ME_ND_1, " +
			"the normalised type that carries no quality descriptor at all",
	},
}

// captureParams are the field widths both captures use. They are asserted
// rather than guessed: if a future capture uses different widths the summary
// diff makes that obvious instead of producing plausible nonsense.
var captureParams = asdu.Params{CASDUOctets: 2, COTOctets: 2, IOAOctets: 3}

// eachCapture runs fn against every capture in the corpus, as its own subtest.
func eachCapture(t *testing.T, fn func(t *testing.T, c captureFile, frames []frame)) {
	t.Helper()
	for _, c := range captures {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			fn(t, c, walk(t, readCapture(t, c.Path)))
		})
	}
}

// rtuCapture is the capture from a real device, for the assertions that pin
// specific values a device reported.
func rtuCapture() captureFile { return captures[0] }

// frame is one APDU pulled out of the capture, with its direction.
type frame struct {
	Dir   Direction
	Conn  uint16
	Raw   []byte
	Frame apci.Frame
}

// stream identifies one direction of one TCP connection.
type stream struct {
	Conn uint16
	Dir  Direction
}

// walk reassembles each stream's bytes and cuts them into frames.
//
// Each direction of each connection is buffered separately. Interleaving the
// two directions would splice one side's frame into the other's; merging two
// connections would do the same across sessions. Both mistakes look exactly
// like a framing bug in apci, which is the last place anyone would look.
func walk(t *testing.T, segs []segment) []frame {
	t.Helper()

	buf := map[stream][]byte{}
	// A connection whose very first octet is not 0x68 never carried 104 at all —
	// a port scan, or another protocol on the same port. It is skipped with a
	// note. A desynchronisation AFTER at least one good frame is a different
	// thing entirely and still fails: that is a real framing error.
	dead := map[stream]bool{}
	frames := map[stream]int{}
	var out []frame

	for _, seg := range segs {
		key := stream{Conn: seg.Conn, Dir: seg.Dir}
		if dead[key] {
			continue
		}
		buf[key] = append(buf[key], seg.Payload...)

		for {
			b := buf[key]
			n, err := apci.NextFrameLen(b)
			if err != nil {
				// Not enough bytes yet is normal: the frame spans segments.
				// Anything else means the stream has gone out of step, and
				// there is no resynchronisation in 104 — report it with the
				// context needed to find it.
				if errors.Is(err, apci.ErrIncomplete) {
					break
				}
				if frames[key] == 0 {
					t.Logf("connection %d (%s): not a 104 stream, first octets % X - skipped",
						key.Conn, key.Dir, b[:min(8, len(b))])
					dead[key] = true
					buf[key] = nil
					break
				}
				t.Fatalf("connection %d (%s): stream desynchronised after %d frame(s): %v (next octets % X)",
					key.Conn, key.Dir, frames[key], err, b[:min(16, len(b))])
			}
			if len(b) < n {
				break
			}

			raw := b[:n]
			f, err := apci.Decode(raw)
			if err != nil {
				t.Fatalf("connection %d (%s) frame %d: Decode(% X): %v",
					key.Conn, key.Dir, frames[key], raw, err)
			}
			out = append(out, frame{Dir: seg.Dir, Conn: seg.Conn, Raw: append([]byte(nil), raw...), Frame: f})
			frames[key]++
			buf[key] = b[n:]
		}
	}

	// Leftover octets on a stream that carried frames mean the capture was
	// stopped mid-frame. That is normal for a capture someone ended by hand, so
	// it is reported rather than failed - but reported, because the same
	// symptom with a large remainder would mean a length-field bug.
	for key, left := range buf {
		if len(left) != 0 {
			t.Logf("connection %d (%s): %d octet(s) after the last complete frame: % X",
				key.Conn, key.Dir, len(left), left)
		}
	}
	return out
}

// TestCaptureFramesRoundTrip is the strongest check in the repository: every
// frame a real device sent must re-encode to exactly the octets it sent.
//
// Every other golden vector here was written by hand from the standard, which
// proves the implementation agrees with the specification. This proves it agrees
// with a device — and interoperability bugs live precisely where those two
// disagree.
func TestCaptureFramesRoundTrip(t *testing.T) {
	t.Parallel()

	eachCapture(t, func(t *testing.T, c captureFile, frames []frame) {
		if len(frames) == 0 {
			t.Fatal("no frames in the capture")
		}

		for i, f := range frames {
			back, err := f.Frame.Encode()
			if err != nil {
				t.Fatalf("frame %d (%s %s): Encode: %v", i, f.Dir, f.Frame, err)
			}
			if !bytes.Equal(back, f.Raw) {
				t.Errorf("frame %d (%s) does not round trip\n got % X\nwant % X", i, f.Dir, back, f.Raw)
			}
		}
		t.Logf("%d frames round-tripped", len(frames))
	})
}

// TestCaptureSequenceNumbers drives two Sequencers through the capture and
// checks them against what the device actually numbered.
//
// This is the test the hand-written ones cannot be: N(S)/N(R) bookkeeping is
// only correct if BOTH ends agree, and the only way to know our arithmetic
// matches a real implementation's is to replay one.
func TestCaptureSequenceNumbers(t *testing.T) {
	t.Parallel()

	eachCapture(t, func(t *testing.T, c captureFile, frames []frame) {
		// One pair of sequencers per TCP connection: a new connection restarts
		// both counters from zero, and carrying them across would reject the first
		// I-frame of the second session for no reason.
		seq := map[stream]*apci.Sequencer{}
		sequencer := func(conn uint16, dir Direction) *apci.Sequencer {
			k := stream{Conn: conn, Dir: dir}
			if seq[k] == nil {
				seq[k] = &apci.Sequencer{}
			}
			return seq[k]
		}

		for i, f := range frames {
			sender := sequencer(f.Conn, f.Dir)
			receiver := sequencer(f.Conn, f.Dir.other())

			switch f.Frame.Format {
			case apci.FormatI:
				// What the sender's counter says it would use must be what the
				// device actually put on the wire.
				ns, nr := sender.NextSend()
				if ns != f.Frame.SendSeq {
					t.Fatalf("frame %d (%s): our N(S) would be %d, the device sent %d",
						i, f.Dir, ns, f.Frame.SendSeq)
				}
				// N(R) may legitimately lag what we would have sent — a device is
				// allowed to acknowledge later than the earliest moment it could —
				// so it is not compared, only accepted.
				_ = nr

				if err := receiver.Receive(f.Frame.SendSeq, f.Frame.RecvSeq); err != nil {
					t.Fatalf("frame %d (%s): the receiving side rejected N(S)=%d N(R)=%d: %v",
						i, f.Dir, f.Frame.SendSeq, f.Frame.RecvSeq, err)
				}

			case apci.FormatS:
				sender.SendSupervisory()
				if _, err := receiver.Acknowledge(f.Frame.RecvSeq); err != nil {
					t.Fatalf("frame %d (%s): S-frame N(R)=%d rejected: %v",
						i, f.Dir, f.Frame.RecvSeq, err)
				}

			case apci.FormatU:
				// Unnumbered; nothing to check.
			}
		}

		for k, sq := range seq {
			if sq.SendSeq() != 0 {
				t.Logf("connection %d (%s) sent %d I-frame(s)", k.Conn, k.Dir, sq.SendSeq())
			}
		}
	})
}

// TestCaptureASDUsParse decodes every ASDU in the capture and asserts that
// nothing in it is unsupported or malformed.
func TestCaptureASDUsParse(t *testing.T) {
	t.Parallel()

	eachCapture(t, func(t *testing.T, c captureFile, frames []frame) {
		seen := 0

		for i, f := range frames {
			if f.Frame.Format != apci.FormatI {
				continue
			}
			seen++

			h, body, err := captureParams.ParseHeader(f.Frame.ASDU)
			if err != nil {
				t.Fatalf("frame %d (%s): ParseHeader(% X): %v", i, f.Dir, f.Frame.ASDU, err)
			}

			// The header must re-encode to its own octets. A header we accept but
			// cannot reproduce has been silently reinterpreted.
			back, err := h.AppendTo(nil, captureParams)
			if err != nil {
				t.Fatalf("frame %d: the header did not re-encode: %v", i, err)
			}
			if want := f.Frame.ASDU[:captureParams.HeaderLen()]; !bytes.Equal(back, want) {
				t.Errorf("frame %d: header round trip\n got % X\nwant % X", i, back, want)
			}

			switch {
			case h.Type.IsMonitor():
				points, err := captureParams.ParseMonitor(h, body)
				if err != nil {
					t.Errorf("frame %d: %s: %v", i, h, err)
					continue
				}
				if len(points) != int(h.Count) {
					t.Errorf("frame %d: %d points for a header promising %d", i, len(points), h.Count)
				}

			case h.Type == asdu.CIcNa1:
				if _, err := captureParams.ParseInterrogation(h, body); err != nil {
					t.Errorf("frame %d: %s: %v", i, h, err)
				}

			case h.Type == asdu.MEiNa1:
				if _, err := captureParams.ParseEndOfInit(h, body); err != nil {
					t.Errorf("frame %d: %s: %v", i, h, err)
				}

			case h.Type == asdu.CCiNa1:
				if _, _, err := captureParams.ParseCounterInterrogation(h, body); err != nil {
					t.Errorf("frame %d: %s: %v", i, h, err)
				}

			case h.Type == asdu.CCsNa1:
				if _, err := captureParams.ParseClockSync(h, body); err != nil {
					t.Errorf("frame %d: %s: %v", i, h, err)
				}

			case h.Type.IsControl():
				// Commands are encoded, not parsed, by this gateway. The capture
				// carries the station's confirmations of them, and what matters
				// here is that the object splits correctly at the configured
				// widths.
				if _, err := captureParams.SplitElements(h, body, controlElementLen(t, h.Type)); err != nil {
					t.Errorf("frame %d: %s: %v", i, h, err)
				}

			default:
				t.Errorf("frame %d: %s is neither monitor, control nor a system type we handle", i, h)
			}
		}

		if seen == 0 {
			t.Fatal("the capture contains no I-frames")
		}
		t.Logf("%d ASDUs parsed", seen)
	})
}

// controlElementLen is the element width of a control type, for splitting the
// station's confirmations. It covers only the untimestamped commands the
// capture contains.
func controlElementLen(t *testing.T, typ asdu.TypeID) int {
	t.Helper()
	switch typ {
	case asdu.CScNa1, asdu.CDcNa1, asdu.CRcNa1:
		return 1
	case asdu.CSeNa1, asdu.CSeNb1:
		return 3
	case asdu.CSeNc1:
		return 5
	case asdu.CBoNa1:
		return 4
	default:
		t.Fatalf("no element length known for %s", typ)
		return 0
	}
}

// TestCaptureSummaryMatchesGolden renders every frame in the capture as one line
// of text and compares it with a committed file.
//
// The capture is binary and unreadable in a review. This summary is the readable
// half: when a decoder changes, the diff shows exactly which frames now read
// differently, in terms an engineer can argue with. Regenerate with
//
//	go test ./test/capture -update
//
// and read the diff before committing it — that reading is the point.
func TestCaptureSummaryMatchesGolden(t *testing.T) {
	t.Parallel()

	eachCapture(t, func(t *testing.T, c captureFile, frames []frame) {
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n", filepath.Base(c.Path))
		fmt.Fprintf(&b, "# generated by go test ./test/capture -update\n")
		fmt.Fprintf(&b, "# widths: casdu=%d cot=%d ioa=%d\n#\n",
			captureParams.CASDUOctets, captureParams.COTOctets, captureParams.IOAOctets)

		for _, f := range frames {
			fmt.Fprintf(&b, "%s %s", f.Dir, f.Frame)
			if f.Frame.Format == apci.FormatI {
				describeASDU(t, &b, f.Frame.ASDU)
			}
			b.WriteByte('\n')
		}

		got := b.String()

		if *update {
			if err := os.MkdirAll(filepath.Dir(c.Golden), 0o755); err != nil {
				t.Fatalf("creating the golden directory: %v", err)
			}
			if err := os.WriteFile(c.Golden, []byte(got), 0o644); err != nil {
				t.Fatalf("writing the golden file: %v", err)
			}
			t.Logf("wrote %s (%d lines)", c.Golden, strings.Count(got, "\n"))
			return
		}

		want, err := os.ReadFile(c.Golden)
		if err != nil {
			t.Fatalf("reading the golden file (run with -update to create it): %v", err)
		}
		if got != string(want) {
			t.Errorf("the capture now decodes differently; run with -update and read the diff.\n%s",
				firstDifference(string(want), got))
		}
	})
}

// describeASDU appends a human-readable rendering of one ASDU.
func describeASDU(t *testing.T, b *strings.Builder, raw []byte) {
	t.Helper()

	h, body, err := captureParams.ParseHeader(raw)
	if err != nil {
		fmt.Fprintf(b, "  <unparsable: %v>", err)
		return
	}
	fmt.Fprintf(b, "  %s", h)

	switch {
	case h.Type.IsMonitor():
		points, err := captureParams.ParseMonitor(h, body)
		if err != nil {
			fmt.Fprintf(b, "  <%v>", err)
			return
		}
		for _, p := range points {
			// M_ME_ND_1 carries no quality octet at all, and the zero Quality
			// prints as "good". Rendering both the same way would make the
			// summary say the device reported clean quality when it reported
			// none — exactly the distinction this type exists to test.
			quality := p.Q.String()
			if p.Type == asdu.MMeNd1 {
				quality = "(absent)"
			}
			fmt.Fprintf(b, "\n    ioa=%-7d %s q=%s", p.IOA, describeValue(p), quality)
			if p.HasTime {
				if ts, ok := p.Time.Time(time.UTC); ok {
					fmt.Fprintf(b, " ts=%s", ts.Format("2006-01-02T15:04:05.000"))
				} else {
					fmt.Fprintf(b, " ts=<%s>", p.Time)
				}
				if p.Time.Invalid {
					b.WriteString(" ts-invalid")
				}
			}
		}

	case h.Type == asdu.CIcNa1:
		if qoi, err := captureParams.ParseInterrogation(h, body); err == nil {
			fmt.Fprintf(b, "  qoi=%s", qoi)
		}

	case h.Type == asdu.MEiNa1:
		if init, err := captureParams.ParseEndOfInit(h, body); err == nil {
			fmt.Fprintf(b, "  init=%s", init)
		}

	case h.Type == asdu.CCiNa1:
		if req, frz, err := captureParams.ParseCounterInterrogation(h, body); err == nil {
			fmt.Fprintf(b, "  rqt=%d frz=%d", req, frz)
		}

	case h.Type == asdu.CCsNa1:
		if tag, err := captureParams.ParseClockSync(h, body); err == nil {
			fmt.Fprintf(b, "  clock=%s", tag)
		}

	case h.Type.IsControl():
		elems, err := captureParams.SplitElements(h, body, controlElementLen(t, h.Type))
		if err == nil {
			for _, e := range elems {
				fmt.Fprintf(b, "\n    ioa=%-7d elem=% X", e.IOA, e.Data)
			}
		}
	}
}

func describeValue(p asdu.Point) string {
	switch p.Kind {
	case asdu.KindBool:
		if p.Type == asdu.MDpNa1 || p.Type == asdu.MDpTb1 {
			return fmt.Sprintf("dp=%s", p.DoubleState)
		}
		return fmt.Sprintf("bool=%v", p.Bool)
	case asdu.KindFloat:
		return fmt.Sprintf("num=%g", p.Float)
	default:
		s := fmt.Sprintf("int=%d", p.Int)
		if p.Transient {
			s += " transient"
		}
		if p.Type == asdu.MItNa1 || p.Type == asdu.MItTb1 {
			s += fmt.Sprintf(" seq=%d", p.CounterSeq)
		}
		return s
	}
}

// firstDifference reports the first line that differs, which is far easier to
// read than a whole-file diff in test output.
func firstDifference(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("first difference at line %d:\n  want: %s\n   got: %s", i+1, w, g)
		}
	}
	return "(files differ only in trailing content)"
}
