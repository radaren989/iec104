package command

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
)

// The acceptance criterion for this task names four device behaviours, and
// every one of them has been seen on real equipment: a device that answers
// properly, a device that refuses, a device that confirms and never terminates,
// and a device that says nothing at all. The last two are the interesting ones,
// because they are the cases where "it worked" and "it did not" are both wrong
// answers.

// --- harness ------------------------------------------------------------------

// wire records what was sent and lets a test fail the send.
type wire struct {
	mu   sync.Mutex
	sent [][]byte
	err  error
}

func (w *wire) send(_ context.Context, raw []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.sent = append(w.sent, append([]byte(nil), raw...))
	return nil
}

func (w *wire) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sent)
}

// rig is an operator with a fake clock and a reply channel a test drives.
type rig struct {
	op      *Operator
	wire    *wire
	fake    *clock.Fake
	replies chan Reply
}

func newRig(t *testing.T, tune func(*OperatorConfig)) *rig {
	t.Helper()

	r := &rig{
		wire:    &wire{},
		fake:    clock.NewFake(time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)),
		replies: make(chan Reply, 8),
	}

	cfg := OperatorConfig{
		Params:         asdu.DefaultParams(),
		CommonAddress:  1,
		Clock:          r.fake,
		Replies:        r.replies,
		ActConTimeout:  10 * time.Second,
		ActTermTimeout: 60 * time.Second,
		WaitForActTerm: true,
	}
	if tune != nil {
		tune(&cfg)
	}

	op, err := NewOperator(cfg)
	if err != nil {
		t.Fatalf("NewOperator: %v", err)
	}
	r.op = op
	return r
}

// start runs a command in the background and hands back its result channel.
func (r *rig) start(ctx context.Context, cmd asdu.Command) <-chan Result {
	out := make(chan Result, 1)
	go func() {
		out <- r.op.Run(ctx, r.wire.send, Request{Cmd: cmd, Label: "test command"})
	}()
	return out
}

// armed waits until the dialogue has sent its frame and armed a timer, so that
// advancing the fake clock cannot land before the timer it is meant to expire.
func (r *rig) armed(t *testing.T, n int) {
	t.Helper()
	r.fake.BlockUntilAtLeast(n)
}

func (r *rig) reply(cause asdu.Cause, negative bool, cmd asdu.Command) {
	r.replies <- Reply{Type: cmd.Type, IOA: cmd.IOA, Cause: cause, Negative: negative}
}

func result(t *testing.T, ch <-chan Result) Result {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	select {
	case res := <-ch:
		return res
	case <-timeout.C:
		t.Fatal("the dialogue never finished")
		return Result{}
	}
}

// testCmd is one single command, the simplest thing that drives an output.
func testCmd() asdu.Command {
	return asdu.Command{Type: asdu.CScNa1, IOA: 4711, On: true}
}

// --- the four device behaviours -------------------------------------------------

// One: the device does what the standard says.
func TestAConfirmedAndTerminatedCommandSucceeds(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.start(t.Context(), cmd)

	r.armed(t, 1)
	r.reply(asdu.CauseActCon, false, cmd)
	r.reply(asdu.CauseActTerm, false, cmd)

	res := result(t, done)
	if res.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome %s (%s): %v", res.Outcome, res.Code, res.Err)
	}
	if res.SentAt.IsZero() || res.ConfirmedAt.IsZero() {
		t.Error("a completed command left no timestamps; an Indeterminate one later " +
			"is investigated against the device's own log using exactly these")
	}
	if res.LastCause != asdu.CauseActTerm {
		t.Errorf("last cause %s, want %s", res.LastCause, asdu.CauseActTerm)
	}
}

// Two: the device understood and refused. The output did not move and it said
// so, which is the one unhappy ending that is safe to correct and send again.
func TestANegativeConfirmationIsRejectedAndSafeToRetry(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.start(t.Context(), cmd)

	r.armed(t, 1)
	r.reply(asdu.CauseActCon, true, cmd) // P/N set

	res := result(t, done)
	if res.Outcome != OutcomeRejected {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeRejected, res.Err)
	}
	if !res.Negative {
		t.Error("the P/N bit was not carried into the result; an operator needs to know " +
			"the device refused rather than that something timed out")
	}
	if !res.Outcome.Safe() {
		t.Error("a refused command is not retryable; the device said the output did not move")
	}
}

