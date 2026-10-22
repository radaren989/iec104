// Package command runs commands against one station, one at a time.
//
// This is the package that drives a breaker. Everything here is written on the
// assumption that the answer to "what if this goes wrong" is physical.
//
// The rule the whole package exists to enforce is serialisation. IEC 104 has no
// correlation identifier on a command: a confirmation carries the type, the
// common address and the information object address, and nothing that says
// which of your requests it belongs to. Two commands to the same point in
// flight at once therefore cannot be told apart, and a service that allowed it
// would eventually report the wrong one as successful. That is not a race that
// shows up as a crash — it shows up as an operator being told a breaker opened
// when it did not.
//
// So: one queue per station, one consumer, no overlap, ever.
package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/radaren989/iec104/asdu"
)

// Sentinels the caller compares against.
var (
	// ErrQueueFull means this station already has as many commands accepted as
	// it is allowed. The command was NOT sent.
	//
	// Rejecting is deliberate, and blocking would be worse: a caller holding a
	// gRPC deadline open behind four other commands learns nothing useful, and
	// an unbounded queue turns a slow device into a memory leak.
	ErrQueueFull = errors.New("command queue is full")

	// ErrClosed means the queue is shutting down and took no new work.
	ErrClosed = errors.New("command queue is closed")

	// ErrConfig is a queue built wrong. A programming error, not a field one.
	ErrConfig = errors.New("invalid command queue configuration")
)

// Outcome is how a command ended.
//
// The four values are a safety decision, not a status code (design §5.6, Study
// Reference §8). The important one is Indeterminate: a command that was sent
// and never confirmed MAY have driven the output, so it is not a failure that
// can be retried. Anything that collapses Indeterminate into "failed" invites a
// caller to send it again, which is driving the breaker a second time.
type Outcome string

const (
	// OutcomeSucceeded is a command the device confirmed.
	OutcomeSucceeded Outcome = "Succeeded"

	// OutcomeRejected is a command the device refused. The output did not move,
	// and the device said so.
	OutcomeRejected Outcome = "Rejected"

	// OutcomeIndeterminate is a command that went out and was never answered.
	// It MAY have driven the output. It is never retried automatically, and a
	// human decides what happens next.
	OutcomeIndeterminate Outcome = "Indeterminate"

	// OutcomeNotSent is a command that never reached the wire. Retrying it is
	// safe, because the field saw nothing.
	OutcomeNotSent Outcome = "NotSent"
)

// String makes Outcome printable without a conversion at every call site.
func (o Outcome) String() string { return string(o) }

// Safe reports whether a caller may send this command again.
//
// It exists so that no caller has to remember which outcomes are retryable: the
// one dangerous case is the one that looks most like a transient failure, so the
// question is asked here rather than re-derived at every call site.
func (o Outcome) Safe() bool {
	return o == OutcomeNotSent || o == OutcomeRejected
}

// ErrorCode says why a command did not succeed.
//
// The spellings are the wire contract's, exactly (SendCommandResponse.error_code
// in iec104_manager.proto), and so are Outcome's. One vocabulary end to end
// means the gRPC layer copies rather than translates, and a translation table
// between an internal spelling and an external one is a thing that drifts —
// silently, and in the layer where a wrong answer is a breaker.
//
// They are also metric label values (iec104_command_total). Label VALUES have
// no case convention in Prometheus; only names do.
type ErrorCode string

const (
	// ErrCodeQueueFull is the station's queue being at its limit.
	ErrCodeQueueFull ErrorCode = "QueueFull"

	// ErrCodeCallerGone is the caller having given up before the command was
	// sent. Reported rather than silently dropped, because a caller that keeps
	// timing out while queued is a capacity problem worth seeing.
	ErrCodeCallerGone ErrorCode = "CallerGone"

	// ErrCodeShuttingDown is the queue closing with work still waiting.
	ErrCodeShuttingDown ErrorCode = "ShuttingDown"
)

// Result is what a command produced.
type Result struct {
	Outcome Outcome

	// Code is set for everything except OutcomeSucceeded.
	Code ErrorCode

	// Err carries the detail behind Code, for logs. Callers branch on Outcome
	// and Code, never on this.
	Err error

	// LastCause is the cause of transmission on the last answer the device
	// gave, and Negative its P/N bit. They are the device's own words about
	// what happened, kept because "rejected" alone does not tell an operator
	// whether to fix the request or the wiring.
	LastCause asdu.Cause
	Negative  bool

	// SentAt is when the command reached the connection's writer, and
	// ConfirmedAt when the last answer arrived. Both are zero when nothing was
	// sent. They exist so that an Indeterminate outcome can be investigated
	// against the device's own log rather than argued about.
	SentAt      time.Time
	ConfirmedAt time.Time
}

