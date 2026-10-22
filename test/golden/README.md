# `test/golden/` — what a real device actually sent

Every hand-written vector in `internal/apci` and `internal/asdu` was worked out
from the standard. Those prove the implementation agrees with **the
specification**, which is necessary and not sufficient: interoperability bugs
live exactly where a device and the specification disagree. The files here are
the other half of the claim.

## The corpus

The captures are in `test/pcap/`; the summaries here are generated from them.

| Capture | Summary | What it contributes |
|---|---|---|
| `iec104.pcap` | `capture-summary.txt` | A real RTU. Cold start, TESTFR, two general interrogations, fourteen monitor types, and all seven untimestamped commands with their confirmations and terminations. |
| `sim-cap.pcapng` | `sim-capture-summary.txt` | A simulator. Clock synchronisation (`C_CS_NA_1`) and counter interrogation (`C_CI_NA_1`), which the RTU capture never exercises. Its types are now a subset of `cap2`'s, but it is kept for its *shape*: three TCP sessions in one file, one of them not 104 at all. That is what the connection handling in `walk` exists to survive, and deleting the capture would delete the only test of it. |
| `cap2.pcapng` | `cap2-capture-summary.txt` | The same simulator with counters configured. Adds `M_IT_NA_1` and `M_ME_ND_1` — the normalised type that carries no quality descriptor at all, where "no flags set" has to mean *no information* rather than *good*. |

The corpus is listed in one place — the `captures` table in
`test/capture/replay_test.go`. Adding a capture means adding a row and running
the update target.

## Why the summaries are committed

A capture is binary and unreadable in a review. The summary is the readable
half: one line per frame, with the decoded ASDU under it. When a decoder
changes, the diff shows exactly which frames now read differently, in terms an
engineer can argue with.

They are generated, never edited by hand:

```
make capture-update     # or: .\make.ps1 capture-update
git diff                # READ THIS - the reading is the point
```

A summary that changes without anyone being able to say why is the signal this
file exists to produce.

## Adding a capture

```
tshark -i <iface> -f 'tcp port 2404' -w new.pcapng
```

Drop it in `test/pcap/`, add a row to `captures` with a one-line note on what it
adds, run `make capture-update`, and read the new summary before committing it.

Both classic pcap and pcapng are read, over Ethernet or loopback link types.
Anything else is refused with a message saying so rather than misread.

**A capture from a different vendor's device is the single most valuable thing
anyone can contribute to these two packages.** `TestCaptureCoverage` logs what is
still tested only against the standard — that list is the shopping list.
