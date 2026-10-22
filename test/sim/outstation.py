#!/usr/bin/env python3
"""A reference IEC 60870-5-104 outstation, written from the standard.

Why this exists, and why it is Python
-------------------------------------
Every other test in this repository talks to code this repository also wrote.
When a real device then disagrees with us, there is no way to tell whose reading
of the standard is wrong: ours, or the device's. This file is the third opinion.
It was written from IEC 60870-5-101 clause 7 and the 104 companion standard
directly, in a different language, sharing no code and no author's-eye-view with
internal/apci or internal/asdu. If our Go master and this Python outstation
agree, the specification is very probably being read the same way by two
independent implementations. If they disagree, one of us has a bug and the
disagreement says where to look.

It also fills the gaps a real simulator left open (T6.9 findings): quality bits
that are actually set, double points in every one of their four states, and
time-tagged types, none of which the Freyr simulator produces.

Modes
-----
    strict      The bracket as the standard defines it: act-con, then the
                COT=20 stream, then act-term. Once per activation. This is the
                control case.

    freyr       What the Freyr simulator was observed doing on 2026-09-28:
                data first, then act-term, then act-con, and the whole response
                repeated in a loop that no one asked for. This is the
                reproduction case; a master that copes with `strict` and not
                with `freyr` has found a real difference, not a mystery.

    no-actcon   Terminates without confirming.
    no-actterm  Confirms and never terminates.
    silent      Ignores interrogations entirely.

Command modes (--cmd-mode), the four device behaviours a master has to survive:

    normal      act-con, a pause for the mechanism, then act-term.
    refuse      act-con with P/N set. The output did not move and it says so.
    no-actterm  Confirms and never terminates. Study Reference §8 warns that
                plenty of real devices do this; a master that insists on a
                termination reports every one of their commands as unknown.
    silent      Says nothing. The command is on the wire and may have driven the
                output, which is the one case where neither "it worked" nor "it
                failed" is an honest answer.
    select-refuse
                Refuses the SELECTION of a select-before-operate dialogue and
                answers an execution normally. A master that gets this right
                never sends the execution at all, so the field stays untouched.

Usage
-----
    python3 outstation.py --addr 127.0.0.1:2404 --mode strict
    python3 outstation.py --mode freyr --loop-period 1.0

It speaks only what a master needs from an outstation: the U-format handshake,
I/S sequencing with a w-window acknowledgement, general interrogation, clock
synchronisation, and periodic and spontaneous monitor data. It is not a device
emulator and does not pretend to be one.
"""

from __future__ import annotations

import argparse
import datetime as dt
import math
import random
import socket
import struct
import sys
import threading
import time

# --- APCI ---------------------------------------------------------------------

START = 0x68
MAX_SEQ = 1 << 15  # sequence numbers are 15-bit and wrap

# U-format function bits. Each is a distinct pair (act, con) and only one may be
# set at a time, which is why they are whole octets rather than flags to combine.
STARTDT_ACT, STARTDT_CON = 0x07, 0x0B
STOPDT_ACT, STOPDT_CON = 0x13, 0x23
TESTFR_ACT, TESTFR_CON = 0x43, 0x83

I_FORMAT, S_FORMAT, U_FORMAT = "I", "S", "U"


def parse_apci(ctrl: bytes):
    """Split four control octets into (format, send_seq, recv_seq, function)."""
    c1, c2, c3, c4 = ctrl
    if c1 & 0x01 == 0:
        return I_FORMAT, ((c2 << 8) | c1) >> 1, ((c4 << 8) | c3) >> 1, None
    if c1 & 0x03 == 0x01:
        return S_FORMAT, None, ((c4 << 8) | c3) >> 1, None
    return U_FORMAT, None, None, c1


def i_frame(send_seq: int, recv_seq: int, asdu: bytes) -> bytes:
    ctrl = struct.pack("<HH", (send_seq << 1) & 0xFFFF, (recv_seq << 1) & 0xFFFF)
    return bytes([START, len(ctrl) + len(asdu)]) + ctrl + asdu


def s_frame(recv_seq: int) -> bytes:
    ctrl = struct.pack("<BBH", 0x01, 0x00, (recv_seq << 1) & 0xFFFF)
    return bytes([START, 4]) + ctrl


def u_frame(function: int) -> bytes:
    return bytes([START, 4, function, 0x00, 0x00, 0x00])


# --- ASDU ---------------------------------------------------------------------

