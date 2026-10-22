package station

import (
	"testing"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
)

// The filter takes the arrival instant as a parameter, like apci.Timers, so
// these tests are tables of instants rather than choreography with a clock.
var chatterBase = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// contact is an untimestamped single-point change at ioa.
func contact(ioa asdu.IOA) Reading {
	return Reading{Point: asdu.Point{
		Type:  asdu.MSpNa1,
		IOA:   ioa,
		Kind:  asdu.KindBool,
		Cause: asdu.CauseSpontaneous,
	}}
}

// stamped is the same change carrying a device timestamp of deviceMillis.
func stamped(ioa asdu.IOA, deviceMillis int64) Reading {
	r := contact(ioa)
	r.Point.Type = asdu.MSpTb1
	r.TSValid = true
	r.TSUTCMillis = deviceMillis
	return r
}

// change is one observation: which reading, and when it arrived.
type change struct {
	r  Reading
	at time.Duration // after chatterBase
}

// flagged runs changes through a fresh default filter and returns the verdicts.
func flagged(changes []change) []bool {
	f := newChatterFilter(DefaultChatter(), chatterBase)
	out := make([]bool, len(changes))
	for i, c := range changes {
		out[i] = f.observe(c.r, chatterBase.Add(c.at))
	}
	return out
}

// evenly is n changes of one untimestamped contact, every apart.
func evenly(ioa asdu.IOA, n int, every time.Duration) []change {
	out := make([]change, n)
	for i := range out {
		out[i] = change{contact(ioa), time.Duration(i) * every}
	}
	return out
}

// firstFlagged is the index of the first flagged verdict, or -1.
func firstFlagged(v []bool) int {
	for i, f := range v {
		if f {
			return i
		}
	}
	return -1
}

// expectFlagsFrom asserts every verdict before from is false and every one from
// it on is true. from == len(v) means none.
func expectFlagsFrom(t *testing.T, v []bool, from int) {
	t.Helper()
	for i, got := range v {
		if want := i >= from; got != want {
			t.Errorf("change %d: flagged = %v, want %v (verdicts %v)", i+1, got, want, v)
			return
		}
	}
}

// --- the threshold ----------------------------------------------------------

// The acceptance test of T7.2: twenty changes in ten seconds, and every change
// past the fifth carries the flag. The second case runs long enough for the
// quiet-contact sweep to run twice, and the contact must survive both.
func TestChatterFlagsChangesBeyondTheThreshold(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		n     int
		every time.Duration
	}{
		{"twenty in ten seconds", 20, 500 * time.Millisecond},
		{"one a second for half a minute", 30, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expectFlagsFrom(t, flagged(evenly(1, tc.n, tc.every)), DefaultChatterMaxChanges)
		})
	}
}

// Five changes is the policy's allowance, not a violation of it.
func TestChatterAllowsExactlyTheThreshold(t *testing.T) {
	t.Parallel()
	expectFlagsFrom(t, flagged(evenly(1, DefaultChatterMaxChanges, 10*time.Millisecond)), DefaultChatterMaxChanges)
}

// "Within the window" is strict: six changes spanning exactly the window are not
// chatter, six spanning just under it are.
func TestChatterWindowBoundary(t *testing.T) {
	t.Parallel()

	span := func(total time.Duration) []change {
		return evenly(1, DefaultChatterMaxChanges+1, total/time.Duration(DefaultChatterMaxChanges))
	}

	if got := flagged(span(DefaultChatterWindow)); firstFlagged(got) != -1 {
		t.Errorf("six changes spanning exactly the window were flagged: %v", got)
	}
	if got := flagged(span(DefaultChatterWindow - 5*time.Millisecond)); firstFlagged(got) != DefaultChatterMaxChanges {
		t.Errorf("six changes spanning just under the window: verdicts %v, want the sixth flagged", got)
	}
}

// Once a contact calms down it is reported plainly again. The window slides, it
// is not a sentence.
func TestChatterClearsOnceTheWindowSlides(t *testing.T) {
	t.Parallel()

	changes := evenly(1, 6, 100*time.Millisecond) // the sixth is chatter
	changes = append(changes,
		change{contact(1), 10200 * time.Millisecond}, // oldest held is now 10.1 s back
		change{contact(1), 13 * time.Second},
		change{contact(1), 16 * time.Second},
	)

	want := []bool{false, false, false, false, false, true, false, false, false}
	got := flagged(changes)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("verdicts %v, want %v", got, want)
		}
	}
}

