package live

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
	"github.com/radaren989/iec104/command"
	"github.com/radaren989/iec104/station"
)

// The environment is the whole configuration surface. A device's address,
// common address and time zone are site facts, not repository facts, so none of
// them has a literal here.
// simAddr is the same setting as IEC104_SIM_ADDR, as a flag.
//
// It exists because setting an environment variable per run configuration is
// awkward in an IDE, and the shortcut people reach for instead — hard-coding the
// address in the source — quietly breaks `make check`: the skip below is the
// only reason `go test ./...` does not sit for ninety seconds trying to reach a
// device that is not there. One flag field costs nothing and keeps the skip.
//
//	go test ./test/live -run TestAgainstALiveDevice -v -sim 127.0.0.1:2404
//
// In GoLand: Run configuration -> Program arguments -> -sim 127.0.0.1:2404
var simAddr = flag.String("sim", "",
	"address of a live IEC 104 device, host:port; same as "+envAddr)

const (
	envAddr   = "IEC104_SIM_ADDR"   // "host:port", required; everything skips without it
	envCASDU  = "IEC104_SIM_CASDU"  // common address of ASDU, default 1
	envZone   = "IEC104_SIM_ZONE"   // IANA zone the device's clock is set to, default UTC
	envCycles = "IEC104_SIM_CYCLES" // reconnections to run, default 10
	envWatch  = "IEC104_SIM_WATCH"  // how long to watch for spontaneous data, default 15s
	envIOAs   = "IEC104_SIM_IOAS"   // comma-separated addresses to read back, optional

	// envCommandIOA opts in to DRIVING AN OUTPUT. Nothing is ever commanded
	// without it, and it takes an address rather than a boolean so that turning
	// it on is a decision about one specific point rather than a switch someone
	// flips while pointed at a substation.
	envCommandIOA = "IEC104_SIM_COMMAND_IOA"

	// envCommandSBO runs the command as select-before-operate. Any non-empty
	// value turns it on.
	envCommandSBO = "IEC104_SIM_COMMAND_SBO"
)

// --- what the device told us ------------------------------------------------

// observer records everything the station reports. It is a recorder, not an
// assertion: what a given device sends is that device's business, and this test
// exists partly to find out what that is.
type observer struct {
	mu sync.Mutex

	moves   []string
	types   map[asdu.TypeID]int
	causes  map[asdu.Cause]int
	addrs   map[asdu.IOA]int
	gis     []time.Duration
	giFails []error
	skews   []time.Duration
	syncErr []error
	unread  int // ASDUs whose header would not parse

	// events counts readings the station reported as events, by cause. On a
	// device that only answers interrogations this stays empty, and that is
	// the point: the filter is doing its job.
	events map[asdu.Cause]int

	// chattering counts events the chatter filter flagged. They are still
	// events and still counted above; this says how many carried the flag.
	chattering int

	// anomalies is the device breaking the standard in ways the connection
	// survives. Counted by kind, with one example of each kept: a device stuck
	// in a loop produces thousands of identical reports and the thousandth says
	// nothing the first did not.
	anomalies map[station.AnomalyKind]int
	examples  map[station.AnomalyKind]string

	// commandOutcomes is one line per finished command, for the report.
	commandOutcomes []string

	changed chan struct{}
}

func newObserver() *observer {
	return &observer{
		types:     make(map[asdu.TypeID]int),
		causes:    make(map[asdu.Cause]int),
		addrs:     make(map[asdu.IOA]int),
		events:    make(map[asdu.Cause]int),
		anomalies: make(map[station.AnomalyKind]int),
		examples:  make(map[station.AnomalyKind]string),
		changed:   make(chan struct{}, 256),
	}
}

func (o *observer) OnState(from, to station.State, reason error) {
	o.mu.Lock()
	line := fmt.Sprintf("%s -> %s", from, to)
	if reason != nil {
		line += ": " + reason.Error()
	}
	o.moves = append(o.moves, line)
	o.mu.Unlock()

	select {
	case o.changed <- struct{}{}:
	default:
	}
}

