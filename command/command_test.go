package command

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// --- the seam -----------------------------------------------------------------

// gate is a dialogue that a test drives by hand: it announces that it has
// started, then waits to be released. It is how overlap is made visible — two
// commands running at once show up as two starts before any release.
type gate struct {
	mu sync.Mutex
	// order is the label of every command the dialogue ran, in the order it ran
	// them.
	order []string
	// live is how many dialogues are inside the call right now, and peak is the
	// largest that ever got to. peak > 1 is the bug this package exists to
	// prevent.
	live, peak int

	started chan string
	release chan struct{}
}

func newGate() *gate {
	return &gate{
		started: make(chan string, 64),
		release: make(chan struct{}),
	}
}

func (g *gate) run(ctx context.Context, req Request) Result {
	g.mu.Lock()
	g.live++
	if g.live > g.peak {
		g.peak = g.live
	}
	g.order = append(g.order, req.Label)
	g.mu.Unlock()

	g.started <- req.Label

	select {
	case <-g.release:
	case <-ctx.Done():
	}

	g.mu.Lock()
	g.live--
	g.mu.Unlock()

	return Result{Outcome: OutcomeSucceeded}
}

func (g *gate) snapshot() (order []string, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.order...), g.peak
}

// waitStarted blocks until n dialogues have begun.
func (g *gate) waitStarted(t *testing.T, n int) {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for range n {
		select {
		case <-g.started:
		case <-timeout.C:
			t.Fatalf("only %d of %d commands started", len(g.started), n)
		}
	}
}

// immediate is a dialogue that finishes at once.
func immediate(Outcome) Dialogue {
	return func(context.Context, Request) Result { return Result{Outcome: OutcomeSucceeded} }
}

// --- configuration -------------------------------------------------------------

func TestNewRejectsAQueueWithoutADialogue(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{}); !errors.Is(err, ErrConfig) {
		t.Errorf("New with no dialogue: %v, want ErrConfig", err)
	}
	if _, err := New(Config{Depth: -1, Run: immediate(OutcomeSucceeded)}); !errors.Is(err, ErrConfig) {
		t.Errorf("New with a negative depth: %v, want ErrConfig", err)
	}
}

func TestTheDefaultDepthApplies(t *testing.T) {
	t.Parallel()

	cfg := Config{Run: immediate(OutcomeSucceeded)}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if cfg.Depth != DefaultDepth {
		t.Errorf("depth %d, want the default %d", cfg.Depth, DefaultDepth)
	}
}

// --- serialisation: the whole reason this package exists -----------------------

// Ten commands at once to one station must run one at a time. IEC 104 puts no
// correlation identifier on a command, so two in flight at once cannot be told
// apart by their confirmations — and the failure is not a crash, it is an
// operator being told the wrong breaker moved.
func TestCommandsToOneStationNeverOverlap(t *testing.T) {
	t.Parallel()

	const commands = 10

	g := newGate()
	q, err := New(Config{Depth: commands, Run: g.run})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	var wg sync.WaitGroup
	for i := range commands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = q.Send(t.Context(), Request{Label: fmt.Sprintf("cmd-%02d", i)})
		}()
	}

	// Let them through one at a time, checking after each that only one was
	// ever inside the dialogue.
	go func() {
		for range commands {
			g.release <- struct{}{}
		}
	}()

	wg.Wait()

	order, peak := g.snapshot()
	if peak != 1 {
		t.Errorf("%d dialogues ran at once; commands to one station must never overlap, "+
			"because 104 confirmations carry nothing that says which request they answer", peak)
	}
	if len(order) != commands {
		t.Errorf("%d commands ran, want %d", len(order), commands)
	}
}