// Two contacts each within the allowance are not one contact over it.
func TestChatterIsCountedPerContact(t *testing.T) {
	t.Parallel()

	var changes []change
	for i := 0; i < DefaultChatterMaxChanges; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		changes = append(changes, change{contact(1), at}, change{contact(2), at + time.Millisecond})
	}

	if got := flagged(changes); firstFlagged(got) != -1 {
		t.Errorf("two contacts at five changes each were flagged: %v", got)
	}
}

// --- which clock ------------------------------------------------------------

// After an outage an RTU empties its buffer in one burst. Measured by arrival,
// changes an hour apart all look like chatter; measured by the device, they are
// what they were. This is the reason the device's clock is preferred.
func TestChatterMeasuresTimestampedChangesByTheDevice(t *testing.T) {
	t.Parallel()

	device := chatterBase.UnixMilli()

	var backlog, unstamped []change
	for i := 0; i < 10; i++ {
		arrival := time.Duration(i) * time.Millisecond // all within ten ms
		backlog = append(backlog, change{stamped(1, device+int64(i)*time.Minute.Milliseconds()), arrival})
		unstamped = append(unstamped, change{contact(1), arrival})
	}

	if got := flagged(backlog); firstFlagged(got) != -1 {
		t.Errorf("a buffered burst of changes a minute apart was flagged as chatter: %v", got)
	}
	// The same burst without timestamps has only our clock to go on, and by
	// our clock it IS ten changes in ten milliseconds.
	expectFlagsFrom(t, flagged(unstamped), DefaultChatterMaxChanges)
}

// A device whose clock is stepped back — synchronised by another master, say —
// produces an instant earlier than the ones it sent before. The difference is
// negative, which is "within the window" to a naive comparison.
func TestChatterResetsWhenTheDeviceClockStepsBack(t *testing.T) {
	t.Parallel()

	device := chatterBase.UnixMilli()
	hour := time.Hour.Milliseconds()

	var changes []change
	for i := 0; i < DefaultChatterMaxChanges; i++ {
		changes = append(changes, change{stamped(1, device+int64(i)*1000), time.Duration(i) * time.Second})
	}
	// The clock steps back an hour, then the contact goes on changing once a
	// second: the fresh history fills and the sixth change after the step is
	// chatter again.
	for i := 0; i < DefaultChatterMaxChanges+1; i++ {
		changes = append(changes,
			change{stamped(1, device-hour+int64(i)*1000), time.Duration(DefaultChatterMaxChanges+i) * time.Second})
	}

	// Without the reset the change right after the step (index 5) is flagged.
	expectFlagsFrom(t, flagged(changes), 2*DefaultChatterMaxChanges)
}

// Two clocks are never compared. A real CP56Time2a is epoch milliseconds from
// the year 2000 on and could not be this small, so the step-back rule would
// usually catch a mix anyway; the rule is stated on clocks, not on magnitudes,
// and this case keeps it that way.
func TestChatterNeverComparesTheTwoClocks(t *testing.T) {
	t.Parallel()

	changes := evenly(1, DefaultChatterMaxChanges, 100*time.Millisecond) // arrival 0..400 ms
	changes = append(changes, change{stamped(1, 500), 500 * time.Millisecond})

	if got := flagged(changes); firstFlagged(got) != -1 {
		t.Errorf("an arrival-timed history was compared with a device instant: %v", got)
	}
}

// --- memory -----------------------------------------------------------------

// The map holds contacts that changed recently, not every contact that ever
// did. A device reporting addresses it does not have must not grow it forever.
func TestChatterForgetsQuietContacts(t *testing.T) {
	t.Parallel()

	f := newChatterFilter(DefaultChatter(), chatterBase)
	for ioa := asdu.IOA(1); ioa <= 1000; ioa++ {
		f.observe(contact(ioa), chatterBase)
	}
	f.observe(contact(5000), chatterBase.Add(9*time.Second)) // still recent at the sweep

	f.observe(contact(6000), chatterBase.Add(DefaultChatterWindow))

	if got := len(f.points); got != 2 {
		t.Errorf("%d contacts held after a quiet window, want the 2 that changed within it", got)
	}
	for _, ioa := range []asdu.IOA{5000, 6000} {
		if _, ok := f.points[ioa]; !ok {
			t.Errorf("contact %d was forgotten while it was still changing", ioa)
		}
	}
}