func (o *observer) OnASDU(raw []byte) {
	p := asdu.DefaultParams()
	h, body, err := p.ParseHeader(raw)

	o.mu.Lock()
	defer o.mu.Unlock()

	if err != nil {
		// Not a failure of the device: an ASDU this service does not yet
		// understand is a finding, and the count is the finding.
		o.unread++
		return
	}
	o.types[h.Type]++
	o.causes[h.Cause]++

	points, err := p.ParseMonitor(h, body)
	if err != nil {
		return
	}
	for _, pt := range points {
		o.addrs[pt.IOA]++
	}
}

func (o *observer) OnEvent(e station.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events[e.Point.Cause]++
	if e.ChatterSuppressed {
		o.chattering++
	}
}

func (o *observer) OnGI(d time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		o.giFails = append(o.giFails, err)
		return
	}
	o.gis = append(o.gis, d)
}

func (o *observer) OnClockSkew(skew time.Duration, _ bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.skews = append(o.skews, skew)
}

func (o *observer) OnCommand(cmd asdu.Command, res command.Result) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.commandOutcomes = append(o.commandOutcomes,
		fmt.Sprintf("%s ioa=%d -> %s (%s)", cmd.Type, cmd.IOA, res.Outcome, res.Code))
}

func (o *observer) OnAnomaly(kind station.AnomalyKind, detail string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.anomalies[kind]++
	if _, seen := o.examples[kind]; !seen {
		o.examples[kind] = detail
	}
}

func (o *observer) OnClockSync(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.syncErr = append(o.syncErr, err)
}

// countState is how many times the station has entered a state.
func (o *observer) countState(want station.State) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	suffix := "-> " + want.String()
	n := 0
	for _, m := range o.moves {
		if strings.Contains(m, suffix) {
			n++
		}
	}
	return n
}

// await blocks until the station has entered a state for the nth time.
func (o *observer) await(t *testing.T, want station.State, n int, within time.Duration) {
	t.Helper()

	deadline := time.NewTimer(within)
	defer deadline.Stop()

	for o.countState(want) < n {
		select {
		case <-o.changed:
		case <-deadline.C:
			o.mu.Lock()
			moves := strings.Join(o.moves, "\n  ")
			o.mu.Unlock()
			t.Fatalf("station reached %s %d time(s) in %s, want %d; it went:\n  %s",
				want, o.countState(want), within, n, moves)
		}
	}
}

// report prints what the device turned out to be. This is the half of the test
// that is not an assertion: GI_CONCURRENCY and the interrogation timeout are
// sized from the numbers below, and until a real device produces them they are
// guesses.
func (o *observer) report(t *testing.T) {
	t.Helper()

	o.mu.Lock()
	defer o.mu.Unlock()

	var b strings.Builder
	b.WriteString("\n--- what the device sent ---------------------------------------\n")

	b.WriteString("ASDU types:\n")
	for _, ty := range sortedKeys(o.types) {
		_, _ = fmt.Fprintf(&b, "  %-14s (%3d) x %d\n", ty, uint8(ty), o.types[ty])
	}
	if o.unread > 0 {
		_, _ = fmt.Fprintf(&b, "  UNPARSEABLE HEADER x %d\n", o.unread)

	}

	b.WriteString("causes of transmission:\n")
	for _, c := range sortedKeys(o.causes) {
		_, _ = fmt.Fprintf(&b, "  %-26s x %d\n", c, o.causes[c])
	}

	_, _ = fmt.Fprintf(&b, "distinct information object addresses: %d\n", len(o.addrs))

	if len(o.events) == 0 {
		b.WriteString("events: none (interrogation responses are not events)\n")
	} else {
		b.WriteString("events, by cause:\n")
		for _, c := range sortedKeys(o.events) {
			_, _ = fmt.Fprintf(&b, "  %-26s x %d\n", c, o.events[c])
		}
		_, _ = fmt.Fprintf(&b, "  flagged as chatter: %d\n", o.chattering)
	}

	if len(o.gis) > 0 {
		var total, worst time.Duration
		for _, d := range o.gis {
			total += d
			if d > worst {
				worst = d
			}
		}
		_, _ = fmt.Fprintf(&b, "interrogations: %d · mean %s · slowest %s\n",
			len(o.gis), total/time.Duration(len(o.gis)), worst)

		b.WriteString("  (this is the number GI_CONCURRENCY and GI_TIMEOUT_MS are sized from)\n")
	}
	for _, err := range o.giFails {
		_, _ = fmt.Fprintf(&b, "  interrogation FAILED: %v\n", err)
	}

	if len(o.skews) > 0 {
		var worst time.Duration
		for _, s := range o.skews {
			if s < 0 {
				s = -s
			}
			if s > worst {
				worst = s
			}
		}
		_, _ = fmt.Fprintf(&b, "clock skew: %d sample(s), largest |skew| %s\n", len(o.skews), worst)
		_, _ = fmt.Fprintf(&b, "  (a skew near a whole number of hours means %s is wrong, "+
			"not that the device's clock is)\n", envZone)
	}
	for _, err := range o.syncErr {
		_, _ = fmt.Fprintf(&b, "  clock synchronisation FAILED: %v\n", err)
	}

	for _, line := range o.commandOutcomes {
		_, _ = fmt.Fprintf(&b, "command: %s\n", line)
	}

	if len(o.anomalies) == 0 {
		b.WriteString("standard violations: none\n")
	} else {
		b.WriteString("STANDARD VIOLATIONS:\n")
		for kind, n := range o.anomalies {
			_, _ = fmt.Fprintf(&b, "  %-26s x %d\n    first: %s\n", kind, n, o.examples[kind])
		}
		b.WriteString("  (the connection survived these; see internal/station/anomaly.go\n" +
			"   for why they are reported rather than tolerated in silence)\n")
	}

	b.WriteString("----------------------------------------------------------------")
	t.Log(b.String())
}

