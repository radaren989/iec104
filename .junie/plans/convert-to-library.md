---
sessionId: session-261022-143441-1nl6
---

# Requirements

### Overview
Proje şu anda `iec104-gateway` adında bir binary projesi. Amaç, bunu `go.iot/iec104` adında bir **kütüphane**ye çevirmek. Kullanıcı başka bir Go projesinde `go get go.iot/iec104` yaparak, `station.New()` ile bir station oluşturacak, `OnEvent` callback'i ile event verisi alacak ve `SendCommand()` ile komut gönderecek.

### Target Usage
```go
import (
    "go.iot/iec104/station"
    "go.iot/iec104/clock"
    "go.iot/iec104/asdu"
)

s, _ := station.New(station.Config{
    Address:       "192.168.1.50:2404",
    Clock:         clock.System(),
    CommonAddress: 1,
    Observer: &myObserver{},
})

// Komut gönder
res, _ := s.SendCommand(ctx, station.CommandRequest{
    Cmd: asdu.Command{Type: asdu.CScNa1, IOA: 1, On: true},
})
```

### Scope

**In Scope:**
- Modül adı `iec104-gateway` → `go.iot/iec104`
- `internal/` → `apci/`, `asdu/`, `clock/`, `command/`, `conn/`, `station/` (package'lar artık internal değil, public API)
- `main.go` ve `cmd/` silinir (kütüphane binary üretmez)
- `test/` dizini korunur (testler kütüphane içinde kalır)
- `Makefile` kütüphane hedeflerine güncellenir (binary build kaldırılır, test/lint/generate kalır)
- `go.mod` modül adı güncellenir
- Tüm cross-package import'lar yeni modül adıyla güncellenir

**Out of Scope:**
- gRPC katmanı (bu projede yok, DeviceManager'da yaşıyor)
- `gen/` ve sözleşme üretimi (kütüphane için gerekli değil, Makefile'dan kaldırılacak)
- Dockerfile (kütüphane konteynerize edilmez)
- iec104-structure.md gibi dokümantasyon dosyalarının içeriği güncellenmez

# Technical Design

### Current Implementation
Proje şu anda 6 paket ve test dosyalarından oluşuyor:
- `internal/apci/` — APCI frame encode/decode (pure)
- `internal/asdu/` — ASDU header, TypeID, monitor, control (pure)
- `internal/clock/` — Time interface (stdlib wrapper)
- `internal/command/` — Command serialization, queue, dialogue
- `internal/conn/` — TCP socket, APCI state machine
- `internal/station/` — Lifecycle, GI, cache, events, commands
- `test/` — Integration, capture replay, live tests
- `main.go` — Placeholder main (Hello World)

### Key Decisions

1. **Modül adı: `go.iot/iec104`** — Go modül adları genellikle `domain/path` formatında olur. `go.iot` domain'i IoT alanını temsil eder, `iec104` paketin ne olduğunu belirtir. Bu ad, `go get go.iot/iec104` ile import edilebilir.

2. **`internal/` kaldırılacak** — Şu anda tüm paketler `internal/` altında ve bu Go'nun "sadece bu modül içinde import edilebilir" kuralını tetikliyor. Kütüphane olarak kullanılmak için paketler `internal/` dışına taşınacak: `apci/`, `asdu/`, `clock/`, `command/`, `conn/`, `station/`.

3. **Paket isimleri korunacak** — `package station`, `package asdu` gibi isimler değişmeyecek. Kullanıcılar `iec104/station` olarak import edecek ve `station.New()` gibi çağrılar yapacak.

4. **`test/` dizini korunacak** — Testler kütüphane kalitesini korur. `test/capture/` ve `test/live/` paketleri kütüphane içinde kalacak.

5. **Makefile'dan kaldırılacak hedefler:** `build` (binary yok), `docker` (kütüphane konteynerize edilmez), `contract-fetch`, `generate`, `generate-check` (sözleşme üretimi kütüphane için gerekli değil).

### Proposed Changes

#### Modül ve import güncellemesi
- `go.mod`: `module iec104-gateway` → `module go.iot/iec104`
- Tüm `.go` dosyalarındaki `"iec104-gateway/..."` import'ları → `"go.iot/iec104/..."`

#### Paket taşıma (internal → root)
- `internal/apci/` → `apci/`
- `internal/asdu/` → `asdu/`
- `internal/clock/` → `clock/`
- `internal/command/` → `command/`
- `internal/conn/` → `conn/`
- `internal/station/` → `station/`
- `internal/` dizini silinir

#### Silinecek dosyalar
- `main.go` — kütüphane binary üretmez
- `cmd/` dizini (varsa)

#### Makefile güncellemesi
- `MODULE` → `go.iot/iec104`, `BINARY` ve `CMD_DIR` kaldırılır
- `build` hedefi: binary build kaldırılır, `go build ./...` (paket derleme) kalır
- `docker` hedefi kaldırılır
- `contract-fetch`, `generate`, `generate-check` hedefleri kaldırılır
- `DOCKER_IMAGE`, `CONTRACT_*` değişkenleri kaldırılır
- `test`, `lint`, `vet`, `fmt`, `fuzz`, `capture`, `check`, `tools`, `clean` hedefleri korunur

### Public API Surface
Kullanıcıların erişeceği exported tipler:

**`station` paketi:**
- `New(cfg Config) (*Station, error)` — station oluştur
- `Station.Release() error` — station kapat
- `Station.State() State` — durum sorgula
- `Station.SendCommand(ctx, req) (command.Result, error)` — komut gönder
- `Config` — yapılandırma
- `Observer` interface — callback'ler (`OnEvent`, `OnState`, `OnASDU`, `OnGI`, `OnCommand`, vb.)
- `Event`, `Reading`, `State`, `GI`, `ClockSync`, `Backoff`, `Commands`, `Chatter`, `CommandRequest`, `AnomalyKind`, `Gate`

**`asdu` paketi:**
- `Command`, `Point`, `Params`, `IOA`, `TypeID`, `Cause`, `Quality`, `CP56Time2a`, `DoubleCommand`, `StepCommand`, `OutputDuration`, `ValueKind` ve tüm `M_*` / `C_*` sabitleri

**`clock` paketi:**
- `Clock` interface, `Timer`, `Ticker`, `System()`

**`command` paketi:**
- `Result`, `Outcome`, `ErrorCode`, `Request`

**`apci` paketi:**
- `Frame`, `Params`, `Timers`, `UFunction`, format sabitleri

**`conn` paketi:**
- `Conn`, `Config`, `Handler` interface

# Testing

### Validation Approach
Değişikliklerden sonra kütüphane olarak çalışıp çalışmadığı doğrulanacak.

### Key Scenarios
1. **Modül derleniyor mu?** — `go build ./...` tüm paketleri derler (binary yok)
2. **Testler geçiyor mu?** — `make test` tüm testleri race detector ile koşar
3. **Import'lar doğru mu?** — Hiçbir dosyada `iec104-gateway/` veya `internal/` referansı kalmamış
4. **Paket yapısı doğru mu?** — `apci/`, `asdu/`, `clock/`, `command/`, `conn/`, `station/` root'ta
5. **Kullanım örneği çalışıyor mu?** — Basit bir `example_test.go` ile `station.New()` çağrısı doğrulanacak

### Edge Cases
- `test/capture/` ve `test/live/` paketlerinin import'ları yeni modül adıyla güncellenmeli
- `Makefile`'daki `FMT_PATHS` değişkeni `internal` yerine yeni paket dizinlerini kullanmalı
- `go.mod`'de harici bağımlılık olmadığı için `go mod tidy` gerekli değil

# Delivery Steps

### ✓ Step 1: Paketleri internal'den root'a taşı ve modül adını güncelle
Tüm `internal/` altındaki paketler root'a taşınacak ve modül adı `go.iot/iec104` olacak.
- `internal/apci/` → `apci/`, `internal/asdu/` → `asdu/`, `internal/clock/` → `clock/`, `internal/command/` → `command/`, `internal/conn/` → `conn/`, `internal/station/` → `station/`
- `internal/` dizini silinir
- `go.mod`: `module iec104-gateway` → `module go.iot/iec104`
- Tüm `.go` dosyalarındaki import'lar `"go.iot/iec104/..."` olarak güncellenir (yaklaşık 30+ dosya)
- `main.go` silinir (kütüphane binary üretmez)

### ✓ Step 2: Makefile'ı kütüphane hedeflerine güncelle
Makefile kütüphane kullanımına uygun hale getirilecek.
- `MODULE` → `go.iot/iec104`, `BINARY` ve `CMD_DIR` kaldırılır
- `build` hedefi: binary build kısmı kaldırılır, sadece `go build ./...` (paket derleme)
- `docker`, `contract-fetch`, `generate`, `generate-check` hedefleri kaldırılır
- `DOCKER_IMAGE`, `CONTRACT_*`, `CONTRACT_WORK` değişkenleri kaldırılır
- `FMT_PATHS`: `$(wildcard cmd internal test)` → `$(wildcard apci asdu clock command conn station test)`
- `help` mesajı güncellenir
- `test`, `lint`, `vet`, `fmt`, `fuzz`, `capture`, `check`, `tools`, `clean` hedefleri korunur