// Every contact holds at most MaxChanges+1 instants, however long it bounces.
func TestChatterHistoryIsBounded(t *testing.T) {
	t.Parallel()

	f := newChatterFilter(DefaultChatter(), chatterBase)
	for i := 0; i < 10_000; i++ {
		f.observe(contact(1), chatterBase.Add(time.Duration(i)*time.Millisecond))
	}
	if got := len(f.points[1].instant); got != DefaultChatterMaxChanges+1 {
		t.Errorf("%d instants held, want %d", got, DefaultChatterMaxChanges+1)
	}
}

// --- policy -----------------------------------------------------------------

func TestChatterValidateRejectsWhatCannotBeBounded(t *testing.T) {
	t.Parallel()

	if err := DefaultChatter().Validate(); err != nil {
		t.Fatalf("the default policy is rejected: %v", err)
	}
	for name, c := range map[string]Chatter{
		"only a window":            {Window: DefaultChatterWindow},
		"only a threshold":         {MaxChanges: DefaultChatterMaxChanges},
		"threshold beyond bound":   {MaxChanges: MaxChatterMaxChanges + 1, Window: DefaultChatterWindow},
		"window under the minimum": {MaxChanges: 5, Window: MinChatterWindow - time.Millisecond},
		"window over the maximum":  {MaxChanges: 5, Window: MaxChatterWindow + time.Second},
		"negative threshold":       {MaxChanges: -1, Window: DefaultChatterWindow},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: %+v was accepted", name, c)
		}
	}
}

// A station that says nothing about chatter gets the contract's defaults, not a
// policy of zero that would flag everything or nothing.
func TestAStationWithNoChatterPolicyGetsTheDefault(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Address:       "127.0.0.1:2404",
		Params:        apci.DefaultParams(),
		ASDUParams:    asdu.DefaultParams(),
		CommonAddress: testCASDU,
		Clock:         clock.System(),
		Observer:      newRecorder(),
	}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if cfg.Chatter != DefaultChatter() {
		t.Errorf("chatter policy = %+v, want %+v", cfg.Chatter, DefaultChatter())
	}
}

// --- through a live station -------------------------------------------------

// A bouncing contact is flagged and still published; a measurand reporting just
// as often is neither flagged nor dropped.
func TestABouncingContactIsFlaggedAndStillPublished(t *testing.T) {
	t.Parallel()

	const (
		bouncing asdu.IOA = 400
		busy     asdu.IOA = 401
		changes           = DefaultChatterMaxChanges + 3
	)

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	for i := 0; i < changes; i++ {
		body, err := singlePoint(bouncing, i%2 == 0)
		if err != nil {
			t.Fatalf("encoding M_SP_NA_1: %v", err)
		}
		o.sendRaw(body)
		o.sendMeasurand(busy, float32(i))
	}
	o.sendMeasurand(barrierIOA, 0)
	rec.waitForEvent(t, barrierIOA)

	var contactFlags, measurandFlags []bool
	for _, e := range rec.eventReports() {
		switch e.Point.IOA {
		case bouncing:
			contactFlags = append(contactFlags, e.ChatterSuppressed)
		case busy:
			measurandFlags = append(measurandFlags, e.ChatterSuppressed)
		}
	}

	if len(contactFlags) != changes {
		t.Fatalf("%d events for the bouncing contact, want all %d — flagged events are still published",
			len(contactFlags), changes)
	}
	expectFlagsFrom(t, contactFlags, DefaultChatterMaxChanges)

	if len(measurandFlags) != changes {
		t.Fatalf("%d events for the measurand, want %d", len(measurandFlags), changes)
	}
	if i := firstFlagged(measurandFlags); i != -1 {
		t.Errorf("a measurand was flagged as chatter at change %d: only contacts chatter", i+1)
	}
}

// singlePoint builds an M_SP_NA_1 with COT=spontaneous: one SIQ octet.
func singlePoint(ioa asdu.IOA, on bool) ([]byte, error) {
	p := asdu.DefaultParams()
	h := asdu.Header{
		Type:       asdu.MSpNa1,
		Count:      1,
		Cause:      asdu.CauseSpontaneous,
		CommonAddr: testCASDU,
	}
	out, err := h.AppendTo(nil, p)
	if err != nil {
		return nil, err
	}
	var siq byte
	if on {
		siq = 0x01 // SPI
	}
	return p.AppendElements(out, h, []asdu.Element{{IOA: ioa, Data: []byte{siq}}})
}