func sortedKeys[K ~uint8 | ~uint16, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// --- cutting the link on purpose --------------------------------------------

// proxy sits between the station and the device so that the test can cut the
// link whenever it likes.
//
// Without it there is no way to test reconnection against equipment we do not
// control: a simulator offers no "drop this client now" button, and releasing
// the station and building another one would test construction rather than
// recovery — a different thing, and the easier one.
type proxy struct {
	t      *testing.T
	ln     net.Listener
	target string

	mu       sync.Mutex
	open     []net.Conn
	accepted int
	dialErrs []error
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the proxy: %v", err)
	}

	p := &proxy{t: t, ln: ln, target: target}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.cut()
	})
	return p
}

func (p *proxy) addr() string { return p.ln.Addr().String() }

func (p *proxy) serve() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}

		var d net.Dialer
		up, err := d.DialContext(context.Background(), "tcp", p.target)
		if err != nil {
			p.mu.Lock()
			p.dialErrs = append(p.dialErrs, err)
			p.mu.Unlock()
			_ = down.Close()
			continue
		}

		p.mu.Lock()
		p.open = append(p.open, down, up)
		p.accepted++
		p.mu.Unlock()

		go p.pipe(down, up)
		go p.pipe(up, down)
	}
}

// pipe copies one direction and tears down both when it ends, so a half-open
// connection never outlives the half that failed.
func (p *proxy) pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

// cut closes every socket in flight, which is what the station experiences when
// a link drops: not a graceful STOPDT, just silence and a reset.
func (p *proxy) cut() {
	p.mu.Lock()
	open := p.open
	p.open = nil
	p.mu.Unlock()

	for _, c := range open {
		_ = c.Close()
	}
}

func (p *proxy) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}

func (p *proxy) dialFailures() []error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]error(nil), p.dialErrs...)
}

// --- the tests --------------------------------------------------------------

