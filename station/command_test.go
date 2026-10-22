package station

import (
	"errors"
	"testing"
	"time"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/command"
)

// internal/command tests the dialogue against a fake wire. These tests run the
// same dialogue through a real socket and real frames, because the two halves
// meet in places a fake cannot reach: whether the reply this service parses out
// of a device's echo is the one the dialogue is waiting for, and whether a
// command refused because the station is disconnected is refused before
// anything is handed to a writer.

func commandConfig(t *testing.T, addr string, obs Observer) Config {
	t.Helper()

	cfg := testConfig(t, addr, obs)
	cfg.Commands = Commands{
		Enabled:        true,
		ActConTimeout:  2 * time.Second,
		ActTermTimeout: 4 * time.Second,
		WaitForActTerm: true,
	}
	return cfg
}

func singleCommand(ioa asdu.IOA, on bool) asdu.Command {
	return asdu.Command{Type: asdu.CScNa1, IOA: ioa, On: on}
}

func directOperate(ioa asdu.IOA, on bool) CommandRequest {
	return CommandRequest{Cmd: singleCommand(ioa, on)}
}

func selectAndOperate(ioa asdu.IOA, on bool) CommandRequest {
	return CommandRequest{Cmd: singleCommand(ioa, on), SelectBeforeOperate: true}
}

// The whole path: a command goes out over a socket, the device answers with the
// bracket, and the station reports success.
func TestACommandTravelsTheWholePathAndSucceeds(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(commandConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeSucceeded {
		t.Fatalf("outcome %s (%s): %v", res.Outcome, res.Code, res.Err)
	}

	got := o.commands()
	if len(got) != 1 {
		t.Fatalf("the device received %d commands, want 1", len(got))
	}
	if got[0].Type != asdu.CScNa1 || got[0].IOA != 4711 || !got[0].On {
		t.Errorf("the device received %s ioa=%d on=%t, want C_SC_NA_1 ioa=4711 on=true",
			got[0].Type, got[0].IOA, got[0].On)
	}
	if got[0].Cause != asdu.CauseActivation {
		t.Errorf("cause %s, want %s; a command is an activation", got[0].Cause, asdu.CauseActivation)
	}
}

// A device that refuses. The output did not move and it said so.
func TestARefusedCommandComesBackRejected(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.cmdRefuse = true
	rec := newRecorder()

	s, err := New(commandConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeRejected {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, command.OutcomeRejected, res.Err)
	}
	if !res.Negative {
		t.Error("the P/N bit did not survive the trip through a real frame")
	}
}

// A device that confirms and never terminates. The station cannot know whether
// the output finished moving, and says so.
func TestACommandWithNoTerminationIsIndeterminate(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.cmdNoActTerm = true
	rec := newRecorder()

	cfg := commandConfig(t, o.addr(), rec)
	cfg.Commands.ActTermTimeout = 300 * time.Millisecond

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeIndeterminate {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, command.OutcomeIndeterminate, res.Err)
	}
	if res.Outcome.Safe() {
		t.Error("the station offered to let a caller retry a command the device accepted")
	}
}

// The same device, once it is configured correctly. This is the pair that makes
// the per-device flag worth having: the device did nothing different.
func TestTheSameDeviceSucceedsOnceItIsKnownNotToTerminate(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.cmdNoActTerm = true
	rec := newRecorder()

	cfg := commandConfig(t, o.addr(), rec)
	cfg.Commands.WaitForActTerm = false

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeSucceeded {
		t.Fatalf("outcome %s (%s): %v", res.Outcome, res.Code, res.Err)
	}
}

// A device that says nothing at all.
func TestASilentDeviceLeavesTheCommandIndeterminate(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.cmdSilent = true
	rec := newRecorder()

	cfg := commandConfig(t, o.addr(), rec)
	cfg.Commands.ActConTimeout = 300 * time.Millisecond

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeIndeterminate {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, command.OutcomeIndeterminate, res.Err)
	}
	if res.SentAt.IsZero() {
		t.Error("no send timestamp; this is the outcome where knowing exactly when the " +
			"command went out is the only thing an investigation has to go on")
	}
}