// Dialogue runs one command to completion on an established connection.
//
// It is the seam between this file, which is about who goes when, and the
// protocol exchange, which is about what the device said. Splitting them is what
// lets the serialisation rules be tested without a socket and the dialogue rules
// be tested without a queue.
//
// A Dialogue is called from the consumer goroutine and never concurrently with
// itself. It is given the caller's context, so a caller that gives up mid-flight
// can end the wait — but see Result: giving up on a command already on the wire
// produces Indeterminate, not a cancellation.
type Dialogue func(ctx context.Context, req Request) Result

// Request is one command.
type Request struct {
	// Cmd is the point to drive and what to drive it to. Encoding and
	// validating it belongs to internal/asdu; this package decides only who
	// goes when and what the answer meant.
	Cmd asdu.Command

	// SelectBeforeOperate runs the two-step dialogue: reserve the point, wait
	// for the device to confirm the reservation, and only then execute.
	//
	// The bit on Cmd is set by the dialogue, not by the caller. A caller that
	// set it itself could ask for a selection and get an execution.
	SelectBeforeOperate bool

	// Label is a short description for logs and test failures, such as
	// "C_SC_NA_1 ioa=4711 on". It is never used to make a decision.
	Label string
}

// Config describes one station's queue.
type Config struct {
	// Depth is how many commands may be accepted at once, queued and executing
	// together. Zero means DefaultDepth.
	//
	// It counts the executing command too, so a depth of four means the fifth
	// caller is refused, not the sixth. Counting only the waiting ones would
	// make the limit quietly one larger than it reads.
	Depth int

	// Run is the dialogue. Required.
	Run Dialogue
}

// DefaultDepth is the queue depth when none is configured.
//
// Four, because a station with more than a few commands outstanding is a
// station something is wrong with: 104 commands are operator actions, not a
// data stream, and a deep queue would only let a backlog build where a refusal
// tells the caller the truth sooner.
const DefaultDepth = 4

func (cfg *Config) applyDefaults() error {
	if cfg.Depth == 0 {
		cfg.Depth = DefaultDepth
	}
	if cfg.Depth < 1 {
		return fmt.Errorf("%w: depth %d", ErrConfig, cfg.Depth)
	}
	if cfg.Run == nil {
		return fmt.Errorf("%w: no dialogue", ErrConfig)
	}
	return nil
}

// job is one queued command and the channel its caller is waiting on.
type job struct {
	ctx  context.Context //nolint:containedctx // see below
	req  Request
	done chan Result
}

// The context is stored on the job rather than passed along a call chain
// because the queue is exactly the place where a caller's deadline outlives its
// own stack frame: Send hands the work to another goroutine and waits. The
// consumer uses it for one decision only — whether this command may still be
// sent — and then passes it on.