// Three: the device confirms and never terminates, and nobody told us.
//
// This is the case that punishes optimism in both directions. Reporting success
// would claim an operation finished that we have no evidence finished; reporting
// failure would invite a retry, and a retry is a second operation on a breaker
// the device already accepted a command for.
func TestAConfirmedCommandWithNoTerminationIsIndeterminate(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.start(t.Context(), cmd)

	r.armed(t, 1)
	r.reply(asdu.CauseActCon, false, cmd)

	// The termination timer is armed only after the confirmation is read.
	r.armed(t, 2)
	r.fake.Advance(61 * time.Second)

	res := result(t, done)
	if res.Outcome != OutcomeIndeterminate {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeIndeterminate, res.Err)
	}
	if res.Code != ErrCodeActTermTimeout {
		t.Errorf("code %q, want %q", res.Code, ErrCodeActTermTimeout)
	}
	if res.Outcome.Safe() {
		t.Error("an unterminated command must never be marked safe to retry: the device " +
			"accepted it, so sending it again is a second operation")
	}
	if !errors.Is(res.Err, ErrNoAnswer) {
		t.Errorf("error %v does not wrap ErrNoAnswer", res.Err)
	}
}

// Three, corrected: the same device, once someone has told us it never
// terminates. Study Reference §8 is blunt that these exist, and against one of
// them the flag is the difference between every command reporting success and
// every command reporting "we do not know".
func TestADeviceKnownNotToTerminateSucceedsOnTheConfirmation(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(cfg *OperatorConfig) { cfg.WaitForActTerm = false })
	cmd := testCmd()
	done := r.start(t.Context(), cmd)

	r.armed(t, 1)
	r.reply(asdu.CauseActCon, false, cmd)

	res := result(t, done)
	if res.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome %s (%s): %v", res.Outcome, res.Code, res.Err)
	}
}

// Four: silence. The command is on the wire and may have driven the output.
func TestAnUnansweredCommandIsIndeterminateNotFailed(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	done := r.start(t.Context(), testCmd())

	r.armed(t, 1)
	r.fake.Advance(11 * time.Second)

	res := result(t, done)
	if res.Outcome != OutcomeIndeterminate {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeIndeterminate, res.Err)
	}
	if res.Code != ErrCodeActConTimeout {
		t.Errorf("code %q, want %q", res.Code, ErrCodeActConTimeout)
	}
	if res.SentAt.IsZero() {
		t.Error("no send timestamp on a command that reached the wire; this is the one " +
			"outcome where knowing exactly when it went out matters most")
	}
}

// --- refusal by cause ------------------------------------------------------------

// Causes 44 to 47 are the device saying it did not understand the request. They
// are refusals, not transport failures.
func TestTheErrorCausesAreRefusals(t *testing.T) {
	t.Parallel()

	for _, cause := range []asdu.Cause{
		asdu.CauseUnknownType,
		asdu.CauseUnknownCause,
		asdu.CauseUnknownCommonAd,
		asdu.CauseUnknownIOA,
	} {
		t.Run(cause.String(), func(t *testing.T) {
			t.Parallel()

			r := newRig(t, nil)
			cmd := testCmd()
			done := r.start(t.Context(), cmd)

			r.armed(t, 1)
			r.reply(cause, false, cmd)

			res := result(t, done)
			if res.Outcome != OutcomeRejected {
				t.Errorf("outcome %s, want %s: %v", res.Outcome, OutcomeRejected, res.Err)
			}
			if res.LastCause != cause {
				t.Errorf("last cause %s, want %s", res.LastCause, cause)
			}
			if !res.Outcome.Safe() {
				t.Error("the device did not understand the request; correcting and " +
					"resending it is exactly what should happen next")
			}
		})
	}
}

// A device may accept a command and then report that it did not complete.
func TestANegativeTerminationIsRejected(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.start(t.Context(), cmd)

	r.armed(t, 1)
	r.reply(asdu.CauseActCon, false, cmd)
	r.reply(asdu.CauseActTerm, true, cmd)

	res := result(t, done)
	if res.Outcome != OutcomeRejected {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeRejected, res.Err)
	}
}

