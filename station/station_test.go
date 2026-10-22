package station

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
	"github.com/radaren989/iec104/command"
)

// --- test doubles -----------------------------------------------------------

// recorder collects the state changes a station reports.
type recorder struct {
	mu        sync.Mutex
	moves     []move
	asdus     [][]byte
	events    []Event
	gis       []giReport
	skews     []skewReport
	syncErrs  []error
	anomalies []anomalyReport
	commands  []commandReport
	changed   chan struct{}
}

type move struct {
	from, to State
	reason   error
}

func newRecorder() *recorder {
	return &recorder{changed: make(chan struct{}, 1)}
}

func (r *recorder) OnState(from, to State, reason error) {
	r.mu.Lock()
	r.moves = append(r.moves, move{from, to, reason})
	r.mu.Unlock()

	// Non-blocking: OnState runs on the station's own goroutine, and a blocked
	// observer would stall the lifecycle it is observing.
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *recorder) OnASDU(raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asdus = append(r.asdus, raw)
}

func (r *recorder) OnEvent(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()

	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// eventReports is every event this observer was told about, in order.
func (r *recorder) eventReports() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// waitForEvent blocks until an event for ioa has been reported, or fails.
//
// Frames from one connection are handled in order on one goroutine, so once
// the event for a frame sent last has arrived, every frame sent before it has
// been handled too. That is what lets a test assert that an earlier frame
// produced NO event without a window for it to turn up in.
func (r *recorder) waitForEvent(t *testing.T, ioa asdu.IOA) {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for {
		for _, e := range r.eventReports() {
			if e.Point.IOA == ioa {
				return
			}
		}
		select {
		case <-r.changed:
		case <-timeout.C:
			t.Fatalf("no event for IOA %d; saw %v", ioa, r.eventReports())
		}
	}
}

type giReport struct {
	d   time.Duration
	err error
}

func (r *recorder) OnGI(d time.Duration, err error) {
	r.mu.Lock()
	r.gis = append(r.gis, giReport{d, err})
	r.mu.Unlock()

	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *recorder) giReports() []giReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]giReport(nil), r.gis...)
}

type skewReport struct {
	skew   time.Duration
	beyond bool
}

func (r *recorder) OnClockSkew(skew time.Duration, beyond bool) {
	r.mu.Lock()
	r.skews = append(r.skews, skewReport{skew, beyond})
	r.mu.Unlock()

	select {
	case r.changed <- struct{}{}:
	default:
	}
}

type anomalyReport struct {
	kind   AnomalyKind
	detail string
}

type commandReport struct {
	cmd asdu.Command
	res command.Result
}

func (r *recorder) OnCommand(cmd asdu.Command, res command.Result) {
	r.mu.Lock()
	r.commands = append(r.commands, commandReport{cmd: cmd, res: res})
	r.mu.Unlock()

	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// commandReports is every finished command this observer was told about.
func (r *recorder) commandReports() []commandReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]commandReport(nil), r.commands...)
}

func (r *recorder) OnAnomaly(kind AnomalyKind, detail string) {
	r.mu.Lock()
	r.anomalies = append(r.anomalies, anomalyReport{kind: kind, detail: detail})
	r.mu.Unlock()

	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// anomalyCount is how many of one kind have been reported.
func (r *recorder) anomalyCount(kind AnomalyKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for _, a := range r.anomalies {
		if a.kind == kind {
			n++
		}
	}
	return n
}

func (r *recorder) OnClockSync(err error) {
	r.mu.Lock()
	r.syncErrs = append(r.syncErrs, err)
	r.mu.Unlock()

	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *recorder) skewReports() []skewReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]skewReport(nil), r.skews...)
}

func (r *recorder) syncErrors() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.syncErrs...)
}

func (r *recorder) snapshot() []move {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]move(nil), r.moves...)
}

// waitFor blocks until the station has reached want, or fails.
//
// It waits on the observer's pulse rather than sleeping and re-reading: the
// station tells us when it moves, so there is nothing to poll for.
func (r *recorder) waitFor(t *testing.T, want State) {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for {
		for _, m := range r.snapshot() {
			if m.to == want {
				return
			}
		}
		select {
		case <-r.changed:
		case <-timeout.C:
			t.Fatalf("never reached %s; saw %v", want, r.snapshot())
		}
	}
}