M_SP_NA_1, M_DP_NA_1, M_ST_NA_1, M_ME_NC_1 = 1, 3, 5, 13
M_SP_TB_1, M_ME_TF_1, M_EI_NA_1 = 30, 36, 70
C_IC_NA_1, C_CS_NA_1 = 100, 103

# Control direction. A master sends these; an outstation echoes them back with
# act-con and then act-term.
C_SC_NA_1, C_DC_NA_1, C_RC_NA_1 = 45, 46, 47
C_SE_NA_1, C_SE_NB_1, C_SE_NC_1, C_BO_NA_1 = 48, 49, 50, 51
CONTROL_TYPES = {
    C_SC_NA_1, C_DC_NA_1, C_RC_NA_1,
    C_SE_NA_1, C_SE_NB_1, C_SE_NC_1, C_BO_NA_1,
    58, 59, 60, 61, 62, 63, 64,  # the CP56Time2a variants
}

CAUSE_PERIODIC = 1
CAUSE_SPONTANEOUS = 3
CAUSE_INIT = 4
CAUSE_ACT = 6
CAUSE_ACTCON = 7
CAUSE_ACTTERM = 10
CAUSE_INTROGEN = 20

QOI_STATION = 20


def asdu_header(type_id: int, count: int, cause: int, casdu: int,
                sequence: bool = False, negative: bool = False,
                test: bool = False, originator: int = 0) -> bytes:
    vsq = (count & 0x7F) | (0x80 if sequence else 0)
    cot = (cause & 0x3F) | (0x40 if negative else 0) | (0x80 if test else 0)
    return struct.pack("<BBBBH", type_id, vsq, cot, originator, casdu)


def ioa(addr: int) -> bytes:
    """Three octets, little endian. 104 fixes the width at three."""
    return struct.pack("<I", addr)[:3]


def cp56time2a(when: dt.datetime, invalid: bool = False, summer: bool = False) -> bytes:
    """Seven octets. Carries LOCAL time with no offset — the SU bit says summer
    time is in effect but not by how much, which is why a receiver needs to be
    told the zone out of band."""
    ms = when.second * 1000 + when.microsecond // 1000
    return struct.pack(
        "<HBBBBB",
        ms,
        (when.minute & 0x3F) | (0x80 if invalid else 0),
        (when.hour & 0x1F) | (0x80 if summer else 0),
        (when.day & 0x1F) | ((when.isoweekday() & 0x07) << 5),
        when.month & 0x0F,
        (when.year - 2000) & 0x7F,
    )


def qds(overflow=False, blocked=False, substituted=False, not_topical=False,
        invalid=False) -> int:
    """Quality descriptor. The five flags a master must not ignore: a value with
    IV set is the device saying its own reading is untrustworthy."""
    return ((0x01 if overflow else 0) | (0x10 if blocked else 0)
            | (0x20 if substituted else 0) | (0x40 if not_topical else 0)
            | (0x80 if invalid else 0))


def siq(on: bool, **flags) -> int:
    return (0x01 if on else 0) | (qds(**flags) & 0xF0)


def diq(state: int, **flags) -> int:
    """state: 0 indeterminate, 1 off, 2 on, 3 indeterminate."""
    return (state & 0x03) | (qds(**flags) & 0xF0)


# --- the device's database ----------------------------------------------------

