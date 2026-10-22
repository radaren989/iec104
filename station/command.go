package station

import (
	"context"
	"fmt"
	"time"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/command"
)

// This file binds the command path to a station.
//
// The split is deliberate: command package
// knows nothing about a connection. It owns two questions — who goes when
// (Queue) and what the device's answer meant (Operator) — and both are testable
// without a socket. This file owns the third question, which is the only one
// that needs a station: which connection is current, and is it in a state to
// carry a command at all.
//
// The queue lives as long as the station, not as long as a connection. A command
// arriving while a station is reconnecting should be refused with a reason, not
// dropped into a queue that is about to be rebuilt — and a queue that came and
// went with each connection would lose its depth limit exactly when a flapping
// link makes that limit matter most.

// Commands is the command policy for one station.
type Commands struct {
	// Enabled says this station accepts commands at all.
	//
	// Off by default. A station that only needs reading has no business being
		// able to drive an output, and the cheapest way to guarantee that is for
		// the queue not to exist.
	Enabled bool

	// Depth is how many commands may be outstanding. Zero means
	// command.DefaultDepth.
	Depth int

	// ActConTimeout, ActTermTimeout and SelectTimeout bound the steps of the
	// dialogue. Zero means the defaults in internal/command.
	ActConTimeout  time.Duration
	ActTermTimeout time.Duration
	SelectTimeout  time.Duration

	// WaitForActTerm says this device sends a termination. See
	// command.OperatorConfig: against a device that does not, leaving this on
	// makes every command report Indeterminate.
	WaitForActTerm bool
}

// ErrCommandsDisabled is a command sent to a station that was not configured to
// accept any.
var ErrCommandsDisabled = fmt.Errorf("commands are not enabled for this station")

// CommandRequest is one command for this station.
//
// It is a struct rather than a command plus a boolean because a boolean
// parameter at a call site says nothing: SendCommand(ctx, cmd, true) is
// unreadable, and this one decides whether a point is reserved before it is
// driven. It also matches the field on the wire contract, so the gRPC layer
// copies rather than translates.
type CommandRequest struct {
	Cmd asdu.Command

	// SelectBeforeOperate reserves the point and waits for the device to
	// confirm the reservation before anything is executed. The execution is
	// never sent if the selection was not confirmed.
	SelectBeforeOperate bool
}

// SendCommand drives one point and waits for the result.
//
// The Result is the answer even when things went wrong: only a caller error —
// a full queue, or commands being switched off — comes back as an error. That
// is because "it failed" and "we do not know whether it happened" are different
// answers and the difference is the point of the whole package.
func (s *Station) SendCommand(ctx context.Context, req CommandRequest) (command.Result, error) {
	if s.commands == nil {
		res := command.Result{
			Outcome: command.OutcomeNotSent,
			Code:    ErrCodeCommandsDisabled,
			Err:     ErrCommandsDisabled,
		}
		// Reported like any other refusal: a station that takes no commands and
		// is being sent them is a configuration mistake somebody needs to see,
		// and it is invisible if only the caller is told.
		s.cfg.Observer.OnCommand(req.Cmd, res)
		return res, ErrCommandsDisabled
	}

	label := fmt.Sprintf("%s ioa=%d", req.Cmd.Type, req.Cmd.IOA)
	if req.SelectBeforeOperate {
		label += " (select+operate)"
	}

	res, err := s.commands.Send(ctx, command.Request{
		Cmd:                 req.Cmd,
		SelectBeforeOperate: req.SelectBeforeOperate,
		Label:               label,
	})
	// The dialogue reports what happened to a command that reached it. This
	// reports the ones that did not: a refused queue, a closed one, and a caller
	// that stopped waiting. Each of those is a fact an operator needs and
	// nothing else would record.
	//
	// A caller that gives up while its command is already on the wire produces
	// two records — this one, and the dialogue's when it finishes. They are two
	// different facts and both are true.
	if err != nil || res.Code == command.ErrCodeCallerGone {
		s.cfg.Observer.OnCommand(req.Cmd, res)
	}
	return res, err
}

// ErrCodeCommandsDisabled is reported when a station takes no commands.
const ErrCodeCommandsDisabled command.ErrorCode = "CommandsDisabled"

// dialogue is the seam between the queue and the connection.
//
// It runs on the queue's consumer goroutine. Everything it needs from the
// station is read under the lock and released before the dialogue begins,
// because the dialogue waits for a device and holding a station's lock for a
// minute would block every reader of the cache.
func (s *Station) dialogue(ctx context.Context, req command.Request) command.Result {
	s.mu.Lock()
	c, state := s.conn, s.state
	s.mu.Unlock()

	// A command needs a connection with data transfer started. Refusing here
	// rather than letting the send fail gives the caller a reason it can act
	// on, and — more importantly — it is unambiguously NotSent: nothing was
	// handed to a writer, so there is no chance the field saw it.
	if c == nil || !state.Connected() {
		res := command.Result{
			Outcome: command.OutcomeNotSent,
			Code:    command.ErrCodeStationDisconnected,
			Err:     fmt.Errorf("station is %s", state),
		}
		s.cfg.Observer.OnCommand(req.Cmd, res)
		return res
	}

	res := s.operator.Run(ctx, c.SendASDU, req)
	s.cfg.Observer.OnCommand(req.Cmd, res)
	return res
}

// routeCommandReply hands a control-direction answer to whichever command is
// waiting for it.
//
// It runs on the connection's goroutine, so it must not block:
// a full channel means an answer is dropped and one dialogue waits out its
// timeout, which is bad. Blocking here would stall the loop that answers t1 for
// this station and every frame behind it, which is worse.
func (s *Station) routeCommandReply(h asdu.Header, body []byte) {
	if s.cmdReplies == nil {
		return
	}
	reply, ok := command.ParseReply(s.cfg.ASDUParams, h, body)
	if !ok {
		return
	}
	select {
	case s.cmdReplies <- reply:
	default:
	}
}

// startCommands builds the queue and the dialogue for a station configured to
// accept commands.
//
// The reply channel is buffered because it is fed from the connection's
// goroutine, which must never block. Four deep: a device may
// repeat a confirmation, and with commands serialised there is never more than
// one dialogue's worth of answers legitimately in flight.
func (s *Station) startCommands() error {
	s.cmdReplies = make(chan command.Reply, 4)

	op, err := command.NewOperator(command.OperatorConfig{
		Params:         s.cfg.ASDUParams,
		CommonAddress:  s.cfg.CommonAddress,
		Originator:     s.cfg.Originator,
		Clock:          s.cfg.Clock,
		Replies:        s.cmdReplies,
		ActConTimeout:  s.cfg.Commands.ActConTimeout,
		ActTermTimeout: s.cfg.Commands.ActTermTimeout,
		SelectTimeout:  s.cfg.Commands.SelectTimeout,
		WaitForActTerm: s.cfg.Commands.WaitForActTerm,
	})
	if err != nil {
		return fmt.Errorf("building the command dialogue: %w", err)
	}
	s.operator = op

	q, err := command.New(command.Config{
		Depth: s.cfg.Commands.Depth,
		Run:   s.dialogue,
	})
	if err != nil {
		return fmt.Errorf("building the command queue: %w", err)
	}
	s.commands = q
	return nil
}
