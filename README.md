# IEC 104 Gateway Library

[![Go Version](https://img.shields.io/badge/go-1.27+-blue.svg)](https://golang.org/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Gelişim sürecinde olan** bir IEC 60870-5-104 protokol kütüphanesi.

Bu kütüphane, IEC 60870-5-104 protokolü üzerinden endüstriyel RTU/PLC cihazlarına bağlanmayı, ölçüm verilerini okumayı ve kontrol komutları göndermeyi sağlar. Go dilinde yazılmıştır ve sadece standart kütüphane bağımlılıklarını kullanır.

## Özellikler

- **TCP bağlantı yönetimi** — IEC 104 master-side bağlantı, APCI state machine, T0/T1/T2 zamanlayıcılar
- **Genel sorgu (General Interrogation)** — Bağlantı kurulduğunda cihazdan tüm ölçüm verilerini çekme
- **Otomatik yeniden bağlanma** — Backoff ile kesilen bağlantıları geri alma
- **Saat senkronizasyonu** — Cihaz saatini sistem saatiyle eşleştirme
- **Komut gönderme** — Tek nokta, çift nokta, adım, süre komutları
- **Kuyruk tabanlı komut yönetimi** — Dialogue kuralları (ACTCON) ile sıralı komut işleme
- **Ölçüm önbelleği** — Son bilinen değerleri saklama ve delta tespiti
- **Test edilebilir zaman** — `clock.Clock` arayüzü ile testlerde `time.Now()` yerine mock zaman

## Kurulum

```bash
go get github.com/radaren989/iec104
```

## Kullanım

### Station Oluşturma

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

    // Bağlantı otomatik olarak kurulur ve genel sorgu (GI) çalışır.
    // Olaylar OnEvent callback'i ile gelir.
}
```

### Komut Gönderme

```go
import (
    "context"
    "github.com/radaren989/iec104/asdu"
    "github.com/radaren989/iec104/station"
)

// Tek nokta komutu (C_SC_NA_1) — bir çıkışı aç/kapat
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
log.Printf("komut sonucu: %v", result)
```

## Paket Yapısı

| Paket | Açıklama |
|-------|----------|
| `station` | Yüksek seviye API — bağlantı, GI, önbellek, komutlar |
| `conn` | TCP bağlantı ve APCI state machine |
| `command` | Komut serileştirme, kuyruk, dialogue yönetimi |
| `asdu` | ASDU header, TypeID, monitor ve control tipleri |
| `apci` | APCI frame encode/decode, U-function, format sabitleri |
| `clock` | Zaman arayüzü — production ve test için |

## Geliştirme

```bash
# Tüm paketleri derle
make build

# Testler (race detector ile)
make test

# Kalite kontrol (biçim + vet + lint + test)
make check
```

## Lisans

MIT