class Database:
    """Forty-two points, deliberately mirroring the Freyr simulator's address
    layout so the two can be compared directly — but with values and quality
    that actually vary, which is the part Freyr does not do."""

    def __init__(self, casdu: int, rng: random.Random):
        self.casdu = casdu
        self.rng = rng
        self.singles = list(range(100, 116))    # 16
        self.doubles = list(range(200, 208))    # 8
        self.steps = list(range(300, 302))      # 2
        self.floats = list(range(600, 616))     # 16
        self.tick = 0

    def total_points(self) -> int:
        return len(self.singles) + len(self.doubles) + len(self.steps) + len(self.floats)

    # Quality is rotated rather than random so that a run of any length covers
    # every flag, and so two runs are comparable.
    def _quality(self, n: int) -> dict:
        return [
            {},
            {"invalid": True},
            {"not_topical": True},
            {"substituted": True},
            {"blocked": True},
            {"overflow": True},
        ][n % 6]

    def interrogation_asdus(self, cause: int = CAUSE_INTROGEN) -> list[bytes]:
        """The whole database, packed the way a device does it: one ASDU per
        type, several objects each, never one frame per point."""
        out = []
        t = self.tick

        body = b"".join(
            ioa(a) + bytes([siq((t + i) % 2 == 0, **self._quality(i))])
            for i, a in enumerate(self.singles)
        )
        out.append(asdu_header(M_SP_NA_1, len(self.singles), cause, self.casdu) + body)

        # Every double-point state, including both spellings of "indeterminate".
        body = b"".join(
            ioa(a) + bytes([diq((t + i) % 4, **self._quality(i))])
            for i, a in enumerate(self.doubles)
        )
        out.append(asdu_header(M_DP_NA_1, len(self.doubles), cause, self.casdu) + body)

        body = b"".join(
            ioa(a) + bytes([((t + i) % 64) & 0x7F, qds(**self._quality(i))])
            for i, a in enumerate(self.steps)
        )
        out.append(asdu_header(M_ST_NA_1, len(self.steps), cause, self.casdu) + body)

        body = b"".join(
            ioa(a) + struct.pack("<f", self._value(a, t)) + bytes([qds(**self._quality(i))])
            for i, a in enumerate(self.floats)
        )
        out.append(asdu_header(M_ME_NC_1, len(self.floats), cause, self.casdu) + body)

        return out

    def _value(self, addr: int, t: int) -> float:
        return round(230.0 + 10.0 * math.sin((addr + t) / 7.0), 3)

    def spontaneous_asdu(self, now: dt.datetime, summer: bool) -> bytes:
        """One time-tagged measurand. The tag is what makes clock skew
        measurable at all, so a simulator that never sends one leaves that whole
        path unexercised."""
        self.tick += 1
        addr = self.rng.choice(self.floats)
        body = (ioa(addr)
                + struct.pack("<f", self._value(addr, self.tick))
                + bytes([qds()])
                + cp56time2a(now, summer=summer))
        return asdu_header(M_ME_TF_1, 1, CAUSE_SPONTANEOUS, self.casdu) + body

    def periodic_asdu(self) -> bytes:
        self.tick += 1
        body = b"".join(
            ioa(a) + struct.pack("<f", self._value(a, self.tick)) + bytes([qds()])
            for a in self.floats
        )
        return asdu_header(M_ME_NC_1, len(self.floats), CAUSE_PERIODIC, self.casdu) + body

    def end_of_init(self) -> bytes:
        return asdu_header(M_EI_NA_1, 1, CAUSE_INIT, self.casdu) + ioa(0) + bytes([0])


# --- one connection -----------------------------------------------------------