// --- refusals that never reach the wire -------------------------------------------

// A station configured for reading only must not be able to drive anything, and
// the cheapest guarantee of that is that the queue does not exist.
func TestAStationWithoutCommandsRefusesThem(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	// testConfig leaves Commands disabled, which is the default.
	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if !errors.Is(err, ErrCommandsDisabled) {
		t.Fatalf("SendCommand: %v, want ErrCommandsDisabled", err)
	}
	if res.Outcome != command.OutcomeNotSent {
		t.Errorf("outcome %s, want %s", res.Outcome, command.OutcomeNotSent)
	}
	if n := len(o.commands()); n != 0 {
		t.Errorf("%d commands reached a station that takes none", n)
	}
}

// A command sent while the station is not connected is refused before anything
// is handed to a writer, so it is unambiguously NotSent.
func TestACommandToADisconnectedStationIsNotSent(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	cfg := commandConfig(t, "127.0.0.1:1", rec) // nothing listens there
	cfg.Backoff = Backoff{Base: time.Hour, Cap: time.Hour, Factor: 2}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateFailed)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeNotSent {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, command.OutcomeNotSent, res.Err)
	}
	if res.Code != command.ErrCodeStationDisconnected {
		t.Errorf("code %q, want %q", res.Code, command.ErrCodeStationDisconnected)
	}
	if !res.Outcome.Safe() {
		t.Error("nothing was sent, so this is exactly the case a caller may retry")
	}
}

// Data transfer has to be started before anything is driven. A device that
// accepts the connection and never confirms STARTDT leaves the station holding
// a live socket it may not send on — and a command that slipped out there would
// be one the device is entitled to ignore, leaving the caller with a result
// that means nothing.
func TestACommandBeforeTheHandshakeCompletesNeverReachesTheDevice(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.noStartDTCon = true
	rec := newRecorder()

	cfg := commandConfig(t, o.addr(), rec)
	cfg.Backoff = Backoff{Base: time.Hour, Cap: time.Hour, Factor: 2}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateConnecting)

	res, err := s.SendCommand(t.Context(), directOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeNotSent {
		t.Errorf("outcome %s, want %s: %v", res.Outcome, command.OutcomeNotSent, res.Err)
	}
	if n := len(o.commands()); n != 0 {
		t.Errorf("%d commands reached a device that never confirmed STARTDT", n)
	}
}

// --- serialisation, end to end -----------------------------------------------------

// The queue's guarantee has to survive the trip through a real connection: two
// commands to one station must not be on the wire at the same time, because
// their confirmations would be indistinguishable.
func TestTwoCommandsToOneStationDoNotOverlapOnTheWire(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.cmdHold = make(chan struct{})
	rec := newRecorder()

	s, err := New(commandConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	first := make(chan command.Result, 1)
	go func() {
		res, _ := s.SendCommand(t.Context(), directOperate(1, true))
		first <- res
	}()

	// Wait for the device to be holding the first command.
	waitCommands(t, o, 1)

	second := make(chan command.Result, 1)
	go func() {
		res, _ := s.SendCommand(t.Context(), directOperate(2, true))
		second <- res
	}()

	// The second must not reach the device while the first is unanswered. There
	// is no event for "nothing happened", so this is a window rather than a
	// wait — but a queue that let it through would let it through at once.
	settleForNegative()
	if n := len(o.commands()); n != 1 {
		t.Fatalf("the device has %d commands at once; their confirmations would be "+
			"indistinguishable", n)
	}

	o.cmdHold <- struct{}{} // let the first finish
	if res := <-first; res.Outcome != command.OutcomeSucceeded {
		t.Errorf("first command: %s (%s): %v", res.Outcome, res.Code, res.Err)
	}

	o.cmdHold <- struct{}{} // and the second
	if res := <-second; res.Outcome != command.OutcomeSucceeded {
		t.Errorf("second command: %s (%s): %v", res.Outcome, res.Code, res.Err)
	}

	got := o.commands()
	if len(got) != 2 || got[0].IOA != 1 || got[1].IOA != 2 {
		t.Errorf("the device saw %v, want ioa 1 then ioa 2", got)
	}
}

// --- helpers -------------------------------------------------------------------------

func waitCommands(t *testing.T, o *outstation, n int) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for len(o.commands()) < n {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("the device received %d commands, want %d", len(o.commands()), n)
		}
	}
}