// --- correlation ------------------------------------------------------------------

// 104 offers type and address and nothing else to match an answer to a request.
// After a simulator was found sending whole interrogation brackets nobody asked
// for, accepting any answer that happens to arrive is not a risk worth running.
func TestAnAnswerAboutAnotherPointDoesNotCloseTheDialogue(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.start(t.Context(), cmd)

	r.armed(t, 1)

	// Both wrong answers are REFUSALS. A positive one would be indistinguishable
	// from the right answer arriving early — the dialogue would finish
	// successfully either way and this test would pass against a service with no
	// correlation at all. A refusal cannot be confused with success, so if
	// either of these is taken as ours the outcome flips.
	r.replies <- Reply{Type: cmd.Type, IOA: cmd.IOA + 1, Cause: asdu.CauseActCon, Negative: true}
	r.replies <- Reply{Type: asdu.CDcNa1, IOA: cmd.IOA, Cause: asdu.CauseActCon, Negative: true}

	r.reply(asdu.CauseActCon, false, cmd)
	r.reply(asdu.CauseActTerm, false, cmd)

	res := result(t, done)
	if res.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome %s (%s): %v; a refusal meant for another point was taken as "+
			"this command's answer, and type plus address is the whole of the "+
			"correlation 104 offers", res.Outcome, res.Code, res.Err)
	}
	if res.Negative {
		t.Error("a reply about another point was taken as this command's answer")
	}
}

// A reply left over from an earlier command must not close the next one.
func TestAStaleReplyIsDrainedBeforeSending(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()

	// Left over from a previous dialogue, or a previous connection.
	r.reply(asdu.CauseActCon, false, cmd)
	r.reply(asdu.CauseActTerm, false, cmd)

	done := r.start(t.Context(), cmd)
	r.armed(t, 1)
	r.fake.Advance(11 * time.Second)

	res := result(t, done)
	if res.Outcome != OutcomeIndeterminate {
		t.Fatalf("outcome %s, want %s; a leftover confirmation closed a dialogue it had "+
			"nothing to do with, which reports another command's answer as this one's",
			res.Outcome, OutcomeIndeterminate)
	}
}

// --- nothing reaching the wire ------------------------------------------------------

func TestACommandThatCannotBeSentIsNotSent(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	r.wire.err = errors.New("connection closed")

	res := result(t, r.start(t.Context(), testCmd()))
	if res.Outcome != OutcomeNotSent {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeNotSent, res.Err)
	}
	if res.Code != ErrCodeStationDisconnected {
		t.Errorf("code %q, want %q", res.Code, ErrCodeStationDisconnected)
	}
	if !res.SentAt.IsZero() {
		t.Error("a command that never reached the wire has a send timestamp")
	}
}

// A qualifier this service refuses to encode must never become a frame. An
// invalid double command does not fail safely at the device — 0 and 3 are "not
// permitted", and what a device does with one is its own business.
func TestAnInvalidCommandNeverReachesTheWire(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	bad := asdu.Command{Type: asdu.CDcNa1, IOA: 4711, Double: 0}

	res := result(t, r.start(t.Context(), bad))
	if res.Outcome != OutcomeNotSent {
		t.Fatalf("outcome %s, want %s", res.Outcome, OutcomeNotSent)
	}
	if res.Code != ErrCodeInvalidValue {
		t.Errorf("code %q, want %q", res.Code, ErrCodeInvalidValue)
	}
	if n := r.wire.count(); n != 0 {
		t.Errorf("%d frames went out for a command that should never have been encoded", n)
	}
}

// --- ParseReply --------------------------------------------------------------------