class Session:
    def __init__(self, sock: socket.socket, peer, opts, log):
        self.sock = sock
        self.peer = peer
        self.opts = opts
        self.log = log
        self.db = Database(opts.casdu, random.Random(opts.seed))

        self.lock = threading.Lock()
        self.send_seq = 0
        self.recv_seq = 0          # how many I-frames we have taken in
        self.acked = 0             # how many of those we have acknowledged
        self.started = False
        self.closing = False

        self.gi_wanted = threading.Event()
        self.last_rx = time.monotonic()

    # -- framing --

    def _write(self, raw: bytes):
        try:
            self.sock.sendall(raw)
        except OSError:
            self.closing = True

    def send_asdu(self, asdu: bytes):
        with self.lock:
            if not self.started or self.closing:
                return
            frame = i_frame(self.send_seq, self.recv_seq, asdu)
            self.send_seq = (self.send_seq + 1) % MAX_SEQ
            self.acked = self.recv_seq
        self._write(frame)

    def send_u(self, function: int):
        self._write(u_frame(function))

    def send_s(self):
        with self.lock:
            if self.acked == self.recv_seq:
                return
            self.acked = self.recv_seq
            n = self.recv_seq
        self._write(s_frame(n))

    # -- receiving --

    def run(self):
        threading.Thread(target=self.background, daemon=True).start()

        buf = b""
        while not self.closing:
            try:
                chunk = self.sock.recv(65536)
            except OSError:
                break
            if not chunk:
                break
            buf += chunk
            self.last_rx = time.monotonic()

            while len(buf) >= 2:
                if buf[0] != START:
                    self.log(f"{self.peer}: not a 104 frame (0x{buf[0]:02x}); closing")
                    self.closing = True
                    break
                need = 2 + buf[1]
                if len(buf) < need:
                    break
                self.handle(buf[2:need])
                buf = buf[need:]

        self.closing = True
        try:
            self.sock.close()
        except OSError:
            pass
        self.log(f"{self.peer}: disconnected")

    def handle(self, apdu: bytes):
        fmt, ns, nr, fn = parse_apci(apdu[:4])

        if fmt == U_FORMAT:
            if fn == STARTDT_ACT:
                self.log(f"{self.peer}: STARTDT act -> con")
                self.started = True
                self.send_u(STARTDT_CON)
            elif fn == STOPDT_ACT:
                self.log(f"{self.peer}: STOPDT act -> con")
                self.started = False
                self.send_u(STOPDT_CON)
            elif fn == TESTFR_ACT:
                self.send_u(TESTFR_CON)
            return

        if fmt == S_FORMAT:
            return  # the master acknowledging us; nothing to do

        # I-format.
        with self.lock:
            self.recv_seq = (ns + 1) % MAX_SEQ
        self.dispatch(apdu[4:])

        # The w window: acknowledge every w-th frame rather than every frame.
        with self.lock:
            outstanding = (self.recv_seq - self.acked) % MAX_SEQ
        if outstanding >= self.opts.w:
            self.send_s()

    def dispatch(self, asdu: bytes):
        if len(asdu) < 6:
            return
        type_id, vsq, cot_octet, _orig, casdu = struct.unpack("<BBBBH", asdu[:6])
        cause = cot_octet & 0x3F
        body = asdu[6:]

        if casdu != self.opts.casdu:
            self.log(f"{self.peer}: ASDU for common address {casdu}, we are {self.opts.casdu}")
            return

        if type_id == C_IC_NA_1 and cause == CAUSE_ACT:
            qoi = body[3] if len(body) >= 4 else 0
            self.log(f"{self.peer}: C_IC_NA_1 act (QOI={qoi})")
            self.gi_wanted.set()

        elif type_id in CONTROL_TYPES and cause == CAUSE_ACT:
            self.answer_command(type_id, vsq, body)

        elif type_id == C_CS_NA_1 and cause == CAUSE_ACT:
            self.log(f"{self.peer}: C_CS_NA_1 act -> con")
            self.send_asdu(asdu_header(C_CS_NA_1, 1, CAUSE_ACTCON, self.opts.casdu) + body)

    @staticmethod
    def select_bit(type_id: int, element: bytes) -> bool | None:
        """Read the S/E bit out of a command element.

        The qualifier is the element's last octet, except on the timestamped
        variants where a CP56Time2a follows it. C_BO has no qualifier at all,
        which is why a bit-string command cannot be selected.
        """
        if type_id in (C_BO_NA_1, 64):
            return None
        end = len(element) - (7 if 58 <= type_id <= 64 else 0)
        if end < 1:
            return None
        return bool(element[end - 1] & 0x80)

    def answer_command(self, type_id: int, vsq: int, body: bytes):
        """Echo a command back as the standard says: act-con, then act-term.

        The echo matters. A master has only the type and the information object
        address to match an answer to a request — 104 carries no request
        identifier — so an outstation that answered with anything else would be
        unmatchable, and a master that accepted it would be wrong.
        """
        addr = int.from_bytes(body[:3], "little") if len(body) >= 3 else 0
        selected = self.select_bit(type_id, body[3:])
        step = "select" if selected else "execute"
        self.log(f"{self.peer}: command type={type_id} ioa={addr} {step} [{self.opts.cmd_mode}]")

        mode = self.opts.cmd_mode
        if mode == "silent":
            return

        def echo(cause: int, negative: bool = False) -> bytes:
            # The echo carries the qualifier back unchanged, S/E bit included.
            # A master has only the type, the address and that bit to tell a
            # selection's confirmation from an execution's.
            return asdu_header(type_id, vsq & 0x7F, cause, self.opts.casdu,
                               negative=negative) + body

        if mode == "refuse" or (selected and mode == "select-refuse"):
            self.send_asdu(echo(CAUSE_ACTCON, negative=True))
            return

        self.send_asdu(echo(CAUSE_ACTCON))

        # A selection is confirmed, not terminated: it reserves a point, it does
        # not operate one, so there is no mechanism whose finish to report.
        if selected or mode == "no-actterm":
            return

        # A real device takes time here: the confirmation says it accepted the
        # request, the termination says the mechanism finished moving.
        time.sleep(self.opts.cmd_delay)
        self.send_asdu(echo(CAUSE_ACTTERM))

    # -- producing --

    def background(self):
        """Interrogations, periodic data, spontaneous data and t3, all on one
        timeline so the ordering between them is deterministic enough to read in
        a log."""
        next_periodic = time.monotonic() + self.opts.periodic
        next_spont = time.monotonic() + self.opts.spontaneous
        next_loop = None

        if self.opts.announce_restart:
            while not self.started and not self.closing:
                time.sleep(0.01)
            self.send_asdu(self.db.end_of_init())

        while not self.closing:
            now = time.monotonic()

            if self.gi_wanted.is_set():
                self.gi_wanted.clear()
                self.answer_interrogation()
                if self.opts.mode == "freyr":
                    next_loop = time.monotonic() + self.opts.loop_period

            elif next_loop is not None and now >= next_loop:
                # Freyr's unrequested repeat. No master asked for this.
                self.answer_interrogation()
                next_loop = now + self.opts.loop_period

            if now >= next_periodic:
                self.send_asdu(self.db.periodic_asdu())
                next_periodic = now + self.opts.periodic

            if now >= next_spont:
                self.send_asdu(self.db.spontaneous_asdu(
                    dt.datetime.now(), self.opts.summer))
                next_spont = now + self.opts.spontaneous

            # t3: ask the master whether it is still there, after idle.
            if self.opts.t3 and now - self.last_rx > self.opts.t3:
                self.send_u(TESTFR_ACT)
                self.last_rx = now

            time.sleep(0.02)

    def answer_interrogation(self):
        mode = self.opts.mode
        if mode == "silent":
            return

        actcon = asdu_header(C_IC_NA_1, 1, CAUSE_ACTCON, self.opts.casdu) + ioa(0) + bytes([QOI_STATION])
        actterm = asdu_header(C_IC_NA_1, 1, CAUSE_ACTTERM, self.opts.casdu) + ioa(0) + bytes([QOI_STATION])
        data = self.db.interrogation_asdus()

        if mode == "strict":
            self.send_asdu(actcon)
            for a in data:
                self.send_asdu(a)
            self.send_asdu(actterm)

        elif mode == "freyr":
            # Observed on 2026-09-28: the data, then the termination, then the
            # confirmation. Out of order, and repeated unbidden.
            for a in data:
                self.send_asdu(a)
            self.send_asdu(actterm)
            self.send_asdu(actcon)

        elif mode == "no-actcon":
            for a in data:
                self.send_asdu(a)
            self.send_asdu(actterm)

        elif mode == "no-actterm":
            self.send_asdu(actcon)
            for a in data:
                self.send_asdu(a)