// Queue serialises commands for one station.
//
// The zero value is not usable; call New.
type Queue struct {
	cfg  Config
	jobs chan job

	// admitted is how many commands are accepted and not yet finished, queued
	// and executing together. It is the admission counter AND the in-flight
	// gauge, deliberately one number: two counters that mean almost the same
	// thing drift, and the one that drifts is always the one on the dashboard.
	admitted atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// New starts a queue and its single consumer.
func New(cfg Config) (*Queue, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	q := &Queue{
		cfg:    cfg,
		jobs:   make(chan job, cfg.Depth),
		closed: make(chan struct{}),
	}

	q.wg.Add(1)
	go q.consume()
	return q, nil
}

// Send runs one command and waits for its result.
//
// It returns ErrQueueFull immediately when the station is at its limit; it does
// not block waiting for a slot. Everything else comes back as a Result: a
// command that was refused, or timed out, or never sent, is news rather than an
// error, and the difference between those is the whole point of Outcome.
func (q *Queue) Send(ctx context.Context, req Request) (Result, error) {
	select {
	case <-q.closed:
		return Result{}, ErrClosed
	default:
	}

	// Admission first, so the limit is enforced before anything is queued.
	// Compare-and-swap rather than a plain add: an add would let two callers
	// briefly push the count past the limit, and "briefly" on a breaker queue
	// is still a command that should not have been accepted.
	for {
		n := q.admitted.Load()
		if n >= int64(q.cfg.Depth) {
			return Result{
				Outcome: OutcomeNotSent,
				Code:    ErrCodeQueueFull,
				Err:     fmt.Errorf("%w: %d already accepted", ErrQueueFull, n),
			}, ErrQueueFull
		}
		if q.admitted.CompareAndSwap(n, n+1) {
			break
		}
	}

	j := job{ctx: ctx, req: req, done: make(chan Result, 1)}

	select {
	case q.jobs <- j:
	case <-q.closed:
		q.admitted.Add(-1)
		return Result{}, ErrClosed
	}

	select {
	case res := <-j.done:
		return res, nil

	case <-ctx.Done():
		// The caller gave up. This does NOT cancel a command already on the
		// wire — the consumer owns that decision and will report Indeterminate
		// if it comes to it. All that happens here is that nobody is left
		// listening, which is why done is buffered.
		return Result{
			Outcome: OutcomeNotSent,
			Code:    ErrCodeCallerGone,
			Err:     ctx.Err(),
		}, nil
	}
}

// InFlight is how many commands are accepted and not yet finished.
//
// It is the source of iec104_commands_in_flight. It counts queued commands as
// well as the executing one, because a caller waiting behind three others is
// waiting just as surely as the one on the wire.
func (q *Queue) InFlight() int { return int(q.admitted.Load()) }

// Close stops the queue and waits for the consumer.
//
// A command already on the wire is allowed to finish: abandoning it would leave
// the device mid-dialogue and the result unknown, which is the one state this
// package exists to avoid producing carelessly. Commands still queued are
// reported as NotSent, which is true and is safe to retry elsewhere.
func (q *Queue) Close() error {
	q.closeOnce.Do(func() { close(q.closed) })
	q.wg.Wait()
	return nil
}

// consume is the single consumer. Nothing else runs a dialogue.
func (q *Queue) consume() {
	defer q.wg.Done()

	for {
		select {
		case <-q.closed:
			q.drain()
			return

		case j := <-q.jobs:
			// Both cases can be ready at once, and Go picks between them at
			// random. Without this second look, whether a command queued at
			// shutdown gets SENT or reported NotSent would be decided by a coin
			// toss — on a queue that drives breakers, and in a way no test
			// could pin down because it would pass most of the time.
			//
			// So closing wins: once Close has been observed, no new dialogue
			// starts. The one already on the wire is a different matter and
			// finishes in run.
			select {
			case <-q.closed:
				q.admitted.Add(-1)
				j.done <- Result{
					Outcome: OutcomeNotSent,
					Code:    ErrCodeShuttingDown,
					Err:     ErrClosed,
				}
				q.drain()
				return
			default:
			}
			q.run(j)
		}
	}
}

// run executes one command. It is the only place a dialogue is called.
func (q *Queue) run(j job) {
	defer q.admitted.Add(-1)

	// The last chance to not send. A caller that has already given up gets
	// nothing sent on its behalf: the field seeing a command whose requester
	// stopped waiting is how a breaker moves with nobody watching.
	if err := j.ctx.Err(); err != nil {
		j.done <- Result{Outcome: OutcomeNotSent, Code: ErrCodeCallerGone, Err: err}
		return
	}

	j.done <- q.cfg.Run(j.ctx, j.req)
}

// drain answers everything still queued. Nothing here reached the wire.
func (q *Queue) drain() {
	for {
		select {
		case j := <-q.jobs:
			q.admitted.Add(-1)
			j.done <- Result{
				Outcome: OutcomeNotSent,
				Code:    ErrCodeShuttingDown,
				Err:     ErrClosed,
			}
		default:
			return
		}
	}
}

// contractErrorCodes is every code this service can put on the wire.
//
// It is a list rather than a derived set on purpose: adding a code means
// deciding whether SendCommandResponse.error_code in the contract needs the same
// addition, and a list you have to edit is what makes that decision happen.
// TestEveryErrorCodeIsInTheContract is the guard.
var contractErrorCodes = []ErrorCode{
	// In the contract's own list.
	ErrCodeSelectRejected,
	ErrCodeOperateRejected,
	ErrCodeActConTimeout,
	ErrCodeActTermTimeout,
	ErrCodeStationDisconnected,
	ErrCodeInvalidValue,
	ErrCodeQueueFull,

	// Added to the draft by T8.4. A select that was never answered is not the
	// same event as an activation that was never answered — one leaves the
	// field untouched and the other may not — and folding them into one code
	// would throw that away at exactly the layer where it matters.
	ErrCodeSelectTimeout,

	// Also added: these two are this service's own ways of not sending, and an
	// operator looking at a command that never went out needs to know whether
	// the caller gave up or the process was shutting down.
	ErrCodeCallerGone,
	ErrCodeShuttingDown,
}
