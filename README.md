# IEC 104 Gateway Library

[![Go Version](https://img.shields.io/badge/go-1.27+-blue.svg)](https://golang.org/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A **work-in-progress** IEC 60870-5-104 protocol library.

This library provides connectivity to industrial RTU/PLC devices over the IEC 60870-5-104 protocol, enabling reading measurement data and sending control commands. It is written in Go and uses only standard library dependencies.

## Features

- **TCP connection management** — IEC 104 master-side connection, APCI state machine, T0/T1/T2 timers
- **General Interrogation** — Pull all measurement data from the device upon connection
- **Automatic reconnection** — Reconnect dropped connections with backoff
- **Clock synchronization** — Synchronize device clock with system time
- **Command sending** — Single-point, double-point, step, and duration commands
- **Queue-based command management** — Sequential command processing with dialogue rules (ACTCON)
- **Measurement cache** — Store last known values and detect deltas
- **Testable time** — `clock.Clock` interface for mocking `time.Now()` in tests

## Installation

```bash
go get github.com/radaren989/iec104
```

## Usage

### Creating a Station

```go
package main

import (
    "log"

    "github.com/radaren989/iec104/station"
    "github.com/radaren989/iec104/clock"
)

type myObserver struct{}

func (o *myObserver) OnEvent(event station.Event) {
    log.Printf("event: IOA=%d Value=%v Quality=%v", event.IOA, event.Value, event.Quality)
}

func (o *myObserver) OnState(state station.State) {
    log.Printf("state changed: %s", state)
}

func main() {
    s, err := station.New(station.Config{
        Address:       "192.168.1.50:2404",
        Clock:         clock.System(),
        CommonAddress: 1,
        Observer:      &myObserver{},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer s.Release()

    // Connection is established automatically and General Interrogation (GI) runs.
    // Events arrive via the OnEvent callback.
}
```

### Sending Commands

```go
import (
    "context"
    "github.com/radaren989/iec104/asdu"
    "github.com/radaren989/iec104/station"
)

// Single-point command (C_SC_NA_1) — open/close an output
result, err := s.SendCommand(context.Background(), station.CommandRequest{
    Cmd: asdu.Command{
        Type: asdu.CScNa1,
        IOA:  1,
        On:   true,
    },
})
if err != nil {
    log.Fatal(err)
}
log.Printf("command result: %v", result)
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