// The limit counts the executing command too, so a depth of four refuses the
// fifth caller rather than the sixth. Counting only the waiting ones would make
// the configured limit quietly one larger than it reads.
func TestTheFifthCommandIsRefusedAtADepthOfFour(t *testing.T) {
	t.Parallel()

	const depth = 4

	g := newGate()
	q, err := New(Config{Depth: depth, Run: g.run})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		close(g.release)
		_ = q.Close()
	})

	// One command occupies the dialogue; three more fill the queue behind it.
	accepted := make(chan struct{}, depth)
	for i := range depth {
		go func() {
			accepted <- struct{}{}
			_, _ = q.Send(t.Context(), Request{Label: fmt.Sprintf("held-%d", i)})
		}()
	}
	for range depth {
		<-accepted
	}
	g.waitStarted(t, 1)

	// Wait for the queue to actually be full rather than assuming the four
	// goroutines above have got that far.
	waitInFlight(t, q, depth)

	// Refusing has to be IMMEDIATE, not eventual. A caller holding a gRPC
	// deadline open behind four other commands learns nothing useful from
	// waiting, so this runs the send in a goroutine and fails loudly if it
	// blocks — otherwise a queue that quietly waited for a slot would show up
	// as a hung test rather than a wrong one.
	type sent struct {
		res Result
		err error
	}
	refused := make(chan sent, 1)
	go func() {
		res, err := q.Send(t.Context(), Request{Label: "one-too-many"})
		refused <- sent{res, err}
	}()

	blocked := time.NewTimer(2 * time.Second)
	defer blocked.Stop()

	var got sent
	select {
	case got = <-refused:
	case <-blocked.C:
		t.Fatalf("the %dth command is still waiting for a slot; a full queue must "+
			"refuse rather than block", depth+1)
	}

	res, err := got.res, got.err
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("the %dth command returned %v, want ErrQueueFull", depth+1, err)
	}
	if res.Outcome != OutcomeNotSent {
		t.Errorf("a refused command reported %s, want %s; it never reached the wire and "+
			"the caller needs to know that retrying is safe", res.Outcome, OutcomeNotSent)
	}
	if res.Code != ErrCodeQueueFull {
		t.Errorf("code %q, want %q", res.Code, ErrCodeQueueFull)
	}
}

// Order matters as much as exclusion: an operator who sends open then close
// expects them in that order.
func TestCommandsRunInTheOrderTheyWereAccepted(t *testing.T) {
	t.Parallel()

	const commands = 6

	g := newGate()
	q, err := New(Config{Depth: commands, Run: g.run})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	// Sent one at a time and each confirmed before the next, so the expected
	// order is unambiguous — a concurrent submission would be testing the
	// scheduler rather than the queue.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range commands {
			g.release <- struct{}{}
			_ = i
		}
	}()

	for i := range commands {
		if _, err := q.Send(t.Context(), Request{Label: fmt.Sprintf("cmd-%d", i)}); err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	<-done

	order, _ := g.snapshot()
	for i, label := range order {
		if want := fmt.Sprintf("cmd-%d", i); label != want {
			t.Errorf("position %d ran %q, want %q; the full order was %v", i, label, want, order)
		}
	}
}

// --- a caller that gives up ----------------------------------------------------

// A queued command whose caller has stopped waiting must never be sent. The
// field seeing a command whose requester walked away is how a breaker moves
// with nobody watching for the result.
func TestAQueuedCommandIsNotSentAfterItsCallerGivesUp(t *testing.T) {
	t.Parallel()

	g := newGate()
	q, err := New(Config{Depth: 4, Run: g.run})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		close(g.release)
		_ = q.Close()
	})

	// Occupy the dialogue so the next command has to queue.
	blocker := make(chan struct{})
	go func() {
		_, _ = q.Send(t.Context(), Request{Label: "blocker"})
		close(blocker)
	}()
	g.waitStarted(t, 1)

	// Queue a command and then give up on it.
	ctx, cancel := context.WithCancel(t.Context())
	queued := make(chan Result, 1)
	go func() {
		res, _ := q.Send(ctx, Request{Label: "abandoned"})
		queued <- res
	}()
	waitInFlight(t, q, 2)
	cancel()

	res := <-queued
	if res.Outcome != OutcomeNotSent {
		t.Errorf("an abandoned command reported %s, want %s", res.Outcome, OutcomeNotSent)
	}

	// Release the blocker and let the consumer reach the abandoned job.
	g.release <- struct{}{}
	<-blocker
	waitInFlight(t, q, 0)

	order, _ := g.snapshot()
	for _, label := range order {
		if label == "abandoned" {
			t.Fatalf("the abandoned command was sent anyway; the dialogue ran %v", order)
		}
	}
}

