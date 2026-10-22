package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
)

// This file is the direct-operate dialogue: activation, confirmation, and —
// when the device sends one — termination.
//
// Study Reference §8 is the authority, and its two hard lessons are both
// encoded here. The first is that a command is not finished when it is sent, or
// even when it is confirmed: the confirmation says the device accepted the
// request, and only the termination says the output finished moving. The second
// is that plenty of devices never send a termination at all, so a master that
// insists on one waits out its whole timeout on every single command and then
// reports a successful operation as unknown.
//
// The awkward truth in between is that "no answer" is NOT failure. A command
// that went out and was never confirmed may have driven the output. Saying it
// failed invites a retry, and a retry is a second operation on a live breaker.
// Every timeout in this file therefore ends in Indeterminate, and Indeterminate
// is never retried automatically (Outcome.Safe).

// Sender puts one encoded ASDU on the wire.
//
// It returns when the frame has been handed to the connection's writer, not
// when the device has acknowledged it — so a nil error means "sent", not
// "received". That distinction is why a send failure is NotSent and a missing
// confirmation is not.
type Sender func(ctx context.Context, raw []byte) error

// Reply is a control-direction answer, reduced to the four things the dialogue
// decides on.
//
// The information object address is in here rather than left in the body
// because it is half of the only correlation IEC 104 offers. There is no request
// identifier: a confirmation is matched to a request by type and address and
// nothing else, which is exactly why commands to one station are serialised
// (see the package comment).
type Reply struct {
	Type     asdu.TypeID
	IOA      asdu.IOA
	Cause    asdu.Cause
	Negative bool // the P/N bit: the device understood and refused

	// Select is the S/E bit the device echoed back. It is the only thing on the
	// wire that tells a selection's confirmation from an execution's: same
	// type, same address, same cause, one bit apart. Without it a repeated
	// select confirmation would be taken for the execute confirmation and an
	// operation reported as done when only the selection was acknowledged.
	//
	// HasSelect is false for a type with no qualifier octet, where the question
	// does not arise.
	Select    bool
	HasSelect bool
}

// ParseReply reduces a control-direction ASDU to a Reply.
//
// It reports false for anything that is not a single-object control ASDU. A
// command confirmation always carries exactly one object — it echoes the
// request — so a frame that does not is either not an answer to us or is
// malformed, and either way the dialogue must not act on it.
func ParseReply(p asdu.Params, h asdu.Header, body []byte) (Reply, bool) {
	if !asdu.SupportsControlType(h.Type) || h.Count != 1 || h.Sequence {
		return Reply{}, false
	}

	// The element length is whatever is left after the address. Deriving it
	// this way rather than looking the type up keeps every control type working
	// without a second table that could disagree with the encoder's.
	elemLen := len(body) - int(p.IOAOctets)
	if elemLen < 0 {
		return Reply{}, false
	}
	elems, err := p.SplitElements(h, body, elemLen)
	if err != nil || len(elems) != 1 {
		return Reply{}, false
	}

	sel, hasSel := h.Type.SelectBit(elems[0].Data)
	return Reply{
		Type:      h.Type,
		IOA:       elems[0].IOA,
		Cause:     h.Cause,
		Negative:  h.Negative,
		Select:    sel,
		HasSelect: hasSel,
	}, true
}

// Rejected reports whether this cause means the device refused the command.
//
// Causes 44 to 47 are the device saying it did not understand the request —
// unknown type, cause, common address or information object address. They are
// refusals, not transport failures: the output did not move and the device said
// so, which makes them safe to correct and send again.
func rejecting(c asdu.Cause) bool {
	switch c {
	case asdu.CauseUnknownType, asdu.CauseUnknownCause,
		asdu.CauseUnknownCommonAd, asdu.CauseUnknownIOA:
		return true
	default:
		return false
	}
}

// Default timeouts.
//
// ActCon is short because a confirmation is a protocol answer: the device has
// only to accept the request, and a device that has not done so in ten seconds
// is not about to. ActTerm is long because a termination waits for a physical
// mechanism — a breaker takes as long as it takes, and cutting that short turns
// a successful operation into an Indeterminate one, which is the most expensive
// wrong answer this service can give.
const (
	DefaultActConTimeout  = 10 * time.Second
	DefaultActTermTimeout = 60 * time.Second
)