// --- select before operate, end to end ------------------------------------------

func TestSelectAndOperateTravelsTheWholePath(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(commandConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), selectAndOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeSucceeded {
		t.Fatalf("outcome %s (%s): %v", res.Outcome, res.Code, res.Err)
	}

	got := o.commands()
	if len(got) != 2 {
		t.Fatalf("the device received %d frames, want 2 (a selection and an execution): %v", len(got), got)
	}
	if !got[0].Selected {
		t.Error("the first frame did not carry the S/E bit; that is the selection")
	}
	if got[1].Selected {
		t.Error("the second frame still carried the S/E bit; the point was selected twice " +
			"and never executed")
	}
	if got[0].IOA != 4711 || got[1].IOA != 4711 {
		t.Errorf("the two halves went to different points: %d and %d", got[0].IOA, got[1].IOA)
	}
}

// The property the extra round trip buys: a refused selection means the
// execution is never sent, so nothing in the field moved and trying again is
// safe.
func TestARefusedSelectionNeverExecutesOnTheWire(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.cmdSelectRefuse = true
	rec := newRecorder()

	s, err := New(commandConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	res, err := s.SendCommand(t.Context(), selectAndOperate(4711, true))
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if res.Outcome != command.OutcomeNotSent {
		t.Fatalf("outcome %s, want %s: %v", res.Outcome, command.OutcomeNotSent, res.Err)
	}
	if res.Code != command.ErrCodeSelectRejected {
		t.Errorf("code %q, want %q", res.Code, command.ErrCodeSelectRejected)
	}
	if !res.Outcome.Safe() {
		t.Error("a refused selection leaves the field untouched; a caller may try again")
	}

	// Give an execution that must not exist time to arrive.
	settleForNegative()

	got := o.commands()
	if len(got) != 1 {
		t.Fatalf("the device received %d frames after refusing the selection, want 1: %v",
			len(got), got)
	}
	if !got[0].Selected {
		t.Error("the one frame that arrived was not the selection")
	}
}

// Select and execute have to be atomic: another command must not get between
// them, or it would be operating a point somebody else has reserved. The queue
// is what guarantees it, and this is the test that says so out loud.
func TestNoOtherCommandGetsBetweenSelectAndExecute(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	o.cmdHold = make(chan struct{})
	rec := newRecorder()

	s, err := New(commandConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	rec.waitFor(t, StateTopical)

	sbo := make(chan command.Result, 1)
	go func() {
		res, _ := s.SendCommand(t.Context(), selectAndOperate(4711, true))
		sbo <- res
	}()

	// The device is holding the selection.
	waitCommands(t, o, 1)

	// A second, unrelated command arrives now.
	other := make(chan command.Result, 1)
	go func() {
		res, _ := s.SendCommand(t.Context(), directOperate(999, true))
		other <- res
	}()

	o.cmdHold <- struct{}{} // release the selection's answer
	waitCommands(t, o, 2)

	// The second frame must be the execution of 4711, not the other command.
	if got := o.commands(); got[1].IOA != 4711 || got[1].Selected {
		t.Fatalf("the frame after the selection was %s ioa=%d select=%t; another command "+
			"got between a selection and its execution, which means operating a point "+
			"somebody else had reserved", got[1].Type, got[1].IOA, got[1].Selected)
	}

	o.cmdHold <- struct{}{} // the execution's answer
	if res := <-sbo; res.Outcome != command.OutcomeSucceeded {
		t.Errorf("select+operate: %s (%s): %v", res.Outcome, res.Code, res.Err)
	}

	o.cmdHold <- struct{}{} // and now the other command
	if res := <-other; res.Outcome != command.OutcomeSucceeded {
		t.Errorf("the queued command: %s (%s): %v", res.Outcome, res.Code, res.Err)
	}

	got := o.commands()
	if len(got) != 3 {
		t.Fatalf("the device received %d frames, want 3: %v", len(got), got)
	}
	if got[2].IOA != 999 {
		t.Errorf("the last frame went to ioa %d, want 999", got[2].IOA)
	}
}

// --- the audit and metric seam ----------------------------------------------------

// Every finished command is reported exactly once, whatever happened to it.
// iec104_command_total{outcome} and the audit trail (S9) both hang off this, and
// a path that quietly reports nothing is a command nobody can account for.
func TestEveryCommandIsReportedToTheObserver(t *testing.T) {
	t.Parallel()

	t.Run("one that succeeded", func(t *testing.T) {
		t.Parallel()

		o := newOutstation(t)
		rec := newRecorder()
		s, err := New(commandConfig(t, o.addr(), rec))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = s.Release() })
		rec.waitFor(t, StateTopical)

		if _, err := s.SendCommand(t.Context(), directOperate(4711, true)); err != nil {
			t.Fatalf("SendCommand: %v", err)
		}

		got := rec.commandReports()
		if len(got) != 1 {
			t.Fatalf("%d commands reported, want 1", len(got))
		}
		if got[0].res.Outcome != command.OutcomeSucceeded {
			t.Errorf("reported %s, want %s", got[0].res.Outcome, command.OutcomeSucceeded)
		}
		if got[0].cmd.IOA != 4711 {
			t.Errorf("reported ioa %d, want 4711", got[0].cmd.IOA)
		}
	})

	t.Run("one refused because commands are off", func(t *testing.T) {
		t.Parallel()

		o := newOutstation(t)
		rec := newRecorder()
		s, err := New(testConfig(t, o.addr(), rec)) // commands disabled
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = s.Release() })
		rec.waitFor(t, StateTopical)

		//nolint:errcheck // the refusal is the point; the result is what is asserted.
		_, _ = s.SendCommand(t.Context(), directOperate(4711, true))

		got := rec.commandReports()
		if len(got) != 1 {
			t.Fatalf("%d commands reported, want 1; a station being sent commands it "+
				"does not accept is a configuration mistake somebody has to see", len(got))
		}
		if got[0].res.Code != ErrCodeCommandsDisabled {
			t.Errorf("reported code %q, want %q", got[0].res.Code, ErrCodeCommandsDisabled)
		}
	})

	t.Run("one refused by a full queue", func(t *testing.T) {
		t.Parallel()

		o := newOutstation(t)
		o.cmdHold = make(chan struct{})
		rec := newRecorder()

		cfg := commandConfig(t, o.addr(), rec)
		cfg.Commands.Depth = 1

		s, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() {
			close(o.cmdHold)
			_ = s.Release()
		})
		rec.waitFor(t, StateTopical)

		held := make(chan struct{})
		go func() {
			defer close(held)
			//nolint:errcheck // this one is expected to succeed once released.
			_, _ = s.SendCommand(t.Context(), directOperate(1, true))
		}()
		waitCommands(t, o, 1)

		//nolint:errcheck // the refusal is the point.
		_, _ = s.SendCommand(t.Context(), directOperate(2, true))

		var full int
		for _, r := range rec.commandReports() {
			if r.res.Code == command.ErrCodeQueueFull {
				full++
			}
		}
		if full != 1 {
			t.Errorf("%d queue-full reports, want 1; a station being asked for more than "+
				"it can carry is exactly what the metric is for", full)
		}
	})
}