// --- shutdown ------------------------------------------------------------------

func TestCloseReportsQueuedCommandsAsNotSent(t *testing.T) {
	t.Parallel()

	g := newGate()
	q, err := New(Config{Depth: 4, Run: g.run})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	blocker := make(chan struct{})
	go func() {
		_, _ = q.Send(t.Context(), Request{Label: "blocker"})
		close(blocker)
	}()
	g.waitStarted(t, 1)

	queued := make(chan Result, 1)
	go func() {
		res, _ := q.Send(t.Context(), Request{Label: "still-waiting"})
		queued <- res
	}()
	waitInFlight(t, q, 2)

	// Close while one is on the wire and one is waiting.
	closed := make(chan struct{})
	go func() {
		_ = q.Close()
		close(closed)
	}()

	// Close signals immediately and then waits for the consumer, so the signal
	// has to be up before the blocker is released — otherwise the consumer
	// finishes the blocker, loops, and simply runs the queued command before it
	// ever notices that the queue is closing. This test is in-package precisely
	// so it can wait for that signal rather than guess at it.
	<-q.closed

	// The consumer is inside the blocker's dialogue, so nothing can be drained
	// until that returns. Releasing here is not weakening the test: the queued
	// command is reached only after the consumer comes back and sees the close.
	close(g.release)
	<-blocker

	res := <-queued
	if res.Outcome != OutcomeNotSent {
		t.Errorf("a command queued at shutdown reported %s, want %s; it never reached "+
			"the wire, and saying otherwise would stop a caller retrying somewhere else",
			res.Outcome, OutcomeNotSent)
	}
	if res.Code != ErrCodeShuttingDown {
		t.Errorf("code %q, want %q", res.Code, ErrCodeShuttingDown)
	}

	<-closed

	if _, err := q.Send(t.Context(), Request{Label: "after-close"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after Close: %v, want ErrClosed", err)
	}
}

func TestCloseIsIdempotentAndLeavesNoGoroutines(t *testing.T) {
	// Not parallel: it counts goroutines process-wide.

	before := settle(0)

	q, err := New(Config{Run: immediate(OutcomeSucceeded)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := q.Send(t.Context(), Request{Label: "one"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for range 3 {
		if err := q.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}

	if after := settle(before); after > before+2 {
		t.Errorf("goroutines went from %d to %d after Close", before, after)
	}
}

// --- the safety vocabulary ------------------------------------------------------

// Outcome.Safe is the single place that answers "may this be sent again".
//
// The dangerous case is the one that looks most like a transient failure:
// Indeterminate means the command MAY have driven the output, so a retry is a
// second operation on a live breaker. If this table is ever wrong, it is wrong
// in a way no other test would notice.
func TestOnlyOutcomesThatNeverReachedTheFieldAreSafeToRetry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		outcome Outcome
		safe    bool
		why     string
	}{
		{OutcomeNotSent, true, "nothing went out, so the field saw nothing"},
		{OutcomeRejected, true, "the device answered and refused; the output did not move"},
		{OutcomeSucceeded, false, "it already worked; sending again is a second operation"},
		{OutcomeIndeterminate, false, "it MAY have driven the output — this is the whole point"},
	} {
		if got := tc.outcome.Safe(); got != tc.safe {
			t.Errorf("%s.Safe() = %t, want %t: %s", tc.outcome, got, tc.safe, tc.why)
		}
	}
}

// --- helpers --------------------------------------------------------------------

func waitInFlight(t *testing.T, q *Queue, want int) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for q.InFlight() != want {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("%d commands in flight, want %d", q.InFlight(), want)
		}
	}
}

// settle waits for goroutines that have been told to stop to leave the runtime's
// count. Same argument as internal/conn's version: there is no event to wait on
// for "the scheduler has reaped a goroutine", the retry exits early once the
// count has come down, and a real leak cannot be waited away.
func settle(target int) int {
	n := runtime.NumGoroutine()
	for range 50 {
		if n <= target {
			return n
		}
		runtime.Gosched()
		//nolint:forbidigo // a retry bound, not a synchronisation step.
		time.Sleep(2 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}