func TestParseReplyReadsTheAddressOutOfTheEcho(t *testing.T) {
	t.Parallel()

	p := asdu.DefaultParams()

	for _, tc := range []struct {
		name string
		cmd  asdu.Command
	}{
		{"single", asdu.Command{Type: asdu.CScNa1, IOA: 4711, On: true}},
		{"double", asdu.Command{Type: asdu.CDcNa1, IOA: 900, Double: asdu.DoubleOn}},
		{"setpoint float", asdu.Command{Type: asdu.CSeNc1, IOA: 12345, Value: 3.5}},
		{"bitstring", asdu.Command{Type: asdu.CBoNa1, IOA: 7, Bits: 0xDEADBEEF}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Encoded as an activation and then relabelled, because
			// AppendCommand refuses to produce a confirmation — correctly: we
			// are the master, we send activations and a device answers them.
			// Building the answer by hand is a thing only a test needs to do.
			raw, err := p.AppendCommand(nil, asdu.CommandRequest{
				CommonAddr: 1,
				Cause:      asdu.CauseActivation,
				Cmd:        tc.cmd,
			})
			if err != nil {
				t.Fatalf("AppendCommand: %v", err)
			}
			raw = withCause(raw, asdu.CauseActCon)

			h, body, err := p.ParseHeader(raw)
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}

			got, ok := ParseReply(p, h, body)
			if !ok {
				t.Fatal("a well-formed confirmation was not recognised")
			}
			if got.Type != tc.cmd.Type || got.IOA != tc.cmd.IOA {
				t.Errorf("read %s ioa=%d, want %s ioa=%d", got.Type, got.IOA, tc.cmd.Type, tc.cmd.IOA)
			}
			if got.Cause != asdu.CauseActCon {
				t.Errorf("cause %s, want %s", got.Cause, asdu.CauseActCon)
			}
		})
	}
}

// withCause relabels an encoded ASDU. The cause octet follows the type and the
// variable structure qualifier, and the P/N and T bits live in its top two bits,
// so the low six are the cause.
func withCause(raw []byte, c asdu.Cause) []byte {
	const causeOctet = 2

	out := append([]byte(nil), raw...)
	if len(out) > causeOctet {
		out[causeOctet] = (out[causeOctet] &^ 0x3F) | (byte(c) & 0x3F)
	}
	return out
}

func TestParseReplyRefusesWhatIsNotACommandConfirmation(t *testing.T) {
	t.Parallel()

	p := asdu.DefaultParams()

	for _, tc := range []struct {
		name string
		h    asdu.Header
		body []byte
	}{
		{"a monitor type", asdu.Header{Type: asdu.MMeNc1, Count: 1, CommonAddr: 1}, make([]byte, 8)},
		{"more than one object", asdu.Header{Type: asdu.CScNa1, Count: 2, CommonAddr: 1}, make([]byte, 8)},
		{"a sequence", asdu.Header{Type: asdu.CScNa1, Count: 1, Sequence: true, CommonAddr: 1}, make([]byte, 4)},
		{"a body too short for an address", asdu.Header{Type: asdu.CScNa1, Count: 1, CommonAddr: 1}, []byte{0x01}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := ParseReply(p, tc.h, tc.body); ok {
				t.Error("accepted; the dialogue must not act on this")
			}
		})
	}
}

// --- configuration ------------------------------------------------------------------

func TestNewOperatorRejectsAnUnusableConfig(t *testing.T) {
	t.Parallel()

	replies := make(chan Reply)

	if _, err := NewOperator(OperatorConfig{Params: asdu.DefaultParams()}); !errors.Is(err, ErrConfig) {
		t.Errorf("no reply channel: %v, want ErrConfig", err)
	}
	if _, err := NewOperator(OperatorConfig{
		Params: asdu.DefaultParams(), Replies: replies, ActConTimeout: -time.Second,
	}); !errors.Is(err, ErrConfig) {
		t.Errorf("negative timeout: %v, want ErrConfig", err)
	}

	cfg := OperatorConfig{Params: asdu.DefaultParams(), Replies: replies}
	if _, err := NewOperator(cfg); err != nil {
		t.Fatalf("NewOperator: %v", err)
	}
}

func TestTheDefaultTimeoutsApply(t *testing.T) {
	t.Parallel()

	cfg := OperatorConfig{Params: asdu.DefaultParams(), Replies: make(chan Reply)}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if cfg.ActConTimeout != DefaultActConTimeout {
		t.Errorf("act-con timeout %s, want %s", cfg.ActConTimeout, DefaultActConTimeout)
	}
	if cfg.ActTermTimeout != DefaultActTermTimeout {
		t.Errorf("act-term timeout %s, want %s", cfg.ActTermTimeout, DefaultActTermTimeout)
	}

	// The termination waits for a mechanism, the confirmation only for a
	// protocol answer. A termination budget no larger than the confirmation's
	// would turn slow-but-successful operations into Indeterminate ones.
	if cfg.ActTermTimeout <= cfg.ActConTimeout {
		t.Errorf("act-term %s is not longer than act-con %s", cfg.ActTermTimeout, cfg.ActConTimeout)
	}
}