// OperatorConfig describes how one station's commands are carried out.
type OperatorConfig struct {
	Params        asdu.Params
	CommonAddress uint16
	Originator    uint8
	Clock         clock.Clock

	// Replies carries control-direction answers from the connection's
	// goroutine. It is buffered and fed without blocking: a dropped reply
	// stalls a dialogue until its timeout, but a blocked feed would stall the
	// connection that carries every other station's data too.
	Replies <-chan Reply

	// ActConTimeout bounds the wait for the confirmation. Zero means the
	// default.
	ActConTimeout time.Duration

	// ActTermTimeout bounds the wait for the termination. Zero means the
	// default.
	ActTermTimeout time.Duration

	// SelectTimeout bounds the selection step of select-before-operate. Zero
	// means DefaultSelectTimeout.
	SelectTimeout time.Duration

	// WaitForActTerm says this device sends a termination.
	//
	// It is per device and it defaults to true, because the standard says a
	// termination follows and treating an unfinished operation as successful is
	// the optimistic mistake. But Study Reference §8 is blunt that many devices
	// never send one, and against those this must be turned OFF: leaving it on
	// makes every command wait out ActTermTimeout and then report Indeterminate,
	// which reads as "we do not know whether the breaker moved" for a command
	// the device confirmed and carried out perfectly.
	WaitForActTerm bool
}

func (cfg *OperatorConfig) applyDefaults() error {
	if cfg.ActConTimeout == 0 {
		cfg.ActConTimeout = DefaultActConTimeout
	}
	if cfg.ActTermTimeout == 0 {
		cfg.ActTermTimeout = DefaultActTermTimeout
	}
	if cfg.SelectTimeout == 0 {
		cfg.SelectTimeout = DefaultSelectTimeout
	}
	if cfg.ActConTimeout < 0 || cfg.ActTermTimeout < 0 || cfg.SelectTimeout < 0 {
		return fmt.Errorf("%w: negative timeout", ErrConfig)
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System()
	}
	if cfg.Replies == nil {
		return fmt.Errorf("%w: no reply channel", ErrConfig)
	}
	if err := cfg.Params.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	return nil
}

// Operator runs one command at a time against one station.
//
// It is not safe for concurrent use and is not meant to be: Queue guarantees a
// single caller, and that guarantee is what lets the reply channel be matched
// by type and address alone.
type Operator struct {
	cfg OperatorConfig
}

// NewOperator builds the dialogue half of the command path.
func NewOperator(cfg OperatorConfig) (*Operator, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	return &Operator{cfg: cfg}, nil
}

// Error codes this file can produce, in addition to those in command.go.
const (
	// ErrCodeOperateRejected is the device refusing: P/N set, or one of the
	// causes 44 to 47.
	ErrCodeOperateRejected ErrorCode = "OperateRejected"

	// ErrCodeActConTimeout is no confirmation within ActConTimeout. The command
	// went out, so this is Indeterminate, not a failure.
	ErrCodeActConTimeout ErrorCode = "ActConTimeout"

	// ErrCodeActTermTimeout is a confirmed command with no termination within
	// ActTermTimeout. Also Indeterminate: the device accepted it and we do not
	// know whether the output finished moving.
	ErrCodeActTermTimeout ErrorCode = "ActTermTimeout"

	// ErrCodeStationDisconnected is no usable connection at the moment of
	// sending. Nothing reached the field.
	ErrCodeStationDisconnected ErrorCode = "StationDisconnected"

	// ErrCodeInvalidValue is a command this service refuses to encode. It never
	// reaches the wire (S3, S8).
	ErrCodeInvalidValue ErrorCode = "InvalidValue"

	// ErrCodeSelectRejected is the device refusing the selection. Nothing was
	// executed, and a selection that is refused changes nothing in the field.
	ErrCodeSelectRejected ErrorCode = "SelectRejected"

	// ErrCodeSelectTimeout is no answer to the selection.
	//
	// It is NotSent, not Indeterminate, and the difference is the most useful
	// property select-before-operate has: a selection reserves a point and
	// lapses on its own, so an unanswered one leaves the field exactly as it
	// was. That is why a critical point is selected first — the dangerous half
	// of the dialogue never starts unless the harmless half was confirmed.
	ErrCodeSelectTimeout ErrorCode = "SelectTimeout"
)

// DefaultSelectTimeout bounds the selection. It is shorter than the activation
// timeout because a selection is a reservation, not an operation: a device has
// only to say yes.
const DefaultSelectTimeout = 5 * time.Second