# --- server -------------------------------------------------------------------

def main() -> int:
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--addr", default="127.0.0.1:2404")
    p.add_argument("--mode", default="strict",
                   choices=["strict", "freyr", "no-actcon", "no-actterm", "silent"])
    p.add_argument("--casdu", type=int, default=1)
    p.add_argument("--w", type=int, default=8, help="acknowledge every w-th I-frame")
    p.add_argument("--t3", type=float, default=0.0,
                   help="seconds of silence before TESTFR act; 0 disables")
    p.add_argument("--periodic", type=float, default=2.0, help="COT=1 period, seconds")
    p.add_argument("--spontaneous", type=float, default=1.0,
                   help="COT=3 time-tagged period, seconds")
    p.add_argument("--loop-period", type=float, default=1.0,
                   help="freyr mode: how often to repeat the unrequested GI response")
    p.add_argument("--cmd-mode", default="normal",
                   choices=["normal", "refuse", "select-refuse", "no-actterm", "silent"],
                   help="how to answer a command: the four behaviours T8.2 has to cope with")
    p.add_argument("--cmd-delay", type=float, default=0.2,
                   help="seconds between act-con and act-term, standing in for a mechanism")
    p.add_argument("--announce-restart", action="store_true",
                   help="send M_EI_NA_1 after the handshake")
    p.add_argument("--summer", action="store_true", help="set the SU bit on timestamps")
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--quiet", action="store_true")
    opts = p.parse_args()

    host, _, port = opts.addr.rpartition(":")

    def log(msg):
        if not opts.quiet:
            print(f"{dt.datetime.now():%H:%M:%S.%f} {msg}", flush=True)

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((host or "0.0.0.0", int(port)))
    srv.listen(8)
    print(srv.getsockname()[0] + ":" + str(srv.getsockname()[1]), flush=True)
    log(f"outstation ready, mode={opts.mode}, cmd-mode={opts.cmd_mode}, casdu={opts.casdu}")

    try:
        while True:
            sock, peer = srv.accept()
            sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            name = f"{peer[0]}:{peer[1]}"
            log(f"{name}: connected")
            threading.Thread(target=Session(sock, name, opts, log).run, daemon=True).start()
    except KeyboardInterrupt:
        return 0
    finally:
        srv.close()


if __name__ == "__main__":
    sys.exit(main())