// --- select before operate --------------------------------------------------------

// startSBO runs a select-before-operate command in the background.
func (r *rig) startSBO(ctx context.Context, cmd asdu.Command) <-chan Result {
	out := make(chan Result, 1)
	go func() {
		out <- r.op.Run(ctx, r.wire.send, Request{
			Cmd:                 cmd,
			SelectBeforeOperate: true,
			Label:               "test command",
		})
	}()
	return out
}

// waitExecuted blocks until n frames have gone out.
//
// The timer count cannot be used to tell the selection from the execution: the
// selection's timer is stopped before the execution's is armed, so the count
// goes one, zero, one and never reaches two. Counting frames says what the test
// actually means — the execution has been sent.
func (r *rig) waitExecuted(t *testing.T, n int) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for r.wire.count() < n {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("%d frames went out, want %d", r.wire.count(), n)
		}
	}
}

// selectReply is a confirmation with the S/E bit set, which is what separates
// it from an execution's confirmation.
func (r *rig) selectReply(cause asdu.Cause, negative bool, cmd asdu.Command) {
	r.replies <- Reply{
		Type: cmd.Type, IOA: cmd.IOA, Cause: cause, Negative: negative,
		Select: true, HasSelect: true,
	}
}

func (r *rig) operateReply(cause asdu.Cause, negative bool, cmd asdu.Command) {
	r.replies <- Reply{
		Type: cmd.Type, IOA: cmd.IOA, Cause: cause, Negative: negative,
		Select: false, HasSelect: true,
	}
}

func TestSelectThenOperateSucceeds(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.startSBO(t.Context(), cmd)

	r.armed(t, 1)
	r.selectReply(asdu.CauseActCon, false, cmd)
	r.waitExecuted(t, 2)
	r.operateReply(asdu.CauseActCon, false, cmd)
	r.operateReply(asdu.CauseActTerm, false, cmd)

	res := result(t, done)
	if res.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome %s (%s): %v", res.Outcome, res.Code, res.Err)
	}
	if n := r.wire.count(); n != 2 {
		t.Errorf("%d frames went out, want 2 (a selection and an execution)", n)
	}
}

// The property the whole dialogue is for: if the selection is not confirmed,
// the execution is NEVER sent.
func TestAnUnconfirmedSelectionNeverExecutes(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	done := r.startSBO(t.Context(), testCmd())

	r.armed(t, 1)
	r.fake.Advance(6 * time.Second) // past the select timeout

	res := result(t, done)
	if res.Outcome != OutcomeNotSent {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeNotSent, res.Err)
	}
	if res.Code != ErrCodeSelectTimeout {
		t.Errorf("code %q, want %q", res.Code, ErrCodeSelectTimeout)
	}
	if n := r.wire.count(); n != 1 {
		t.Fatalf("%d frames went out; the execution must not be sent when the "+
			"selection was never confirmed — that is the entire reason a critical "+
			"point is operated this way", n)
	}
	if !res.Outcome.Safe() {
		t.Error("an unconfirmed selection changes nothing in the field and lapses on " +
			"its own, so trying again is safe and the caller should be told so")
	}
}

// A refused selection is the same story with a faster ending.
func TestARefusedSelectionNeverExecutes(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.startSBO(t.Context(), cmd)

	r.armed(t, 1)
	r.selectReply(asdu.CauseActCon, true, cmd) // P/N set

	res := result(t, done)
	if res.Outcome != OutcomeNotSent {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeNotSent, res.Err)
	}
	if res.Code != ErrCodeSelectRejected {
		t.Errorf("code %q, want %q", res.Code, ErrCodeSelectRejected)
	}
	if n := r.wire.count(); n != 1 {
		t.Errorf("%d frames went out after a refused selection, want 1", n)
	}
}