// countReached is how many times the station entered want.
func (r *recorder) countReached(want State) int {
	n := 0
	for _, m := range r.snapshot() {
		if m.to == want {
			n++
		}
	}
	return n
}

// failingDialer refuses every dial and counts the attempts.
type failingDialer struct {
	mu      sync.Mutex
	calls   int
	err     error
	attempt chan struct{}
}

func newFailingDialer() *failingDialer {
	return &failingDialer{
		err:     errors.New("connection refused"),
		attempt: make(chan struct{}, 1024),
	}
}

func (d *failingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	select {
	case d.attempt <- struct{}{}:
	default:
	}
	return nil, d.err
}

func (d *failingDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// outstation is a controlled station real enough to run the cold-start
// sequence: it completes the U-format handshake and answers a general
// interrogation with the actcon/actterm bracket.
//
// It is deliberately a real socket speaking real frames, including correct
// sequence numbers, because the code under test validates them and a mock that
// skipped that would test a protocol nobody speaks.
type outstation struct {
	t  *testing.T
	ln net.Listener

	// behaviour, set before the station connects.
	giSilent    bool // never answer an interrogation at all
	giRefuse    bool // answer the activation with P/N set
	giNoActTerm bool // confirm, then never terminate
	clockRefuse bool // answer a clock synchronisation with P/N set

	// giReversed is the Freyr simulator's ordering, observed 2026-09-28: the
	// data, then the termination, then the confirmation. A second termination
	// follows, which is the deterministic reduction of what that device
	// actually does — it repeats the whole response several times a second, so
	// after our activation the station sees term, con, term.
	giReversed bool

	// giExtraActCon confirms twice before terminating. Legal: a device may
	// repeat a confirmation, and this must NOT be reported as an anomaly.
	giExtraActCon bool

	// giPoints is how many interrogated measurands to stream inside the
	// bracket. Zero sends none, which is what most of these tests want.
	giPoints int

	// giSpontaneous, when non-zero, is the address of one spontaneous
	// measurand sent halfway through the interrogated ones: a value that
	// changed while the device was answering.
	giSpontaneous asdu.IOA

	// Command behaviour, matching the four device behaviours T8.2 has to cope
	// with: answer properly, refuse, confirm without terminating, say nothing.
	cmdRefuse    bool
	cmdNoActTerm bool
	cmdSilent    bool

	// cmdSelectRefuse refuses the SELECTION of a select-before-operate
	// dialogue. The execution must then never arrive, which is the property
	// that makes selecting first worth the extra round trip.
	cmdSelectRefuse bool

	// cmdHold, when non-nil, makes the device wait for the test before
	// answering a command. It is how "two commands on the wire at once" is made
	// observable.
	cmdHold chan struct{}

	// noStartDTCon accepts the connection and never confirms STARTDT, which
	// leaves the station connected but with data transfer not started.
	noStartDTCon bool

	// received is every command the device was sent, in order.
	received []recvCommand

	mu       sync.Mutex
	conns    []net.Conn
	startDTs int
	gis      int
	clocks   int
	live     []*outstationConn
}

// outstationConn is one accepted connection with its own sequence numbers.
type outstationConn struct {
	o    *outstation
	sock net.Conn

	mu      sync.Mutex
	sendSeq uint16 // our N(S)
	recvSeq uint16 // how many I-frames we have taken in
}

func newOutstation(t *testing.T) *outstation {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	o := &outstation{t: t, ln: ln}
	go o.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		o.dropAll()
	})
	return o
}

func (o *outstation) addr() string { return o.ln.Addr().String() }

// handshakes is how many STARTDT activations this outstation has answered.
func (o *outstation) handshakes() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.startDTs
}

// clockSyncs is how many clock synchronisations it has been sent.
func (o *outstation) clockSyncs() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.clocks
}

// recvCommand is one command as the device saw it. The cause is kept alongside
// because asdu.Command carries only what to drive, not why — and a command that
// arrived with anything other than an activation is a master bug.
type recvCommand struct {
	asdu.Command
	Cause asdu.Cause

	// Selected is the S/E bit as it arrived. A select-before-operate dialogue
	// shows up here as two entries for the same point: one with it set and one
	// without.
	Selected bool
}

func selectedBit(t asdu.TypeID, data []byte) bool {
	sel, _ := t.SelectBit(data)
	return sel
}

