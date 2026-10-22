package apci

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDefaultParamsAreValid(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	if err := p.Validate(); err != nil {
		t.Fatalf("the standard's own defaults must validate: %v", err)
	}
	if p.K != 12 || p.W != 8 {
		t.Errorf("k/w = %d/%d, want 12/8", p.K, p.W)
	}
	if p.T0 != 30*time.Second || p.T1 != 15*time.Second || p.T2 != 10*time.Second || p.T3 != 20*time.Second {
		t.Errorf("timers = %s/%s/%s/%s, want 30s/15s/10s/20s", p.T0, p.T1, p.T2, p.T3)
	}
}

func TestValidateRejectsUnworkableParams(t *testing.T) {
	t.Parallel()

	// Each case starts from the valid defaults and breaks exactly one thing, so
	// a failure names the rule under test.
	cases := []struct {
		name   string
		mutate func(*Params)
		// mustMention is a fragment the message has to contain, so that an
		// operator reading a startup failure learns which knob to turn.
		mustMention string
	}{
		{"k of zero", func(p *Params) { p.K = 0 }, "k=0"},
		{"k above the sequence space", func(p *Params) { p.K = MaxSeq + 1 }, "k="},
		{"w of zero", func(p *Params) { p.W = 0 }, "w=0"},

		// The rule that matters: w must stay at two thirds of k.
		{"w equal to k", func(p *Params) { p.W = p.K }, "2k/3"},
		{"w one above the two-thirds limit", func(p *Params) { p.K, p.W = 12, 9 }, "2k/3"},
		{"w above k", func(p *Params) { p.K, p.W = 4, 5 }, "2k/3"},

		{"t0 of zero", func(p *Params) { p.T0 = 0 }, "t0"},
		{"negative t1", func(p *Params) { p.T1 = -time.Second }, "t1"},
		{"t3 of zero", func(p *Params) { p.T3 = 0 }, "t3"},

		// The other silent-failure rule.
		{"t2 equal to t1", func(p *Params) { p.T2 = p.T1 }, "t2"},
		{"t2 above t1", func(p *Params) { p.T1, p.T2 = 10*time.Second, 11*time.Second }, "t2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := DefaultParams()
			tc.mutate(&p)

			err := p.Validate()
			if !errors.Is(err, ErrParams) {
				t.Fatalf("got %v, want ErrParams", err)
			}
			if !strings.Contains(err.Error(), tc.mustMention) {
				t.Errorf("message does not mention %q: %s", tc.mustMention, err)
			}
		})
	}
}

// The two-thirds rule is written as 3w <= 2k rather than w <= 2*k/3 so that
// integer division cannot round the limit up. k=12 is the interesting case:
// 2*12/3 is exactly 8, and a k where the division truncates must not become
// more permissive.
func TestTwoThirdsRuleIsExact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		k, w uint16
		ok   bool
	}{
		{12, 8, true},  // the standard's own pair, exactly at the limit
		{12, 9, false}, // one past
		{3, 2, true},   // 2*3/3 = 2
		{3, 3, false},
		{4, 2, true},  // 2*4/3 truncates to 2
		{4, 3, false}, // ...and 3 must still be refused
		{5, 3, true},  // 2*5/3 truncates to 3, and 3 is genuinely allowed: 3*3 <= 2*5
		{5, 4, false},
		{1, 1, false}, // 3 > 2: the smallest k cannot carry any w
		{2, 1, true},
	}

	for _, tc := range cases {
		verdict := "rejected"
		if tc.ok {
			verdict = "accepted"
		}
		t.Run(fmt.Sprintf("k=%d w=%d is %s", tc.k, tc.w, verdict), func(t *testing.T) {
			t.Parallel()

			p := DefaultParams()
			p.K, p.W = tc.k, tc.w

			err := p.Validate()
			if tc.ok && err != nil {
				t.Fatalf("k=%d w=%d must be accepted: %v", tc.k, tc.w, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("k=%d w=%d must be rejected", tc.k, tc.w)
			}
		})
	}
}

// --- the send window --------------------------------------------------------

// The acceptance criterion for T4.3: with k=12 the thirteenth frame cannot be
// sent, and an acknowledgement reopens the window.
func TestSendWindowClosesAtKAndReopensOnAck(t *testing.T) {
	t.Parallel()

	p := DefaultParams() // k = 12
	var s Sequencer

	for i := uint16(1); i <= p.K; i++ {
		if !p.CanSend(s.Outstanding()) {
			t.Fatalf("frame %d must be sendable with only %d outstanding", i, s.Outstanding())
		}
		s.NextSend()
	}

	if p.CanSend(s.Outstanding()) {
		t.Fatalf("the %dth frame must not be sendable with k=%d", p.K+1, p.K)
	}

	// One frame acknowledged, one slot free — no more.
	if _, err := s.Acknowledge(1); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if !p.CanSend(s.Outstanding()) {
		t.Error("the window must reopen when a frame is acknowledged")
	}
	s.NextSend()
	if p.CanSend(s.Outstanding()) {
		t.Error("the window must close again after using the freed slot")
	}

	// Acknowledging the rest empties it.
	if _, err := s.Acknowledge(s.SendSeq()); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if got := s.Outstanding(); got != 0 {
		t.Fatalf("outstanding = %d after acknowledging everything", got)
	}
	if !p.CanSend(s.Outstanding()) {
		t.Error("an empty window must allow sending")
	}
}

func TestCanSendBoundary(t *testing.T) {
	t.Parallel()

	p := Params{K: 1}
	if !p.CanSend(0) {
		t.Error("k=1 must allow the first frame")
	}
	if p.CanSend(1) {
		t.Error("k=1 must refuse the second frame")
	}
}

// --- the receive window -----------------------------------------------------

// The other half of the acceptance criterion: with w=8 the eighth received
// frame is what makes an acknowledgement due.
func TestAckBecomesDueAtW(t *testing.T) {
	t.Parallel()

	p := DefaultParams() // w = 8
	var s Sequencer

	for i := uint16(0); i < p.W-1; i++ {
		if err := s.Receive(i, 0); err != nil {
			t.Fatalf("Receive(%d): %v", i, err)
		}
		if p.AckDue(s.UnackedReceived()) {
			t.Fatalf("an acknowledgement must not be due after %d frames with w=%d", i+1, p.W)
		}
	}

	if err := s.Receive(p.W-1, 0); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !p.AckDue(s.UnackedReceived()) {
		t.Fatalf("an acknowledgement must be due after %d frames with w=%d", p.W, p.W)
	}

	// Sending the acknowledgement clears it.
	s.SendSupervisory()
	if p.AckDue(s.UnackedReceived()) {
		t.Error("the acknowledgement must not still be due after sending an S-frame")
	}
}

// An outgoing I-frame carries N(R), so on a two-way link the standalone
// acknowledgement is rarely needed at all.
func TestOutgoingTrafficKeepsTheAckFromComingDue(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	var s Sequencer

	for i := uint16(0); i < 3*p.W; i++ {
		if err := s.Receive(i, 0); err != nil {
			t.Fatalf("Receive(%d): %v", i, err)
		}
		s.NextSend() // reply, carrying N(R)
		if p.AckDue(s.UnackedReceived()) {
			t.Fatalf("frame %d: a standalone acknowledgement must never come due on a two-way link", i)
		}
	}
}