func TestAgainstALiveDevice(t *testing.T) {
	addr := *simAddr
	if addr == "" {
		addr = os.Getenv(envAddr)
	}
	if addr == "" {
		t.Skipf("no device: pass -sim host:port or set %s to run this (see DEVELOPER_GUIDE.md)", envAddr)
	}

	obs := newObserver()
	prox := newProxy(t, addr)

	cfg := station.Config{
		Address:       prox.addr(),
		Params:        apci.DefaultParams(),
		ASDUParams:    asdu.DefaultParams(),
		CommonAddress: envCommonAddress(t),
		Clock:         clock.System(),
		Observer:      obs,
		DialGate:      station.NoGate{},
		GI: station.GI{
			OnConnect: true,
			// No periodic re-interrogation: the test drives every one it
			// wants, so a timer firing underneath would make the counts
			// below depend on how long the run happened to take.
			Period:  0,
			Timeout: 60 * time.Second,
		},
		ClockSync: station.ClockSync{
			// Reading the device's timestamps needs the zone; writing the
			// device's clock does not belong in a test that may be pointed at
			// production equipment. Enabled stays false.
			Zone: os.Getenv(envZone),
		},
		Backoff: station.Backoff{Base: 200 * time.Millisecond, Cap: 2 * time.Second, Factor: 2},
		Commands: station.Commands{
			// Enabled so the path is reachable; whether anything is actually
			// sent is decided by IEC104_SIM_COMMAND_IOA, which defaults to
			// sending nothing.
			Enabled:        true,
			WaitForActTerm: true,
			ActConTimeout:  10 * time.Second,
			ActTermTimeout: 20 * time.Second,
		},
	}

	before := goroutines()

	s, err := station.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
		obs.report(t)
	})

	// --- connect, handshake, interrogate ---
	//
	// A minute, because this budget covers a real device's own idea of how long
	// an interrogation takes, and that is exactly the number we do not know yet.
	obs.await(t, station.StateTopical, 1, 90*time.Second)

	if got := len(obs.gis); got == 0 {
		t.Error("the station became topical without completing an interrogation")
	}

	// --- spontaneous traffic ---
	watchFor := envDuration(t, envWatch, 15*time.Second)
	t.Logf("watching for spontaneous data for %s", watchFor)

	watch := time.NewTimer(watchFor)
	defer watch.Stop()
	select {
	case <-watch.C:
	case <-t.Context().Done():
	}

	obs.mu.Lock()
	seen := len(obs.addrs)
	obs.mu.Unlock()
	if seen == 0 {
		// Not an error. A device with a static database and no changing inputs
		// legitimately sends nothing at all after its interrogation.
		t.Logf("the device sent no information objects; either it has none " +
			"configured or nothing changed while we watched")
	}

	// --- reading a named point back out of the cache ---
	if ioas := envIOAList(t); len(ioas) > 0 {
		found, missing := s.ReadPoints(ioas)
		for _, r := range found {
			t.Logf("IOA %d = %s (cot %s, ts valid %t, skew %dms)",
				r.Point.IOA, valueOf(r.Point), r.Point.Cause, r.TSValid, r.SkewMillis)
		}
		if len(missing) > 0 {
			t.Errorf("%s named %v, but the device never sent %v; "+
				"either the addresses are wrong or the interrogation did not cover them",
				envIOAs, ioas, missing)
		}
	}

	// --- driving an output, if and only if we were told to ---
	exerciseCommand(t, s, obs)

	// --- drop, back off, reconnect, re-interrogate ---
	cycles := envCount(t, envCycles, 10)
	t.Logf("cutting the link %d time(s)", cycles)

	for i := 1; i <= cycles; i++ {
		prox.cut()
		obs.await(t, station.StateFailed, i, 30*time.Second)
		obs.await(t, station.StateTopical, i+1, 90*time.Second)
	}

	if got, want := len(obs.gis), cycles+1; got < want {
		t.Errorf("%d interrogations completed over %d reconnections, want at least %d; "+
			"a reconnected station must re-interrogate, because it has no idea "+
			"what changed while it was away", got, cycles, want)
	}
	if got := prox.connections(); got < cycles+1 {
		t.Errorf("the device accepted %d connections over %d reconnections", got, cycles)
	}
	for _, err := range prox.dialFailures() {
		t.Errorf("the proxy could not reach %s: %v", addr, err)
	}
	if fails := obs.giFails; len(fails) > 0 {
		t.Errorf("%d interrogation(s) failed: %v", len(fails), errors.Join(fails...))
	}

	// --- and none of it cost anything ---
	//
	// The bound is generous because the proxy contributes two goroutines per
	// live connection and the runtime reaps them on its own schedule. A leak
	// per reconnection would show up as a multiple of the cycle count, which no
	// bound this size can hide.
	if after := settle(before + 16); after > before+16 {
		t.Errorf("goroutines went from %d to %d over %d reconnections", before, after, cycles)
	}
}