// commands is every command this outstation has been sent, in order.
func (o *outstation) commands() []recvCommand {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]recvCommand(nil), o.received...)
}

// interrogations is how many general interrogations it has been asked for.
func (o *outstation) interrogations() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gis
}

// sendMeasurand sends one short-float measurand with COT=spontaneous on every
// live connection, which is what a station does when a value changes.
func (o *outstation) sendMeasurand(ioa asdu.IOA, v float32) {
	o.mu.Lock()
	conns := append([]*outstationConn(nil), o.live...)
	o.mu.Unlock()

	body, err := measurand(ioa, v)
	if err != nil {
		o.t.Errorf("encoding M_ME_NC_1: %v", err)
		return
	}
	for _, c := range conns {
		c.sendASDU(body)
	}
}

// measurand builds an M_ME_NC_1: a short float plus a quality octet.
//
// Built here from the exported header and element primitives for the same reason
// endOfInit is: the monitor direction travels station to master, so this service
// parses these and never produces one.
func measurand(ioa asdu.IOA, v float32) ([]byte, error) {
	return measurandWithCause(ioa, v, asdu.CauseSpontaneous)
}

// measurandWithCause is the same frame with the cause chosen by the caller,
// which is what separates "the value changed" from "you asked for it".
func measurandWithCause(ioa asdu.IOA, v float32, cause asdu.Cause) ([]byte, error) {
	p := asdu.DefaultParams()
	h := asdu.Header{
		Type:       asdu.MMeNc1,
		Count:      1,
		Cause:      cause,
		CommonAddr: testCASDU,
	}
	out, err := h.AppendTo(nil, p)
	if err != nil {
		return nil, err
	}

	elem := make([]byte, 0, 5)
	elem = binary.LittleEndian.AppendUint32(elem, math.Float32bits(v))
	elem = append(elem, 0) // QDS: no flags set

	return p.AppendElements(out, h, []asdu.Element{{IOA: ioa, Data: elem}})
}

// announceRestart sends M_EI_NA_1 on every live connection, which is what a
// station does after a power cycle (Study Reference §10).
func (o *outstation) announceRestart() {
	o.mu.Lock()
	conns := append([]*outstationConn(nil), o.live...)
	o.mu.Unlock()

	body, err := endOfInit()
	if err != nil {
		o.t.Errorf("encoding M_EI_NA_1: %v", err)
		return
	}
	for _, c := range conns {
		c.sendASDU(body)
	}
}

func (o *outstation) serve() {
	for {
		sock, err := o.ln.Accept()
		if err != nil {
			return
		}
		c := &outstationConn{o: o, sock: sock}

		o.mu.Lock()
		o.conns = append(o.conns, sock)
		o.live = append(o.live, c)
		o.mu.Unlock()

		go c.handle()
	}
}

// sendASDU wraps an ASDU in a correctly numbered I-frame and writes it.
func (c *outstationConn) sendASDU(body []byte) {
	c.mu.Lock()
	f := apci.NewI(c.sendSeq, c.recvSeq, body)
	c.sendSeq = (c.sendSeq + 1) % (apci.MaxSeq + 1)
	c.mu.Unlock()

	raw, err := f.Encode()
	if err != nil {
		return
	}
	_, _ = c.sock.Write(raw)
}

