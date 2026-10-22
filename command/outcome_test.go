package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// This file is T8.4, and it exists to make one decision impossible to change by
// accident.
//
// A command that went out and was never answered MAY have driven the output.
// There is no way to find out from the protocol, so the only honest answer is
// Indeterminate — and the only safe handling of Indeterminate is that nothing
// sends it again on its own. A retry there is a second operation on a live
// breaker, and it is the kind of retry that gets added later by someone
// generalising "transient failures", because that is exactly what a timeout
// looks like from a distance.
//
// So the mapping is written out code by code below, and a new code that has not
// been thought about fails the build rather than defaulting to retryable.

type verdict struct {
	outcome Outcome
	safe    bool
	why     string
}

// retryVerdicts is the decision for every error code this service produces.
var retryVerdicts = map[ErrorCode]verdict{
	ErrCodeQueueFull: {OutcomeNotSent, true,
		"the station was at its limit; nothing was encoded, let alone sent"},
	ErrCodeCallerGone: {OutcomeNotSent, true,
		"the caller stopped waiting before the command left the queue"},
	ErrCodeShuttingDown: {OutcomeNotSent, true,
		"the queue closed with this one still waiting"},
	ErrCodeStationDisconnected: {OutcomeNotSent, true,
		"there was no connection to hand it to"},
	ErrCodeInvalidValue: {OutcomeNotSent, true,
		"the encoder refused it, so it never became a frame"},
	ErrCodeSelectTimeout: {OutcomeNotSent, true,
		"a selection that is not confirmed reserves nothing and lapses on its own; " +
			"no execution was sent and the field is untouched"},
	ErrCodeSelectRejected: {OutcomeNotSent, true,
		"the device refused the reservation; the execution was never sent"},

	ErrCodeOperateRejected: {OutcomeRejected, true,
		"the device answered and refused: the output did not move and it said so"},

	ErrCodeActConTimeout: {OutcomeIndeterminate, false,
		"the activation is on the wire and unanswered; it MAY have driven the output"},
	ErrCodeActTermTimeout: {OutcomeIndeterminate, false,
		"the device accepted it and never said it finished; the output may be mid-travel"},
}

// Every code this service can produce has to have a decided verdict. A code
// added without one fails here rather than inheriting whatever the surrounding
// branch happened to do.
func TestEveryErrorCodeHasADecidedRetryVerdict(t *testing.T) {
	t.Parallel()

	for _, code := range contractErrorCodes {
		v, decided := retryVerdicts[code]
		if !decided {
			t.Errorf("%s has no entry in retryVerdicts. Whoever added it has to say "+
				"what it means: did the command reach the field, and may a caller send "+
				"it again? Guessing is how an Indeterminate becomes a second operation "+
				"on a breaker.", code)
			continue
		}
		if got := v.outcome.Safe(); got != v.safe {
			t.Errorf("%s: outcome %s reports Safe()=%t, but the table says %t because %s",
				code, v.outcome, got, v.safe, v.why)
		}
	}

	// And nothing in the table that the service cannot actually produce: a stale
	// entry is a decision about a case that no longer exists, which reads as
	// coverage and is not.
	known := make(map[ErrorCode]bool, len(contractErrorCodes))
	for _, c := range contractErrorCodes {
		known[c] = true
	}
	for code := range retryVerdicts {
		if !known[code] {
			t.Errorf("retryVerdicts has %s, which is not in contractErrorCodes", code)
		}
	}
}

// The codes are wire values and metric label values, so their spelling is not a
// private matter.
func TestEveryErrorCodeIsAUsableWireValue(t *testing.T) {
	t.Parallel()

	seen := make(map[ErrorCode]bool, len(contractErrorCodes))
	for _, c := range contractErrorCodes {
		if c == "" {
			t.Error("an error code is empty")
			continue
		}
		if seen[c] {
			t.Errorf("%q is listed twice", c)
		}
		seen[c] = true

		if strings.ContainsAny(string(c), " \t\n\"{}") {
			t.Errorf("%q contains a character that has no business in a label or a "+
				"protobuf string", c)
		}
	}
}

// Exactly two outcomes are safe to send again, and both of them are safe for the
// same reason: the field saw nothing, or the device said it did nothing.
func TestOnlyTwoOutcomesAreSafeToSendAgain(t *testing.T) {
	t.Parallel()

	safe := 0
	for _, o := range []Outcome{OutcomeSucceeded, OutcomeRejected, OutcomeIndeterminate, OutcomeNotSent} {
		if o.Safe() {
			safe++
		}
	}
	if safe != 2 {
		t.Errorf("%d outcomes report themselves safe to retry, want exactly 2 "+
			"(NotSent and Rejected)", safe)
	}
}

// --- nothing resends on its own -------------------------------------------------

// The behavioural half of the same rule: on every path that ends Indeterminate,
// exactly one activation reaches the wire and no second one follows.
//
// This runs through the Queue rather than calling the dialogue directly, because
// the queue is where a retry would most plausibly be added — it already has the
// request, the result and a consumer loop to put it in.
func TestNoIndeterminatePathSendsASecondTime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		code    ErrorCode
		advance func(t *testing.T, r *rig, cmd asdu.Command)
	}{
		{
			name: "the activation is never confirmed",
			code: ErrCodeActConTimeout,
			advance: func(t *testing.T, r *rig, _ asdu.Command) {
				r.armed(t, 1)
				r.fake.Advance(11 * time.Second)
			},
		},
		{
			name: "confirmed and never terminated",
			code: ErrCodeActTermTimeout,
			advance: func(t *testing.T, r *rig, cmd asdu.Command) {
				r.armed(t, 1)
				r.reply(asdu.CauseActCon, false, cmd)
				r.armed(t, 2)
				r.fake.Advance(61 * time.Second)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newRig(t, nil)
			q, err := New(Config{Run: func(ctx context.Context, req Request) Result {
				return r.op.Run(ctx, r.wire.send, req)
			}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = q.Close() })

			cmd := testCmd()
			done := make(chan Result, 1)
			go func() {
				res, _ := q.Send(t.Context(), Request{Cmd: cmd, Label: "test command"})
				done <- res
			}()

			tc.advance(t, r, cmd)

			res := result(t, done)
			if res.Outcome != OutcomeIndeterminate {
				t.Fatalf("outcome %s, want %s: %v", res.Outcome, OutcomeIndeterminate, res.Err)
			}
			if res.Code != tc.code {
				t.Errorf("code %q, want %q", res.Code, tc.code)
			}

			// One activation went out. Give a second one time to appear.
			settleForNegative()
			if n := r.wire.count(); n != 1 {
				t.Fatalf("%d activations reached the wire for one Indeterminate command. "+
					"Something resent it, and resending a command that may already have "+
					"driven the output is a second operation on a live breaker", n)
			}
			if res.Outcome.Safe() {
				t.Error("the result invites the caller to send it again as well")
			}
		})
	}
}

// settleForNegative gives a frame that must NOT be sent time to be sent.
func settleForNegative() {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
}