// exerciseCommand sends one single command, but only when an address was named.
//
// This test can be pointed at field equipment. A command is not a read: it moves
// something, and the thing it moves may be a breaker. So the default is to send
// nothing at all, and opting in means naming the exact point.
func exerciseCommand(t *testing.T, s *station.Station, obs *observer) {
	t.Helper()

	raw := os.Getenv(envCommandIOA)
	if raw == "" {
		t.Logf("no command sent: set %s to an address to exercise the command path "+
			"(this DRIVES AN OUTPUT — do not point it at anything live)", envCommandIOA)
		return
	}

	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		t.Fatalf("%s=%q is not an address: %v", envCommandIOA, raw, err)
	}

	cmd := asdu.Command{Type: asdu.CScNa1, IOA: asdu.IOA(n), On: true}
	sbo := os.Getenv(envCommandSBO) != ""
	t.Logf("sending %s ioa=%d on=true (select-before-operate: %t)", cmd.Type, cmd.IOA, sbo)

	res, err := s.SendCommand(t.Context(), station.CommandRequest{
		Cmd:                 cmd,
		SelectBeforeOperate: sbo,
	})
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}

	t.Logf("command outcome: %s (code %q, last cause %s, P/N %t, safe to retry %t)",
		res.Outcome, res.Code, res.LastCause, res.Negative, res.Outcome.Safe())
	if res.Err != nil {
		t.Logf("  detail: %v", res.Err)
	}
	if !res.SentAt.IsZero() && !res.ConfirmedAt.IsZero() {
		t.Logf("  the device answered in %s", res.ConfirmedAt.Sub(res.SentAt))
	}

	// Every outcome is a legitimate thing for a device to do, so none of them
	// fails the test. What would be a real finding is the one impossible
	// answer: a command reported as never sent while the station was topical.
	if res.Outcome == command.OutcomeNotSent && res.Code == command.ErrCodeStationDisconnected {
		t.Errorf("the station was topical and still reported the command as not sent")
	}
	_ = obs
}

// --- environment ------------------------------------------------------------

func envCommonAddress(t *testing.T) uint16 {
	t.Helper()

	raw := os.Getenv(envCASDU)
	if raw == "" {
		return 1
	}
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		t.Fatalf("%s=%q is not a common address: %v", envCASDU, raw, err)
	}
	return uint16(n)
}

// envDuration reads a duration, accepting a bare number as seconds so that
// IEC104_SIM_WATCH=30 means what anyone would expect it to.
func envDuration(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()

	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a duration: %v", key, raw, err)
	}
	return d
}

// envCount reads a positive count.
func envCount(t *testing.T, key string, def int) int {
	t.Helper()

	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a number: %v", key, raw, err)
	}
	if n < 1 {
		t.Fatalf("%s=%d: there is no point running zero reconnections", key, n)
	}
	return n
}

func envIOAList(t *testing.T) []asdu.IOA {
	t.Helper()

	raw := os.Getenv(envIOAs)
	if raw == "" {
		return nil
	}

	var out []asdu.IOA
	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.ParseUint(field, 10, 32)
		if err != nil {
			t.Fatalf("%s=%q: %q is not an address: %v", envIOAs, raw, field, err)
		}
		out = append(out, asdu.IOA(n))
	}
	return out
}

// --- small helpers ----------------------------------------------------------

func valueOf(p asdu.Point) string {
	switch p.Kind {
	case asdu.KindBool:
		return strconv.FormatBool(p.Bool)
	case asdu.KindFloat:
		return strconv.FormatFloat(p.Float, 'g', -1, 64)
	case asdu.KindInt:
		return strconv.FormatInt(p.Int, 10)
	default:
		return "?"
	}
}

func goroutines() int { return runtime.NumGoroutine() }

// settle waits for goroutines that have been told to stop to leave the
// runtime's count. Same argument as internal/station's version: there is no
// event to wait on for "the scheduler has reaped a goroutine", the retry exits
// early once the count has come down, and a real leak cannot be waited away.
func settle(target int) int {
	n := runtime.NumGoroutine()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()

	for n > target {
		select {
		case <-tick.C:
			n = runtime.NumGoroutine()
		case <-deadline.C:
			return runtime.NumGoroutine()
		}
	}
	return n
}