func (c *outstationConn) handle() {
	defer func() { _ = c.sock.Close() }()

	var buf []byte
	chunk := make([]byte, 4*apci.MaxFrameLen)
	for {
		n, err := c.sock.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				need, err := apci.NextFrameLen(buf)
				if err != nil || len(buf) < need {
					break
				}
				f, err := apci.Decode(buf[:need])
				buf = append(buf[:0], buf[need:]...)
				if err != nil {
					return
				}
				if !c.dispatch(f) {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// dispatch answers one frame. It reports whether the connection continues.
func (c *outstationConn) dispatch(f apci.Frame) bool {
	switch f.Format {
	case apci.FormatU:
		con, ok := f.Function.Confirm()
		if !ok {
			return true // a confirmation of ours; nothing to answer
		}
		if f.Function == apci.StartDTAct {
			c.o.mu.Lock()
			c.o.startDTs++
			silent := c.o.noStartDTCon
			c.o.mu.Unlock()
			if silent {
				return true // accepted, never confirmed
			}
		}
		raw, err := apci.NewU(con).Encode()
		if err != nil {
			return false
		}
		_, err = c.sock.Write(raw)
		return err == nil

	case apci.FormatI:
		c.mu.Lock()
		c.recvSeq = (f.SendSeq + 1) % (apci.MaxSeq + 1)
		c.mu.Unlock()
		c.answerASDU(f.ASDU)
		return true

	default: // S
		return true
	}
}

// answerASDU replies to an interrogation activation with the bracket the
// standard defines (Study Reference §10).
func (c *outstationConn) answerASDU(raw []byte) {
	p := asdu.DefaultParams()
	h, body, err := p.ParseHeader(raw)
	if err != nil || h.Cause != asdu.CauseActivation {
		return
	}

	if h.Type == asdu.CCsNa1 {
		c.answerClockSync(p, body)
		return
	}
	if asdu.SupportsControlType(h.Type) {
		c.answerCommand(p, h, body)
		return
	}
	if h.Type != asdu.CIcNa1 {
		return
	}

	c.o.mu.Lock()
	c.o.gis++
	silent, refuse, noTerm := c.o.giSilent, c.o.giRefuse, c.o.giNoActTerm
	reversed, extraCon, points := c.o.giReversed, c.o.giExtraActCon, c.o.giPoints
	midSpontaneous := c.o.giSpontaneous
	c.o.mu.Unlock()

	if silent {
		return
	}

	con, err := p.AppendInterrogation(nil, testCASDU, 0, asdu.CauseActCon, asdu.QOIStation)
	if err != nil {
		c.o.t.Errorf("encoding actcon: %v", err)
		return
	}
	if refuse {
		con = withNegativeBit(con, p)
	}

	term, err := p.AppendInterrogation(nil, testCASDU, 0, asdu.CauseActTerm, asdu.QOIStation)
	if err != nil {
		c.o.t.Errorf("encoding actterm: %v", err)
		return
	}

	if reversed {
		c.streamInterrogated(points, midSpontaneous)
		c.sendASDU(term)
		c.sendASDU(con)
		c.sendASDU(term)
		return
	}

	c.sendASDU(con)
	if extraCon {
		c.sendASDU(con)
	}
	if refuse || noTerm {
		return
	}
	c.streamInterrogated(points, midSpontaneous)

	c.sendASDU(term)
}

// streamInterrogated sends n measurands with the interrogation cause, which is
// what a real device puts between the confirmation and the termination. A
// non-zero spontaneous is sent as a change report halfway through.
func (c *outstationConn) streamInterrogated(n int, spontaneous asdu.IOA) {
	for i := 1; i <= n; i++ {
		if spontaneous != 0 && i == n/2+1 {
			body, err := measurand(spontaneous, float32(spontaneous))
			if err != nil {
				c.o.t.Errorf("encoding a spontaneous measurand: %v", err)
				return
			}
			c.sendASDU(body)
		}
		body, err := measurandWithCause(asdu.IOA(i), float32(i), asdu.CauseIntroGeneral)
		if err != nil {
			c.o.t.Errorf("encoding an interrogated measurand: %v", err)
			return
		}
		c.sendASDU(body)
	}
}

// sendRaw sends an already encoded ASDU on every live connection.
func (o *outstation) sendRaw(body []byte) {
	o.mu.Lock()
	conns := append([]*outstationConn(nil), o.live...)
	o.mu.Unlock()

	for _, c := range conns {
		c.sendASDU(body)
	}
}

// sendInterrogated sends one measurand carrying the interrogation cause on every
// live connection, whether or not anybody asked for an interrogation.
func (o *outstation) sendInterrogated(ioa asdu.IOA, v float32) {
	o.mu.Lock()
	conns := append([]*outstationConn(nil), o.live...)
	o.mu.Unlock()

	body, err := measurandWithCause(ioa, v, asdu.CauseIntroGeneral)
	if err != nil {
		o.t.Errorf("encoding an interrogated measurand: %v", err)
		return
	}
	for _, c := range conns {
		c.sendASDU(body)
	}
}

// answerCommand plays one of the four device behaviours back at the master.
//
// The confirmation echoes the command exactly, which is what a real device does
// and what the master's correlation depends on: 104 offers the type and the
// information object address and nothing else to match an answer to a request.
func (c *outstationConn) answerCommand(p asdu.Params, h asdu.Header, body []byte) {
	elemLen := len(body) - int(p.IOAOctets)
	elems, err := p.SplitElements(h, body, elemLen)
	if err != nil || len(elems) != 1 {
		c.o.t.Errorf("a command arrived that does not split into one object: %v", err)
		return
	}

	c.o.mu.Lock()
	c.o.received = append(c.o.received, recvCommand{
		Command: asdu.Command{
			Type: h.Type,
			IOA:  elems[0].IOA,
			// The state is read back out of the qualifier octet so that a test
			// can assert the master encoded what it was asked to.
			On: len(elems[0].Data) > 0 && elems[0].Data[0]&0x01 != 0,
		},
		Cause:    h.Cause,
		Selected: selectedBit(h.Type, elems[0].Data),
	})
	refuse, noTerm, silent, hold := c.o.cmdRefuse, c.o.cmdNoActTerm, c.o.cmdSilent, c.o.cmdHold
	selRefuse := c.o.cmdSelectRefuse
	c.o.mu.Unlock()

	if silent {
		return
	}
	if hold != nil {
		<-hold
	}

	// The echo carries the qualifier octet back unchanged, S/E bit included,
	// which is what a real device does and what lets the master tell a
	// selection's confirmation from an execution's.
	selected, _ := h.Type.SelectBit(elems[0].Data)

	con := withCause(append([]byte(nil), c.echo(h, body)...), asdu.CauseActCon)
	if refuse || (selected && selRefuse) {
		con = withNegativeBit(con, p)
	}
	c.sendASDU(con)

	// A selection is confirmed, not terminated: there is no operation to finish.
	if selected || refuse || noTerm {
		return
	}
	c.sendASDU(withCause(c.echo(h, body), asdu.CauseActTerm))
}

// echo rebuilds the command frame as the device would send it back.
func (c *outstationConn) echo(h asdu.Header, body []byte) []byte {
	out, err := h.AppendTo(nil, asdu.DefaultParams())
	if err != nil {
		c.o.t.Errorf("re-encoding a command header: %v", err)
		return nil
	}
	return append(out, body...)
}

// withCause relabels an encoded ASDU. The cause octet follows the type and the
// variable structure qualifier; P/N and T live in its top two bits.
func withCause(raw []byte, cause asdu.Cause) []byte {
	const causeOctet = 2

	out := append([]byte(nil), raw...)
	if len(out) > causeOctet {
		out[causeOctet] = (out[causeOctet] &^ 0x3F) | (byte(cause) & 0x3F)
	}
	return out
}

// endOfInit builds an M_EI_NA_1, the frame a station sends after a power cycle.
//
// The asdu package only parses this type, and correctly so: it travels station
// to master, and this service is the master. Building one is a thing only a
// fake station needs to do, so it is built here out of the exported header and
// element primitives rather than by adding a producer nothing in production
// would ever call.
func endOfInit() ([]byte, error) {
	p := asdu.DefaultParams()
	h := asdu.Header{
		Type:       asdu.MEiNa1,
		Count:      1,
		Cause:      asdu.CauseInitialised,
		CommonAddr: testCASDU,
	}
	out, err := h.AppendTo(nil, p)
	if err != nil {
		return nil, err
	}
	return p.AppendElements(out, h, []asdu.Element{{IOA: 0, Data: []byte{byte(asdu.InitLocalPowerOn)}}})
}

// withNegativeBit sets P/N on an encoded ASDU: the station understood what was
// asked and is declining it.
//
// The bit is flipped by hand because the asdu package has no producer for a
// negative confirmation, and should not: this service is the master, so it
// receives these and never sends one. P/N is bit 6 of the cause octet, which
// follows the type and the variable structure qualifier.
func withNegativeBit(raw []byte, p asdu.Params) []byte {
	const causeOctet = 2 // after type (1) and VSQ (1)
	const pnBit = 0x40

	out := append([]byte(nil), raw...)
	if len(out) > causeOctet {
		out[causeOctet] |= pnBit
	}
	_ = p
	return out
}

// withTestBit sets T on an encoded ASDU: the frame is part of a test and must
// not be acted on. T is bit 7 of the cause octet.
func withTestBit(raw []byte) []byte {
	const causeOctet = 2 // after type (1) and VSQ (1)
	const testBit = 0x80

	out := append([]byte(nil), raw...)
	if len(out) > causeOctet {
		out[causeOctet] |= testBit
	}
	return out
}

// answerClockSync confirms a C_CS_NA_1, or refuses it with P/N set.
func (c *outstationConn) answerClockSync(p asdu.Params, body []byte) {
	c.o.mu.Lock()
	c.o.clocks++
	refuse := c.o.clockRefuse
	c.o.mu.Unlock()

	at, err := p.ParseClockSync(asdu.Header{
		Type: asdu.CCsNa1, Count: 1, Cause: asdu.CauseActivation, CommonAddr: testCASDU,
	}, body)
	if err != nil {
		c.o.t.Errorf("parsing the clock synchronisation: %v", err)
		return
	}

	con, err := p.AppendClockSync(nil, testCASDU, 0, asdu.CauseActCon, at)
	if err != nil {
		c.o.t.Errorf("encoding the clock confirmation: %v", err)
		return
	}
	if refuse {
		con = withNegativeBit(con, p)
	}
	c.sendASDU(con)
}

// sendTimestamped sends one short-float measurand carrying a device timestamp,
// which is what the skew measurement needs.
func (o *outstation) sendTimestamped(ioa asdu.IOA, v float32, at time.Time) {
	o.mu.Lock()
	conns := append([]*outstationConn(nil), o.live...)
	o.mu.Unlock()

	body, err := timestampedMeasurand(ioa, v, at)
	if err != nil {
		o.t.Errorf("encoding M_ME_TF_1: %v", err)
		return
	}
	for _, c := range conns {
		c.sendASDU(body)
	}
}

// timestampedMeasurand builds an M_ME_TF_1: a short float, a quality octet and
// a CP56Time2a.
func timestampedMeasurand(ioa asdu.IOA, v float32, at time.Time) ([]byte, error) {
	p := asdu.DefaultParams()
	h := asdu.Header{
		Type:       asdu.MMeTf1,
		Count:      1,
		Cause:      asdu.CauseSpontaneous,
		CommonAddr: testCASDU,
	}
	out, err := h.AppendTo(nil, p)
	if err != nil {
		return nil, err
	}

	elem := make([]byte, 0, 12)
	elem = binary.LittleEndian.AppendUint32(elem, math.Float32bits(v))
	elem = append(elem, 0) // QDS
	elem = asdu.CP56Time2aFrom(at).AppendTo(elem)

	return p.AppendElements(out, h, []asdu.Element{{IOA: ioa, Data: elem}})
}

// dropAll closes every accepted socket, which is what a station losing its link
// actually experiences.
func (o *outstation) dropAll() {
	o.mu.Lock()
	conns := o.conns
	o.conns = nil
	// live is cleared with them: a dropped connection must not go on receiving
	// measurands meant for the link that replaced it, and over a test that
	// reconnects a hundred times the slice would otherwise only grow.
	o.live = nil
	o.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

func testConfig(t *testing.T, addr string, obs Observer) Config {
	t.Helper()
	return Config{
		Address:       addr,
		Params:        apci.DefaultParams(),
		ASDUParams:    asdu.DefaultParams(),
		CommonAddress: testCASDU,
		Clock:         clock.System(),
		Observer:      obs,
		DialGate:      NoGate{},
	}
}

// testCASDU is the common address every test station uses. A single constant
// rather than a literal per test, because a mismatch between what the station
// is configured for and what the outstation answers with is a silent failure:
// the interrogation simply never completes.
const testCASDU uint16 = 1

// --- configuration ----------------------------------------------------------

func TestNewRejectsAnUnusableConfig(t *testing.T) {
	t.Parallel()

	good := func() Config {
		return Config{
			Address:       "127.0.0.1:2404",
			Params:        apci.DefaultParams(),
			ASDUParams:    asdu.DefaultParams(),
			CommonAddress: testCASDU,
			Clock:         clock.System(),
			Observer:      newRecorder(),
		}
	}

	cases := []struct {
		name   string
		break_ func(*Config)
	}{
		{"no address", func(c *Config) { c.Address = "" }},
		{"no clock", func(c *Config) { c.Clock = nil }},
		{"no observer", func(c *Config) { c.Observer = nil }},
		{"impossible APCI parameters", func(c *Config) { c.Params.W = c.Params.K }},
		{"a backoff cap below its base", func(c *Config) {
			c.Backoff = Backoff{Base: time.Minute, Cap: time.Second, Factor: 2}
		}},
		{"a chatter policy with only a window", func(c *Config) {
			c.Chatter = Chatter{Window: DefaultChatterWindow}
		}},
		{"a chatter threshold beyond its bound", func(c *Config) {
			c.Chatter = Chatter{MaxChanges: MaxChatterMaxChanges + 1, Window: DefaultChatterWindow}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := good()
			tc.break_(&cfg)

			s, err := New(cfg)
			if err == nil {
				_ = s.Release()
				t.Fatal("a station was started with a configuration that cannot work")
			}
			if !errors.Is(err, ErrConfig) {
				t.Errorf("%v does not wrap ErrConfig", err)
			}
		})
	}
}

// --- the connect path -------------------------------------------------------

func TestAStationConnectsAndCompletesTheHandshake(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	if got := s.State(); got != StateTopical {
		t.Errorf("State() = %s, want topical", got)
	}
	if o.interrogations() != 1 {
		t.Errorf("the outstation was interrogated %d times, want 1", o.interrogations())
	}

	// Reaching "started" must mean the handshake actually happened. Without
	// this the station could set the state and skip STARTDT entirely, and the
	// test would pass while the connection sat there receiving nothing
	// (Study Reference §4).
	if n := o.handshakes(); n != 1 {
		t.Errorf("the outstation answered %d STARTDT activations, want 1", n)
	}

	// The path taken must be the one the design draws, in order. Started and
	// Interrogating are both on it and neither is topical: between STARTDT con
	// and the actterm the station is up and its cache is a subset of unknown
	// size, which is worse than an empty one because it looks complete.
	want := []State{StateQueued, StateConnecting, StateStarted, StateInterrogating, StateTopical}
	var got []State
	for _, m := range rec.snapshot() {
		got = append(got, m.to)
	}
	if len(got) < len(want) {
		t.Fatalf("states %v, want at least %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("state %d = %s, want %s (full path %v)", i, got[i], w, got)
		}
	}
}

func TestAFailedDialIsReportedWithItsReason(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	cfg := testConfig(t, "127.0.0.1:1", rec)
	cfg.Dialer = newFailingDialer()

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateFailed)

	for _, m := range rec.snapshot() {
		if m.to != StateFailed {
			continue
		}
		// "It reconnected" is noise. "It reconnected because the dial was
		// refused" is what an operator can act on, so a move into Failed
		// without a reason is a bug rather than a tidy state change.
		if m.reason == nil {
			t.Fatal("moved into failed without a reason")
		}
		return
	}
}

// --- backoff and reconnection -----------------------------------------------

func TestAFailedStationRetriesOnTheBackoffSchedule(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	dialer := newFailingDialer()

	rec := newRecorder()
	cfg := testConfig(t, "127.0.0.1:1", rec)
	cfg.Clock = fake
	cfg.Dialer = dialer
	cfg.Backoff = Backoff{Base: time.Second, Cap: time.Minute, Factor: 2} // no jitter

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	waitDials(t, dialer, 1)

	// Nothing until the backoff elapses. A retry that ignored the schedule
	// would hammer a device that is merely rebooting.
	fake.BlockUntil(1)
	fake.Advance(900 * time.Millisecond)
	if got := dialer.count(); got != 1 {
		t.Fatalf("%d dials after 900ms of a 1s backoff, want 1", got)
	}

	fake.Advance(200 * time.Millisecond)
	waitDials(t, dialer, 2)

	// The second wait is twice the first.
	fake.BlockUntil(1)
	fake.Advance(1900 * time.Millisecond)
	if got := dialer.count(); got != 2 {
		t.Fatalf("%d dials after 1.9s of a 2s backoff, want 2", got)
	}
	fake.Advance(200 * time.Millisecond)
	waitDials(t, dialer, 3)
}

// The acceptance criterion: a hundred consecutive failed dials must cost
// nothing. A station whose retry loop leaks a goroutine per attempt is not a
// leak at a thousand stations, it is an outage.
func TestAHundredFailedDialsLeakNothing(t *testing.T) {
	// Not parallel: it counts goroutines process-wide.

	fake := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	dialer := newFailingDialer()

	rec := newRecorder()
	cfg := testConfig(t, "127.0.0.1:1", rec)
	cfg.Clock = fake
	cfg.Dialer = dialer
	cfg.Backoff = Backoff{Base: time.Second, Cap: time.Minute, Factor: 2}

	before := settle(0)

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const attempts = 100
	for i := 1; i <= attempts; i++ {
		waitDials(t, dialer, i)
		fake.BlockUntil(1)
		// Past the cap in one jump, so the schedule never holds the test up.
		fake.Advance(2 * time.Minute)
	}

	during := runtime.NumGoroutine()
	if during > before+8 {
		t.Errorf("goroutines went from %d to %d over %d failed dials", before, during, attempts)
	}

	if err := s.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if after := settle(before); after > before+3 {
		t.Errorf("goroutines went from %d to %d after release", before, after)
	}

	// The schedule is exponential with a ceiling, so a hundred attempts must
	// not have produced a hundred different waits nor a runaway one.
	if d := cfg.Backoff.Delay(attempts); d != cfg.Backoff.Cap {
		t.Errorf("the %dth delay is %s, want the cap %s", attempts, d, cfg.Backoff.Cap)
	}
}

func TestAStationReconnectsAfterTheLinkDrops(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	o := newOutstation(t)

	rec := newRecorder()
	cfg := testConfig(t, o.addr(), rec)
	cfg.Clock = fake
	cfg.Backoff = Backoff{Base: time.Second, Cap: time.Minute, Factor: 2}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	rec.waitFor(t, StateTopical)

	// The link dies the way links actually die: the far end goes away.
	o.dropAll()
	rec.waitFor(t, StateFailed)

	fake.BlockUntil(1)
	fake.Advance(2 * time.Second)

	// Back up, and the second connect is a full path rather than a resumption:
	// 104 has no session to resume, so every reconnection redoes the handshake
	// AND re-interrogates. Nothing cached from before the drop can be trusted:
	// the station may have changed while we were not listening, and 104 sends
	// no history.
	waitCount(t, rec, StateTopical, 2)
	if n := o.interrogations(); n < 2 {
		t.Errorf("interrogated %d times over two connections, want at least 2", n)
	}
	if n := rec.countReached(StateConnecting); n < 2 {
		t.Errorf("reached connecting %d times, want at least 2", n)
	}
	if n := o.handshakes(); n < 2 {
		t.Errorf("the outstation answered %d STARTDT activations, want at least 2; "+
			"104 has no session to resume, so every reconnection redoes the handshake", n)
	}
}

// --- release ----------------------------------------------------------------

func TestReleaseIsTerminal(t *testing.T) {
	t.Parallel()

	o := newOutstation(t)
	rec := newRecorder()

	s, err := New(testConfig(t, o.addr(), rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec.waitFor(t, StateStarted)

	if err := s.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := s.State(); got != StateReleased {
		t.Fatalf("State() = %s, want released", got)
	}

	// Released means released: no reconnection, no further states. This is what
	// separates giving a station up from a connection dropping, and the lease
	// sweeper depends on the difference.
	n := len(rec.snapshot())
	//nolint:forbidigo // Proving that nothing happens needs a window in which
	// it could have. There is no event to wait on for "the station did not
	// restart", and a fake clock would not help: the thing under test is that
	// no timer was armed at all.
	time.Sleep(100 * time.Millisecond)
	if got := len(rec.snapshot()); got != n {
		t.Errorf("%d state changes after release: %v", got-n, rec.snapshot()[n:])
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	cfg := testConfig(t, "127.0.0.1:1", rec)
	cfg.Dialer = newFailingDialer()

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 3 {
		if err := s.Release(); err != nil {
			t.Fatalf("Release %d: %v", i, err)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func waitDials(t *testing.T, d *failingDialer, n int) {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for d.count() < n {
		select {
		case <-d.attempt:
		case <-timeout.C:
			t.Fatalf("only %d of %d dials were attempted", d.count(), n)
		}
	}
}

func waitCount(t *testing.T, r *recorder, want State, n int) {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for r.countReached(want) < n {
		select {
		case <-r.changed:
		case <-timeout.C:
			t.Fatalf("reached %s %d times, want %d; saw %v", want, r.countReached(want), n, r.snapshot())
		}
	}
}

// settle waits for goroutines that have been told to stop to leave the
// runtime's count. Same argument as internal/conn's version: there is no event
// to wait on for "the scheduler has reaped a goroutine", the retry exits early
// once the count has come down, and a real leak cannot be waited away.
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
