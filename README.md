# IEC 60870-5-104 Go Library

[![Go Version](https://img.shields.io/badge/go-1.27+-blue.svg)](https://golang.org/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A Go library that connects to industrial RTU/PLC devices over the IEC 60870-5-104 (CS 104) protocol, reads measurement data, and sends control commands. It uses only standard library dependencies.

## Features

- **TCP connection management** — IEC 104 master-side connection, APCI state machine, T0/T1/T2/T3 timers
- **General Interrogation (GI)** — Pull all measurement data from the device upon connection
- **Automatic reconnection** — Reconnect dropped connections with exponential backoff
- **Clock synchronization** — Synchronize device clock with system time
- **Command sending** — Single-point, double-point, step, and set-point commands
- **Queue-based command management** — Sequential command processing with ACTCON/ACTTERM dialogue
- **Measurement cache** — Store last known values and detect deltas
- **Testable time** — `clock.Clock` interface for mocking `time.Now()` in tests
- **Chatter filter** — Prevent bouncing contacts from generating spurious events
- **Anomaly reporting** — Detect and report non-standard device behavior

## Installation

```bash
go get github.com/radaren989/iec104
```

## Quick Start

The following example shows a complete client that connects to an RTU, reads measurement data, and sends commands.

```go
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/radaren989/iec104/apci"
	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/clock"
	"github.com/radaren989/iec104/command"
	"github.com/radaren989/iec104/station"
)

// Observer receives state changes and data from the station.
type observer struct{}

func (o *observer) OnState(from, to station.State, reason error) {
	if reason != nil {
		log.Printf("state: %s -> %s (reason: %v)", from, to, reason)
	} else {
		log.Printf("state: %s -> %s", from, to)
	}
}

func (o *observer) OnASDU(raw []byte) {
	// Raw ASDU data received. Usually OnEvent is sufficient.
}

func (o *observer) OnEvent(e station.Event) {
	// Access the value as Bool, Float, or Int depending on Point.Kind.
	var value any
	switch e.Point.Kind {
	case asdu.KindBool:
		value = e.Point.Bool
	case asdu.KindFloat:
		value = e.Point.Float
	case asdu.KindInt:
		value = e.Point.Int
	}
	log.Printf("event: IOA=%d Type=%s Value=%v Quality=%v",
		e.Point.IOA, e.Point.Type, value, e.Point.Q)
}

func (o *observer) OnGI(d time.Duration, err error) {
	if err != nil {
		log.Printf("GI failed after %v: %v", d, err)
	} else {
		log.Printf("GI completed in %v", d)
	}
}

func (o *observer) OnClockSkew(skew time.Duration, beyondThreshold bool) {
	flag := "ok"
	if beyondThreshold {
		flag = "BEYOND THRESHOLD"
	}
	log.Printf("clock skew: %v (%s)", skew, flag)
}

func (o *observer) OnClockSync(err error) {
	if err != nil {
		log.Printf("clock sync failed: %v", err)
	}
}

func (o *observer) OnCommand(cmd asdu.Command, res command.Result) {
	log.Printf("command %s ioa=%d: outcome=%s", cmd.Type, cmd.IOA, res.Outcome)
}

func (o *observer) OnAnomaly(kind station.AnomalyKind, detail string) {
	log.Printf("anomaly: %s — %s", kind, detail)
}

func main() {
	// Station configuration.
	cfg := station.Config{
		Address:       "192.168.1.50:2404",
		Params:        apci.DefaultParams(),
		ASDUParams:    asdu.DefaultParams(),
		CommonAddress: 1,
		Clock:         clock.System(),
		Observer:      &observer{},
		Commands: station.Commands{
			Enabled: true,
		},
	}

	s, err := station.New(cfg)
	if err != nil {
		log.Fatalf("station create failed: %v", err)
	}
	defer s.Release()

	// The station connects automatically and runs General Interrogation.
	// Once connected and the cache is populated, the state transitions to topical.

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
}
```

### Sending Commands

After creating the station, use `SendCommand` to send commands. `Commands.Enabled` must be set to `true` in the config.

```go
import (
	"context"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/command"
	"github.com/radaren989/iec104/station"
)

// Single-point command (C_SC_NA_1) — open or close an output
res, err := s.SendCommand(ctx, station.CommandRequest{
	Cmd: asdu.Command{
		Type:     asdu.CScNa1,
		IOA:      asdu.IOA(10),
		On:       true,
		Duration: asdu.DurationPersistent,
	},
	SelectBeforeOperate: false,
})
if err != nil {
	log.Printf("send failed: %v", err)
	return
}

switch res.Outcome {
case command.OutcomeSucceeded:
	log.Println("command succeeded")
case command.OutcomeRejected:
	log.Printf("command rejected by device: %v", res.Code)
case command.OutcomeIndeterminate:
	log.Println("command sent but never confirmed — check device manually")
case command.OutcomeNotSent:
	log.Printf("command was not sent: %v", res.Code)
}
```

### Supported Command Types

| TypeID | Description |
|--------|-------------|
| `CScNa1` (45) | Single command — open/close |
| `CDcNa1` (46) | Double command — off/normal/reverse |
| `CRcNa1` (47) | Regulating step command — step up/down |
| `CSeNa1` (48) | Set point, normalised (-1..+1) |
| `CSeNb1` (49) | Set point, scaled (int16) |
| `CSeNc1` (50) | Set point, short float |
| `CBoNa1` (51) | Bitstring of 32 bits |

Timestamped variants (`CScTa1`, `CDcTa1`, etc.) are also supported.

### Select-Before-Operate

For critical commands, use a two-step dialogue: first the point is reserved, the device confirms, then the command is executed.

```go
import (
	"context"

	"github.com/radaren989/iec104/asdu"
	"github.com/radaren989/iec104/station"
)

res, err := s.SendCommand(ctx, station.CommandRequest{
	Cmd: asdu.Command{
		Type:   asdu.CDcNa1,
		IOA:    asdu.IOA(100),
		Double: asdu.DoubleOn,
	},
	SelectBeforeOperate: true,
})
```

## Package Structure

| Package | Description |
|---------|-------------|
| `station` | High-level API — connection, GI, cache, commands |
| `conn` | TCP connection and APCI state machine |
| `command` | Command serialization, queue, dialogue management |
| `asdu` | ASDU header, TypeID, monitor and control types |
| `apci` | APCI frame encode/decode, U-function, format constants |
| `clock` | Time interface — for production and testing |

## License

MIT