// Run carries out one command.
//
// It is a Dialogue once a Sender is bound to it; see Station wiring. The
// returned Result is always meaningful — an error return would lose the
// difference between "not sent" and "we do not know", which is the whole
// distinction this package exists to preserve.
func (o *Operator) Run(ctx context.Context, send Sender, req Request) Result {
	res := Result{}

	// --- the selection, when this point is operated that way ---
	//
	// Select-before-operate exists so that the dangerous half of the dialogue
	// never starts unless the harmless half was confirmed. Everything below
	// therefore returns early on anything but a positive selection, and every
	// one of those early returns is NotSent: a selection reserves a point and
	// lapses on its own, so a selection that failed leaves the field exactly as
	// it was.
	if req.SelectBeforeOperate {
		if done, r := o.selectPoint(ctx, send, req); done {
			return r
		}
	}

	frame, err := o.cfg.Params.AppendCommand(nil, asdu.CommandRequest{
		CommonAddr: o.cfg.CommonAddress,
		Originator: o.cfg.Originator,
		Cause:      asdu.CauseActivation,
		Cmd:        o.withSelect(req.Cmd, false),
	})
	if err != nil {
		// Refused before the wire. A qualifier this service will not encode is
		// a request that must never become a frame (S3): an invalid qualifier
		// does not fail safely at the device, it drives something else.
		res.Outcome = OutcomeNotSent
		res.Code = ErrCodeInvalidValue
		res.Err = fmt.Errorf("encoding %s: %w", req.Label, err)
		return res
	}

	// Anything already in the channel belongs to an earlier command, an earlier
	// connection, or the selection we have just finished with. Left there, it
	// would close this dialogue instantly and report someone else's answer as
	// ours.
	o.drain()

	sentAt := o.cfg.Clock.Now()
	if err := send(ctx, frame); err != nil {
		res.Outcome = OutcomeNotSent
		res.Code = ErrCodeStationDisconnected
		res.Err = fmt.Errorf("sending %s: %w", req.Label, err)
		return res
	}
	res.SentAt = sentAt

	// --- the confirmation ---
	conTimer := o.cfg.Clock.NewTimer(o.cfg.ActConTimeout)
	defer conTimer.Stop()

	con, err := o.await(ctx, req, asdu.CauseActCon, false, conTimer.C())
	if err != nil {
		// The command is on the wire and unanswered. It MAY have driven the
		// output, so this is not a failure that anyone may retry.
		res.Outcome = OutcomeIndeterminate
		res.Code = ErrCodeActConTimeout
		res.Err = err
		return res
	}
	res.LastCause = con.Cause
	res.Negative = con.Negative
	res.ConfirmedAt = o.cfg.Clock.Now()

	if con.Negative || rejecting(con.Cause) {
		// The device understood and refused. The output did not move and it
		// said so, which makes this the one unhappy ending that is safe to
		// correct and send again.
		res.Outcome = OutcomeRejected
		res.Code = ErrCodeOperateRejected
		res.Err = fmt.Errorf("%s refused: %s", req.Label, describe(con))
		return res
	}

	if !o.cfg.WaitForActTerm {
		res.Outcome = OutcomeSucceeded
		return res
	}

	// --- the termination ---
	termTimer := o.cfg.Clock.NewTimer(o.cfg.ActTermTimeout)
	defer termTimer.Stop()

	term, err := o.await(ctx, req, asdu.CauseActTerm, false, termTimer.C())
	if err != nil {
		// Confirmed but never terminated. The device accepted it; whether the
		// output finished moving is unknown, and guessing either way is worse
		// than saying so.
		res.Outcome = OutcomeIndeterminate
		res.Code = ErrCodeActTermTimeout
		res.Err = err
		return res
	}
	res.LastCause = term.Cause
	res.Negative = term.Negative
	res.ConfirmedAt = o.cfg.Clock.Now()

	if term.Negative {
		// A negative termination is the device reporting that the operation it
		// had accepted did not complete. The output is where it was.
		res.Outcome = OutcomeRejected
		res.Code = ErrCodeOperateRejected
		res.Err = fmt.Errorf("%s did not complete: %s", req.Label, describe(term))
		return res
	}

	res.Outcome = OutcomeSucceeded
	return res
}