// The S/E bit is the only thing separating the two confirmations on the wire. A
// device that repeats its selection confirmation must not have the repeat read
// as the execution's — that would report an operation as done when only the
// reservation was acknowledged.
func TestARepeatedSelectConfirmationIsNotTakenForTheExecution(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.startSBO(t.Context(), cmd)

	r.armed(t, 1)
	r.selectReply(asdu.CauseActCon, false, cmd)

	r.waitExecuted(t, 2)
	r.armed(t, 1)
	// The device says it again, after we have sent the execution.
	r.selectReply(asdu.CauseActCon, false, cmd)
	r.selectReply(asdu.CauseActTerm, false, cmd)

	// Nothing has confirmed the execution yet, so this must still be waiting.
	r.fake.Advance(11 * time.Second)

	res := result(t, done)
	if res.Outcome != OutcomeIndeterminate {
		t.Fatalf("outcome %s, want %s; repeated SELECTION confirmations were taken for "+
			"the execution's, so an operation was reported on the strength of a "+
			"reservation: %v", res.Outcome, OutcomeIndeterminate, res.Err)
	}
}

// Past the selection, the execution is an ordinary operate — including the part
// where an unanswered one is Indeterminate rather than failed.
func TestAnUnansweredExecutionAfterASelectionIsIndeterminate(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	cmd := testCmd()
	done := r.startSBO(t.Context(), cmd)

	r.armed(t, 1)
	r.selectReply(asdu.CauseActCon, false, cmd)
	r.waitExecuted(t, 2)
	r.armed(t, 1)
	r.fake.Advance(11 * time.Second)

	res := result(t, done)
	if res.Outcome != OutcomeIndeterminate {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeIndeterminate, res.Err)
	}
	if res.Code != ErrCodeActConTimeout {
		t.Errorf("code %q, want %q", res.Code, ErrCodeActConTimeout)
	}
	if res.Outcome.Safe() {
		t.Error("the execution went out; it may have driven the output")
	}
}

// A bit-string command has no qualifier octet, so there is nowhere to put the
// S/E bit and it cannot be selected. Saying so beats quietly executing where a
// selection was asked for.
func TestABitstringCommandCannotBeSelected(t *testing.T) {
	t.Parallel()

	r := newRig(t, nil)
	res := result(t, r.startSBO(t.Context(), asdu.Command{
		Type: asdu.CBoNa1, IOA: 7, Bits: 0xDEADBEEF,
	}))

	if res.Outcome != OutcomeNotSent {
		t.Fatalf("outcome %s, want %s", res.Outcome, OutcomeNotSent)
	}
	if res.Code != ErrCodeInvalidValue {
		t.Errorf("code %q, want %q", res.Code, ErrCodeInvalidValue)
	}
	if n := r.wire.count(); n != 0 {
		t.Errorf("%d frames went out for a command that cannot be selected", n)
	}
}

// The S/E bit has to survive encoding and come back out of a real frame, or the
// separation above is theatre.
func TestTheSelectBitSurvivesARoundTrip(t *testing.T) {
	t.Parallel()

	p := asdu.DefaultParams()

	for _, tc := range []struct {
		name string
		cmd  asdu.Command
	}{
		{"single", asdu.Command{Type: asdu.CScNa1, IOA: 4711, On: true}},
		{"double", asdu.Command{Type: asdu.CDcNa1, IOA: 900, Double: asdu.DoubleOn}},
		{"setpoint float", asdu.Command{Type: asdu.CSeNc1, IOA: 12345, Value: 3.5}},
	} {
		for _, sel := range []bool{true, false} {
			name := tc.name + " execute"
			if sel {
				name = tc.name + " select"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				cmd := tc.cmd
				cmd.Select = sel

				raw, err := p.AppendCommand(nil, asdu.CommandRequest{
					CommonAddr: 1, Cause: asdu.CauseActivation, Cmd: cmd,
				})
				if err != nil {
					t.Fatalf("AppendCommand: %v", err)
				}
				h, body, err := p.ParseHeader(withCause(raw, asdu.CauseActCon))
				if err != nil {
					t.Fatalf("ParseHeader: %v", err)
				}

				got, ok := ParseReply(p, h, body)
				if !ok {
					t.Fatal("not recognised as a confirmation")
				}
				if !got.HasSelect {
					t.Fatal("no S/E bit was read out of a type that carries one")
				}
				if got.Select != sel {
					t.Errorf("S/E read as %t, want %t", got.Select, sel)
				}
			})
		}
	}
}