// await waits for one half of the dialogue.
func (o *Operator) await(ctx context.Context, req Request, want asdu.Cause, wantSelect bool, timeout <-chan time.Time) (Reply, error) {
	for {
		select {
		case r := <-o.cfg.Replies:
			// Type and address are the whole of the correlation 104 offers, so
			// an answer about a different point is somebody else's and must not
			// close this dialogue. After the Freyr simulator was found sending
			// interrogation brackets nobody asked for, taking any answer that
			// happens to arrive is not a risk worth running.
			if r.Type != req.Cmd.Type || r.IOA != req.Cmd.IOA {
				continue
			}
			// And the S/E bit separates the two halves of a select dialogue,
			// which are otherwise identical on the wire. A device that repeats
			// its selection confirmation must not have the repeat read as the
			// execution's.
			if r.HasSelect && r.Select != wantSelect {
				continue
			}
			// A refusal ends the dialogue whichever half we were waiting for:
			// there is no point waiting for a termination of something the
			// device just said it will not do.
			if r.Cause == want || r.Negative || rejecting(r.Cause) {
				return r, nil
			}

		case <-timeout:
			return Reply{}, fmt.Errorf("%w: no %s for %s within %s",
				ErrNoAnswer, want, req.Label, o.timeoutFor(want))

		case <-ctx.Done():
			// The caller gave up. The command is still out there, so this is
			// reported as no answer rather than as a cancellation: the outcome
			// above turns it into Indeterminate, which is the truth.
			return Reply{}, fmt.Errorf("%w: caller gave up waiting for %s: %w",
				ErrNoAnswer, want, ctx.Err())
		}
	}
}

// selectPoint runs the first half of select-before-operate. It reports whether
// the dialogue is over, and if so with what.
func (o *Operator) selectPoint(ctx context.Context, send Sender, req Request) (bool, Result) {
	res := Result{}

	frame, err := o.cfg.Params.AppendCommand(nil, asdu.CommandRequest{
		CommonAddr: o.cfg.CommonAddress,
		Originator: o.cfg.Originator,
		Cause:      asdu.CauseActivation,
		Cmd:        o.withSelect(req.Cmd, true),
	})
	if err != nil {
		// A bit-string command has no qualifier octet and so cannot be
		// selected. The encoder says so rather than quietly sending an
		// execution where a selection was asked for.
		res.Outcome = OutcomeNotSent
		res.Code = ErrCodeInvalidValue
		res.Err = fmt.Errorf("encoding the selection of %s: %w", req.Label, err)
		return true, res
	}

	o.drain()

	if err := send(ctx, frame); err != nil {
		res.Outcome = OutcomeNotSent
		res.Code = ErrCodeStationDisconnected
		res.Err = fmt.Errorf("selecting %s: %w", req.Label, err)
		return true, res
	}

	timer := o.cfg.Clock.NewTimer(o.cfg.SelectTimeout)
	defer timer.Stop()

	con, err := o.await(ctx, req, asdu.CauseActCon, true, timer.C())
	if err != nil {
		// NotSent, and this is the whole point of selecting first: no execution
		// was sent, and a selection the device did not confirm reserves nothing
		// and lapses by itself. The field is untouched and a caller may try
		// again safely.
		res.Outcome = OutcomeNotSent
		res.Code = ErrCodeSelectTimeout
		res.Err = err
		return true, res
	}

	res.LastCause = con.Cause
	res.Negative = con.Negative

	if con.Negative || rejecting(con.Cause) {
		res.Outcome = OutcomeNotSent
		res.Code = ErrCodeSelectRejected
		res.Err = fmt.Errorf("%s was not selected: %s", req.Label, describe(con))
		return true, res
	}

	// Selected. The execution follows on the same connection, in the same
	// dialogue, with no chance for another command to come between them —
	// Queue runs one at a time, and that is what makes the pair atomic.
	return false, res
}

// withSelect returns the command with its S/E bit set as the dialogue needs it.
//
// The caller asks for select-before-operate; it does not set the bit. Leaving
// that to a caller would mean a request could ask for a selection and get an
// execution, or the reverse, depending on a field nobody looked at twice.
func (o *Operator) withSelect(c asdu.Command, sel bool) asdu.Command {
	c.Select = sel
	return c
}

func (o *Operator) timeoutFor(want asdu.Cause) time.Duration {
	if want == asdu.CauseActTerm {
		return o.cfg.ActTermTimeout
	}
	return o.cfg.ActConTimeout
}

// drain empties the reply channel.
func (o *Operator) drain() {
	for {
		select {
		case <-o.cfg.Replies:
		default:
			return
		}
	}
}

// ErrNoAnswer is a command that went out and was not answered in time. It is
// wrapped rather than returned bare so that a caller can tell it apart from a
// refusal programmatically.
var ErrNoAnswer = errors.New("no answer from the device")

// describe says why a device refused, in the terms the device used.
func describe(r Reply) string {
	if r.Negative {
		return "P/N set on " + r.Cause.String()
	}
	return r.Cause.String()
}
