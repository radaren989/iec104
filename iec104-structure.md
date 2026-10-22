# IEC 60870-5-104 Protocol Implementation — Structure Documentation

Bu doküman, `elec-monitor` projesindeki IEC 104 protokolü implementasyonunun yapısını, her dosyanın işlevini ve fonksiyonların sorumluluklarını açıklar. Amaç, bağlantı katmanını (connection layer) başka bir projeye alıp üzerinde geliştirme yapabilmek için gerekli tüm bilgiyi sağlamak.

---

## Genel Mimari

IEC 104 implementasyonu katmanlı bir yapıda tasarlanmıştır. Her katman **pure** (saf) fonksiyonlarla yazılmıştır; yani network, Redis, gRPC, logging, metrics gibi dış bağımlılık içermez. Bu sayede her katman bağımsız test edilebilir ve başka projelere taşınabilir.

```
┌─────────────────────────────────────────────────────────────┐
│  station  (Yaşam döngüsü, reconnect, GI, komut kuyruğu)     │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  command  (Komut serileştirme, select-before-execute)  │  │
│  └───────────────────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  conn  (TCP socket, APCI state machine, 3 goroutine)   │  │
│  │  ┌─────────────────────────────────────────────────┐  │  │
│  │  │  apci  (Frame encode/decode, sequence, timers)   │  │  │
│  │  └─────────────────────────────────────────────────┐  │  │
│  │  ┌─────────────────────────────────────────────────┘  │  │
│  │  │  asdu  (ASDU header, TypeID, monitor, control)     │  │  │
│  │  └─────────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────────┘  │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  clock  (Zaman arayüzü — test edilebilirlik için)      │  │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### Paket Bağımlılık Yönü

```
station → conn → apci, asdu, clock
station → command → asdu, clock
station → clock
conn → apci, clock
asdu → (bağımlılık yok — pure)
apci → (bağımlılık yok — pure)
clock → (sadece stdlib time)
```

---

## 1. `internal/apci/` — APCI (Application Protocol Control Information)

**Sorumluluk:** IEC 104 APCI katmanı. Frame geometrisi, encode/decode, sequence number yönetimi, akış kontrolü (k/w window), ve t1/t2/t3 timer mantığı.

Bu paket tamamen **pure**'dir: byte slice girer, değer çıkar. Network, timer, log yok.

### Dosyalar

#### `apci.go` — Temel sabitler, tipler, hatalar

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `StartByte` | `const = 0x68` | Her APDU'nun başlangıç baytı |
| `HeaderLen` | `const = 2` | Start byte + length byte |
| `ControlLen` | `const = 4` | 4 bayt kontrol alanı |
| `MaxAPDULen` | `const = 253` | Maksimum APDU uzunluğu |
| `MaxASDULen` | `const = 249` | Maksimum ASDU uzunluğu (253 - 4) |
| `MaxFrameLen` | `const = 255` | Maksimum frame uzunluğu (2 + 253) |
| `MaxSeq` | `const = 32767` | 15-bit sequence number üst sınırı |
| `Format` | `type uint8` | Frame formatı: `FormatI`, `FormatS`, `FormatU` |
| `UFunction` | `type byte` | U-format fonksiyon: `StartDTAct`, `StartDTCon`, `StopDTAct`, `StopDTCon`, `TestFRAct`, `TestFRCon` |
| `UFunction.Valid()` | `method` | Tam olarak bir fonksiyon bitinin ayarlı olup olmadığını kontrol eder |
| `UFunction.IsAct()` | `method` | Activation mı yoksa confirmation mı olduğunu söyler |
| `UFunction.Confirm()` | `method` | Bir activation'a karşılık gelen confirmation'u döner |
| `ErrIncomplete` | `var error` | Buffer'da tam frame yok — daha fazla byte oku |
| `ErrStartByte` | `var error` | 0x68 başlangıç baytı bulunamadı — bağlantı kapat |
| `ErrLength` | `var error` | Length alanı 4..253 aralığında değil |
| `ErrFormat` | `var error` | Kontrol alanı geçersiz (bilinmeyen U fonksiyon, rezerve bit, S/U frame'de payload) |
| `ErrSeqRange` | `var error` | Sequence number MaxSeq'i aşıyor |
| `ErrASDUTooLong` | `var error` | I-frame'in payload'ı MaxASDULen'i aşıyor |
| `ErrEmptyASDU` | `var error` | I-frame'de ASDU yok |

**Frame Yapısı (wire format):**
```
0x68 | length | ctrl0 | ctrl1 | ctrl2 | ctrl3 | [ASDU...]
     \________ length bu alandan itibaren sayılır _________/
```

#### `frame.go` — Frame encode/decode

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Frame` | `struct` | Decode edilmiş bir APDU: `Format`, `SendSeq`, `RecvSeq`, `Function`, `ASDU` |
| `NewI(sendSeq, recvSeq, asdu)` | `func` | I-format frame oluşturur |
| `NewS(recvSeq)` | `func` | S-format (supervisory) frame oluşturur |
| `NewU(fn)` | `func` | U-format (control) frame oluşturur |
| `NextFrameLen(b []byte)` | `func` | Stream'deki ilk frame'in toplam uzunluğunu döner; `ErrIncomplete` dönerse daha fazla byte oku |
| `Decode(b []byte)` | `func` | Tam bir frame'i parse eder. Trailing byte varsa hata verir |
| `Frame.Encode()` | `method` | Frame'i yeni bir byte slice'a encode eder |
| `Frame.AppendTo(dst []byte)` | `method` | Frame'i mevcut bir slice'a append eder (allocation yok) |

**Iç fonksiyonlar:**
- `decodeSeq(lo, hi byte, name string)` — 15-bit sequence number okur, rezerve bit kontrolü yapar
- `encodeSeq(seq uint16)` — Sequence number'ı 2 bayta encode eder

#### `seq.go` — Sequence Number Yönetimi

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `seqSpace` | `const = 32768` | Sequence number uzayı (0..32767) |
| `ErrUnexpectedSeq` | `var error` | Peer'in N(S) bekleneni değil — frame kayıp, duplike veya re-order |
| `ErrInvalidAck` | `var error` | Peer N(R) outstanding window'un dışında — gelecekten ack veya geriye hareket |
| `Sequencer` | `struct` | Bir bağlantının numaralı durumu: `sendSeq`, `ackLow`, `recvSeq`, `unackedRecv` |
| `Sequencer.Outstanding()` | `method` | Gönderilmiş ama henüz acknowledge edilmemiş I-frame sayısı |
| `Sequencer.UnackedReceived()` | `method` | Alınan ama henüz acknowledge edilmemiş I-frame sayısı (w window için) |
| `Sequencer.SendSeq()` | `method` | Sonraki gönderilecek I-frame'in N(S) değerini döner (tüketmez) |
| `Sequencer.RecvSeq()` | `method` | Sonraki alınacak I-frame'in N(S) / gönderilecek N(R) değerini döner |
| `Sequencer.NextSend()` | `method` | Sonraki N(S) tüketir ve (sendSeq, recvSeq) döner. S-frame göndererek w counter'ı sıfırlar |
| `Sequencer.SendSupervisory()` | `method` | S-frame gönderildiğini kaydeder, recvSeq döner. N(S) tüketmez |
| `Sequencer.Receive(sendSeq, recvSeq)` | `method` | Gelen I-frame'i kaydeder. N(S) beklenen olmalı, N(R) acknowledge uygulanır |
| `Sequencer.Acknowledge(recvSeq)` | `method` | Peer'den gelen N(R) uygular, cleared frame sayısı döner |

**Kritik detay:** Sequence number karşılaştırmaları `seqDistance()` ile modular aritmetik kullanır. 32767'den 0'a wrap sırasında `<` veya `>` kullanırsa saatler/günler sonra bağlantı neden bilinmeden düşer.

#### `timers.go` — t1/t2/t3 Timer Mantığı

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Action` | `type uint8` | Timer sonucunda yapılacak işlem |
| `ActionNone` | `const` | Hiçbir şey yok |
| `ActionClose` | `const` | t1 expired — bağlantı kapat (retransmission yok) |
| `ActionSendSupervisory` | `const` | t2 expired — S-frame gönder |
| `ActionSendTestFR` | `const` | t3 expired — TESTFR act gönder |
| `Timers` | `struct` | Bir bağlantının t1/t2/t3 durumu. Clock tutmaz, her metod `now` alır |
| `NewTimers(p Params)` | `func` | Params ile timer state oluşturur |
| `Timers.Start(now)` | `method` | Bağlantı kuruldu, t3 (idle) timer başlatır |
| `Timers.Stop()` | `method` | Tüm timer'ları kaldırır |
| `Timers.SentI(now)` | `method` | I-frame gönderildi — t1 arm, t2 disarm, t3 reset |
| `Timers.SentS(now)` | `method` | S-frame gönderildi — t2 disarm, t3 reset |
| `Timers.SentU(now, fn)` | `method` | U-frame gönderildi — activation ise t1 arm, t3 reset |
| `Timers.ReceivedI(now, outstanding)` | `method` | I-frame alındı — ack uygula, t2 arm (ilk alımda), t3 reset |
| `Timers.ReceivedS(now, outstanding)` | `method` | S-frame alındı — ack uygula, t3 reset |
| `Timers.ReceivedU(now, fn)` | `method` | U-frame alındı — confirmation ise t1 disarm, t3 reset |
| `Timers.Check(now)` | `method` | En acil action ve sonraki deadline döner. Mutasyon yapmaz |

**t0 bu pakette yoktur.** t0 TCP connect timeout'dur; connection layer dialer'a geçer.

#### `params.go` — APCI Parametreleri

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `DefaultK` | `const = 12` | Varsayılan k window (outstanding send limit) |
| `DefaultW` | `const = 8` | Varsayılan w window (unacked receive limit) |
| `DefaultT0` | `const = 30s` | TCP connect timeout |
| `DefaultT1` | `const = 15s` | Acknowledgement timeout |
| `DefaultT2` | `const = 10s` | Supervisory timeout |
| `DefaultT3` | `const = 20s` | Idle probe timeout |
| `Params` | `struct` | `K`, `W`, `T0`, `T1`, `T2`, `T3` |
| `DefaultParams()` | `func` | Standart önerilen parametreler |
| `Params.Validate()` | `method` | `w <= 2k/3` ve `t2 < t1` kurallarını kontrol eder |
| `Params.CanSend(outstanding)` | `method` | k window'a göre yeni I-frame gönderilebilir mi |
| `Params.AckDue(unackedReceived)` | `method` | w window'a göre S-frame gönderilmeli mi |

---

## 2. `internal/asdu/` — ASDU (Application Service Data Unit)

**Sorumluluk:** IEC 104 uygulama katmanı. ASDU header parse/encode, information object tipleri, quality descriptor, timestamp, komut encode.

Bu paket de tamamen **pure**'dir.

### Dosyalar

#### `asdu.go` — Temel parametreler ve hatalar

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `IOA` | `type uint32` | Information Object Address |
| `MaxObjects` | `const = 0x7F` | Variable structure qualifier'ın ifade edebileceği maksimum object sayısı |
| `Params` | `struct` | `CASDUOctets` (1-2), `COTOctets` (1-2), `IOAOctets` (1-3) |
| `DefaultParams()` | `func` | `{CASDU: 2, COT: 2, IOA: 3}` — IEC 104 standart değerleri |
| `Params.Validate()` | `method` | Alan genişliklerinin geçerli aralıkta olduğunu kontrol eder |
| `Params.HeaderLen()` | `method` | ASDU header'ın bayt uzunluğunu hesaplar |
| `Params.MaxCommonAddr()` | `method` | CASDU genişliğine göre maksimum common address |
| `Params.MaxIOA()` | `method` | IOA genişliğine göre maksimum IOA |

#### `header.go` — ASDU Header

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Header` | `struct` | `Type`, `Sequence` (SQ bit), `Count`, `Cause`, `Negative` (P/N), `Test` (T), `Originator`, `CommonAddr` |
| `Params.ParseHeader(b []byte)` | `method` | ASDU header parse eder, kalan body döner |
| `Header.AppendTo(dst []byte)` | `method` | Header encode eder |

**ASDU Header Yapısı:**
```
TypeID | VSQ | COT [originator] | CASDU
 1B     1B     1-2B              1-2B
```

- VSQ: bit 7 = SQ (sequence), bits 0-6 = object count
- COT: bit 6 = P/N (negative), bit 7 = T (test), bits 0-5 = cause

#### `typeid.go` — Type ID ve Cause of Transmission

**Monitor Yönü (Station → Master):**

| TypeID | Mnemonic | Açıklama |
|--------|----------|----------|
| 1 | `M_SP_NA_1` | Single point (bool) |
| 3 | `M_DP_NA_1` | Double point (4 durum) |
| 5 | `M_ST_NA_1` | Step position (-64..63) |
| 7 | `M_BO_NA_1` | Bitstring 32 bit |
| 9 | `M_ME_NA_1` | Measured normalised (-1..+1) |
| 11 | `M_ME_NB_1` | Measured scaled (-32768..32767) |
| 13 | `M_ME_NC_1` | Measured short float |
| 15 | `M_IT_NA_1` | Integrated totals (counter) |
| 20 | `M_PS_NA_1` | Packed single points |
| 30-37 | `M_SP_TB_1` vb. | CP56Time2a timestamp'li versiyonlar |

**Control Yönü (Master → Station):**

| TypeID | Mnemonic | Açıklama |
|--------|----------|----------|
| 45 | `C_SC_NA_1` | Single command |
| 46 | `C_DC_NA_1` | Double command |
| 47 | `C_RC_NA_1` | Regulating step command |
| 48-50 | `C_SE_NA_1` vb. | Set point (normalised, scaled, float) |
| 51 | `C_BO_NA_1` | Bitstring 32 bit |
| 58-64 | `C_SC_TA_1` vb. | CP56Time2a timestamp'li komutlar |

**Sistem Bilgisi:**

| TypeID | Mnemonic | Açıklama |
|--------|----------|----------|
| 70 | `M_EI_NA_1` | End of initialisation |
| 100 | `C_IC_NA_1` | Interrogation command (GI) |
| 101 | `C_CI_NA_1` | Counter interrogation |
| 102 | `C_RD_NA_1` | Read command |
| 103 | `C_CS_NA_1` | Clock synchronisation |
| 105 | `C_RP_NA_1` | Reset process |

**Cause of Transmission:**

| Cause | Açıklama |
|-------|----------|
| 1 | Periodic (cyclic) |
| 2 | Background scan |
| 3 | Spontaneous (değişim oldu) |
| 4 | Initialised |
| 5 | Request |
| 6 | Activation (komut gönderiliyor) |
| 7 | ActCon (komut kabul edildi/reddedildi) |
| 8 | Deactivation |
| 10 | ActTerm (komut sahadaki işlemi tamamladı) |
| 11 | RetRem (remote komut nedeniyle durum değişti) |
| 12 | RetLoc (local operatör nedeniyle durum değişti) |
| 20 | IntroGeneral (genel sorgulama yanıtı) |
| 21-36 | IntroGroup1-16 (grup sorgulama yanıtı) |
| 44-47 | Unknown type/cause/common-addr/IOA (reddetme nedenleri) |

#### `monitor.go` — Monitor Yönü Decode

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `ErrUnsupportedType` | `var error` | Desteklenmeyen TypeID — bağlantıyı kapatmaz, logla ve atla |
| `ValueKind` | `type` | `KindBool`, `KindFloat`, `KindInt` |
| `Point` | `struct` | Decode edilmiş bir information object: `Type`, `IOA`, `Kind`, `Bool`/`Float`/`Int`, `Q` (Quality), `Cause`, `Negative`, `Test`, `HasTime`, `Time`, `DoubleState`, `Transient`, `CounterSeq`/`CounterCarry`/`CounterAdjusted` |
| `Params.DecodeMonitor(b []byte)` | `method` | Monitor yönü ASDU body'yi parse eder, `[]Point` döner |

**layout tablosu** her TypeID için element uzunluğu ve decode fonksiyonunu tanımlar.

#### `control.go` — Control Yönü Encode

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `ErrCommand` | `var error` | Komut encode edilemiyor |
| `OutputDuration` | `type` | `DurationNone`, `DurationShortPulse`, `DurationLongPulse`, `DurationPersistent` |
| `DoubleCommand` | `type` | `DoubleOff` (1), `DoubleOn` (2) — **ÖNEMLİ: 1=OFF, 2=ON** |
| `StepCommand` | `type` | `StepLower` (1), `StepHigher` (2) |
| `TypeID.SelectBit(data byte)` | `method` | S/E bit'ini okur |
| `TypeID.HasQualifier()` | `method` | Qualifier octet'i var mı |
| `Params.AppendSingleCommand(...)` | `method` | C_SC_NA_1 encode eder |
| `Params.AppendDoubleCommand(...)` | `method` | C_DC_NA_1 encode eder |
| `Params.AppendRegulatingStepCommand(...)` | `method` | C_RC_NA_1 encode eder |
| `Params.AppendSetPointNormalised(...)` | `method` | C_SE_NA_1 encode eder |
| `Params.AppendSetPointScaled(...)` | `method` | C_SE_NB_1 encode eder |
| `Params.AppendSetPointShortFloat(...)` | `method` | C_SE_NC_1 encode eder |
| `Params.AppendBitstring32(...)` | `method` | C_BO_NA_1 encode eder |

**Kritik güvenlik:** `DoubleCommand`'da 1=OFF, 2=ON. Ters yazılırsa breaker yanlış yöne gider ve frame geçerli olur, hiçbir yerde hata raporlanmaz.

#### `quality.go` — Quality Descriptor

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Quality` | `struct` | `Overflow`, `Blocked`, `Substituted`, `NotTopical`, `Invalid` |
| `ParseQDS(b byte)` | `func` | QDS (quality descriptor with overflow) parse eder |
| `Quality.QDS()` | `method` | Quality'yi QDS byte'ına encode eder |
| `Quality.Good()` | `method` | Hiçbir flag ayarlı değil mi |
| `ParseSIQ(b byte)` | `func` | Single point information quality |
| `ParseDIQ(b byte)` | `func` | Double point information quality |
| `ParseBCR(b byte)` | `func` | Counter information quality |

**Quality bitleri:**
- `BL` (0x10): Blocked — cihaz tarafından transmission'a engellenmiş
- `SB` (0x20): Substituted — insan tarafından girilmiş, ölçülmemiş
- `NT` (0x40): Not Topical — cihaz yenileyemedi, eski değer
- `IV` (0x80): Invalid — cihaz değerin güvenilir olmadığını söylüyor

#### `time.go` — CP56Time2a Timestamp

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `CP56Time2aLen` | `const = 7` | 7 bayt tam zaman damgası |
| `centuryBase` | `const = 2000` | 2 haneli yıl 2000'e göre okunur |
| `CP56Time2a` | `struct` | `Millisecond`, `Minute`, `Hour`, `Day`, `DayOfWeek`, `Month`, `Year`, `Invalid`, `Summer` |
| `ParseCP56Time2a(b []byte)` | `func` | 7 bayttan CP56Time2a parse eder |
| `CP56Time2a.AppendTo(dst []byte)` | `method` | 7 bayta encode eder |
| `CP56Time2a.InRange()` | `method` | Tarih alanları geçerli aralıkta mı |
| `CP56Time2a.Time(loc *time.Location)` | `method` | `time.Time`'a dönüştürür (InRange olmalı) |

#### `system.go` — Sistem Bilgisi ASDU'ları

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `systemIOA` | `const = 0` | Station-addressed mesajların IOA adresi |
| `QOI` | `type` | Qualifier of interrogation: `QOIStation` (20), `QOIGroup1` (21)..`QOIGroup16` (36) |
| `Params.AppendInterrogation(...)` | `method` | C_IC_NA_1 encode eder |
| `Params.ParseInterrogation(...)` | `method` | C_IC_NA_1'den QOI okur |
| `FreezeAction` | `type` | `FreezeRead` (0), `FreezeNoReset` (1), `FreezeAndReset` (2) |
| `Params.AppendCounterInterrogation(...)` | `method` | C_CI_NA_1 encode eder |
| `Params.AppendRead(...)` | `method` | C_RD_NA_1 encode eder |
| `Params.AppendClockSync(...)` | `method` | C_CS_NA_1 encode eder |
| `Params.AppendResetProcess(...)` | `method` | C_RP_NA_1 encode eder |
| `Params.AppendEndOfInitialisation(...)` | `method` | M_EI_NA_1 encode eder |

#### `forbidden.go` — IEC 104'te Yasaklı Tipler

IEC 101'den miras kalan ama IEC 104'te kullanılmayan TypeID'leri tanımlar. Bu tiplerin parse edilmesine gerek yoktur; karşılaşıldığında `ErrUnsupportedType` döner.

---

## 3. `internal/conn/` — Connection Layer

**Sorumluluk:** Bir TCP bağlantısı üzerinde APCI state machine çalıştırır. 3 goroutine: `readLoop`, `runLoop`, `writeLoop`.

### Dosyalar

#### `conn.go` — Ana Bağlantı

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `ErrClosed` | `var error` | Close() çağrıldı — normal kapanış |
| `ErrProtocol` | `var error` | Peer standart dışı bir şey gönderdi — bağlantı bitti |
| `ErrTimeout` | `var error` | t1 expired — gönderilen acknowledge edilmedi |
| `ErrConfig` | `var error` | Geçersiz bağlantı parametreleri |
| `ErrNotStarted` | `var error` | STARTDT confirm olmadan ASDU gönderilmeye çalışıldı |
| `ErrControlBusy` | `var error` | STARTDT/STOPDT dialogue devam ederken ikinci talep |
| `Handler` | `interface` | `OnASDU(raw []byte)`, `OnU(fn UFunction)`, `OnClosed(err error)` |
| `Config` | `struct` | `Params` (apci.Params), `Clock`, `Handler`, `QueueDepth`, `WriteTimeout` |
| `Conn` | `struct` | Bir bağlantı: `sock`, `seq` (Sequencer), `timers` (Timers), `transfer` (dataTransfer), `pending` (sendRequest) |
| `New(sock, cfg)` | `func` | TCP socket üzerinde bağlantı başlatır, 3 goroutine oluşturur |
| `Conn.SendASDU(ctx, asdu)` | `method` | I-frame gönderir. k window doluysa block eder |
| `Conn.SendU(ctx, fn)` | `method` | U-frame gönderir. Confirmation beklemez |
| `Conn.Close()` | `method` | Bağlantıyı durdurur. Idempotent |
| `Conn.Done()` | `method` | Bağlantı durduğunda kapanan channel |
| `Conn.Err()` | `method` | Bağlantının neden bittiğini döner |

**3 Goroutine Mimarisi:**

```
readLoop:   socket → frame decode → recvCh channel
runLoop:    recvCh + sendCh + timer → sequencer + timers → writeCh
writeLoop:  writeCh → socket write (SINGLE WRITER)
```

- `readLoop`: Sadece socket'tan okur, frame decode eder, channel'a atar. State tutmaz, karar vermez.
- `runLoop`: Sequencer ve timer'ın tek sahibi. Gelen frame'i validate eder, handler'a teslim eder, ne gönderileceğine karar verir. k window back-pressure uygular.
- `writeLoop`: Tek yazar (single writer). N(S) ve N(R) tek goroutine'de atanır, lock gerekmez.

#### `control.go` — STARTDT/STOPDT Dialogue

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `dataTransfer` | `type` | `transferStopped`, `transferStarting`, `transferStarted`, `transferStopping` |
| `Conn.Start(ctx)` | `method` | STARTDT handshake yapar, confirmation'a kadar block eder |
| `Conn.Stop(ctx)` | `method` | STOPDT handshake yapar, data transfer durur, TCP açık kalır |
| `Conn.Shutdown(ctx)` | `method` | Önce STOPDT, sonra Close — kibar kapanış |

**STARTDT Neden Zorunlu:** Controlled station STARTDT con gönderene kadar **hiç** I-frame göndermez. Bu handshake atlanırsa TCP bağlantısı sağlıklı görünür ama veri akışı hiç başlamaz.

**State Makinesi:**
```
transferStopped → STARTDT act → transferStarting → STARTDT con → transferStarted
transferStarted → STOPDT act → transferStopping → STOPDT con → transferStopped
```

---

## 4. `internal/station/` — Station Lifecycle

**Sorumluluk:** Bir station'ın yaşam döngüsü: dial, handshake, connection, reconnect, backoff, general interrogation, clock sync, komut yönetimi.

### Dosyalar

#### `station.go` — Station Yapısı

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `sender` | `type func` | Bir ASDU gönderir (test edilebilirlik için interface değil) |
| `Dialer` | `interface` | `DialContext(ctx, network, address)` — `*net.Dialer` satisfies |
| `Gate` | `interface` | `Acquire(ctx)` — Fleet-wide rate limiting |
| `NoGate` | `struct` | Her şeyi hemen izin verir (test ve tek station için) |
| `Observer` | `interface` | `OnState`, `OnASDU`, `OnEvent`, `OnGI`, `OnClockSkew`, `OnClockSync`, `OnCommand`, `OnAnomaly` |
| `Config` | `struct` | `Address`, `Params`, `ASDUParams`, `CommonAddress`, `Originator`, `GI`, `ClockSync`, `Clock`, `Observer`, `Dialer`, `DialGate`, `GIGate`, `Backoff`, `Commands`, `Chatter` |
| `Station` | `struct` | Bir station'ın durumu ve yaşam döngüsü |
| `New(cfg)` | `func` | Station oluşturur |
| `Station.Start(ctx)` | `method` | Bağlantı döngüsünü başlatır |
| `Station.Close()` | `method` | Station'ı durdurur |
| `Station.SendCommand(ctx, cmd)` | `method` | Komut gönderir |
| `Station.State()` | `method` | Mevcut state döner |

#### `state.go` — State Makinesi

| State | Açıklama |
|-------|----------|
| `StateDisconnected` | Dinlenme durumu — socket yok |
| `StateQueued` | Dial etmek istiyor, Gate izni bekliyor |
| `StateConnecting` | TCP dial yapılıyor, t0 ile sınırlı |
| `StateStarted` | Bağlantı up, STARTDT confirm. Veri akabilir ama cache boş |
| `StateInterrogating` | Genel sorgulama devam ediyor, cache doluyor |
| `StateTopical` | Cache station'ı yansıtıyor — tek güvenilir state |
| `StateFailed` | Bir şey bozuldu, backoff'tan önce |
| `StateReleased` | Terminal — station bırakıldı |

**Geçişler:**
```
Disconnected → Queued → Connecting → Started → Interrogating → Topical
                                                              ↕ (periodic re-GI)
Her state → Failed → Disconnected (backoff ile)
Her state → Released (terminal)
```

#### `gi.go` — General Interrogation

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `ErrGIRefused` | `var error` | Station P/N ile reddetti — common address yanlış olabilir |
| `ErrGITimeout` | `var error` | Bracket kapanmadı — cache bilinmeyen boyutta kısmi |
| `GI` | `struct` | `OnConnect`, `Period`, `Timeout` |
| `DefaultGI()` | `func` | `{OnConnect: true, Timeout: 60s}` |

**GI Diyalog:**
```
master → station   C_IC_NA_1  COT=6  (activation), QOI=20
station → master   C_IC_NA_1  COT=7  (actcon)
station → master   ...        COT=20 (inrogen)   her bir point
station → master   C_IC_NA_1  COT=10 (actterm)
```

Actterm gelene kadar cache incomplete'tir. Actcon'u bitiş çizgisi olarak almak yarım snapshot'u current gibi gösterir.

#### `backoff.go` — Reconnect Backoff

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Backoff` | `struct` | `Min`, `Max`, `Factor`, `Jitter` |
| `DefaultBackoff()` | `func` | `{Min: 1s, Max: 30s, Factor: 2, Jitter: 0.25}` |
| `Backoff.Delay(attempt)` | `method` | Attempt sayısına göre gecikme döner |

Exponential backoff + jitter. 1000 station'ın aynı anda reconnect yapmasını engeller.

#### `cache.go` — Point Cache

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Cache` | `struct` | IOA → Point eşleştirmesi. GI ile dolar, spontaneous update'lerle güncellenir |
| `Cache.Get(ioa)` | `method` | Bir point'in son değerini döner |
| `Cache.Set(point)` | `method` | Bir point'i günceller |

#### `event.go` — Event Tespiti

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Event` | `struct` | Bir event: `IOA`, `Point`, `Cause` |
| `IsEvent(cause)` | `func` | Cause'a göre bir reading event mi yoksa GI yanıtı mı |

Spontaneous (cause=3), RetRem (cause=11), RetLoc (cause=12) event'tir. Interrogation yanıtı (cause=20..36) event değildir.

#### `chatter.go` — Contact Chatter Detection

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Chatter` | `struct` | `Window`, `Threshold` |
| `ChatterDetector` | `struct` | Bir contact'ın değişim sıklığını takip eder |

Bir contact'ın açık-kapalı arasında zıplamasını tespit eder. Measurand'ın deadband'i aşması chatter değildir.

#### `clock.go` — Clock Synchronisation

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `ClockSync` | `struct` | `Zone` (time zone), `SkewThreshold`, `SetClock` |
| `Station.syncClock(ctx, send)` | `method` | Cihaz saatini C_CS_NA_1 ile senkronize eder |

#### `command.go` — Komut Politikası

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Commands` | `struct` | `Enabled`, `SelectBeforeOperate`, `QueueDepth`, `Timeout` |
| `Station.SendCommand(ctx, cmd)` | `method` | Komut kuyruğuna ekler, sonucu bekler |

#### `anomaly.go` — Anomali Raporlama

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `AnomalyKind` | `type` | `AnomalyDuplicateIOA`, `AnomalyReservedType`, vb. |
| `Observer.OnAnomaly(kind, detail)` | `interface method` | Standart dışı ama bağlantıyı bitirmeyen durumlar |

---

## 5. `internal/command/` — Command Queue

**Sorumluluk:** Bir station'a komut gönderir, seri olarak. IEC 104'te komut için correlation ID yok; type + IOA ile eşleştirilir. Bu yüzden aynı station'a paralel komut gönderilemez.

### Dosyalar

#### `command.go` — Komut Kuyruğu

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `ErrQueueFull` | `var error` | Kuyruk dolu — komut gönderilmedi |
| `ErrClosed` | `var error` | Kuyruk kapanıyor |
| `ErrConfig` | `var error` | Geçersiz kuyruk konfigürasyonu |
| `Outcome` | `type` | `Succeeded`, `Rejected`, `Indeterminate`, `NotSent` |
| `Outcome.Safe()` | `method` | Retry güvenli mi? `NotSent` ve `Rejected` güvenli, `Indeterminate` DEĞİL |
| `ErrorCode` | `type` | `QueueFull`, `CallerGone`, `ShuttingDown` |
| `Result` | `struct` | `Outcome`, `ErrorCode`, `Duration` |
| `Queue` | `struct` | Bir station'ın komut kuyruğu |
| `NewQueue(...)` | `func` | Komut kuyruğu oluşturur |
| `Queue.Enqueue(ctx, cmd)` | `method` | Komut kuyruğa ekler |
| `Queue.Close()` | `method` | Kuyruğu kapatır |

**ÖNEMLİ:** `Indeterminate` otomatik retry edilmemeli. Komut gitmiş ama cevap gelmemiş — sahada output hareket etmiş olabilir. Tekrar göndermek breaker'ı ikinci kez çalıştırır.

#### `dialogue.go` — Komut Diyalog

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Sender` | `type func` | Encode edilmiş ASDU gönderir |
| `Reply` | `struct` | `Type`, `IOA`, `Cause`, `Negative`, `Select`, `HasSelect` |
| `ParseReply(p, h, body)` | `func` | Control yönü ASDU'dan Reply çıkarır |
| `runDirectOperate(...)` | `func` | Direct operate diyalog: activation → confirmation → termination |
| `runSelectBeforeOperate(...)` | `func` | Select-before-operate: select → select con → operate → operate con → termination |

**Diyalog Akışı (Direct Operate):**
```
master → station   Type=45  COT=6  (activation)     S/E=0 (operate)
station → master   Type=45  COT=7  (actcon)         P/N=0 (kabul)
station → master   Type=45  COT=10 (actterm)        (opsiyonel — her cihaz göndermez)
```

**Diyalog Akışı (Select-Before-Operate):**
```
master → station   Type=45  COT=6  (activation)     S/E=1 (select)
station → master   Type=45  COT=7  (actcon)         P/N=0
master → station   Type=45  COT=6  (activation)     S/E=0 (operate)
station → master   Type=45  COT=7  (actcon)         P/N=0
station → master   Type=45  COT=10 (actterm)        (opsiyonel)
```

---

## 6. `internal/clock/` — Time Abstraction

**Sorumluluk:** Zamanı bir interface arkasına alır ki timer-driven davranışlar deterministik test edilebilsin.

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `Clock` | `interface` | `Now()`, `Since(t)`, `NewTimer(d)`, `NewTicker(d)`, `After(d)` |
| `Timer` | `interface` | `C()`, `Stop()`, `Reset(d)` |
| `Ticker` | `interface` | `C()`, `Stop()`, `Reset(d)` |
| `System()` | `func` | Gerçek `time` paketini kullanan Clock |

`Sleep` yok — goroutine block etmeyi engellemek için.

#### `fake.go` — Fake Clock (Test)

| Sembol | Tür | Açıklama |
|--------|-----|----------|
| `NewFake()` | `func` | Başlangıç zamanı ile fake clock |
| `FakeClock.Set(t)` | `method` | Saati ilerletir, bekleyen timer'ları tetikler |
| `FakeClock.Adjust(d)` | `method` | Saati d kadar ilerletir |

---

## 7. `gen/iec104v1/` — gRPC Generated Code

Protobuf'tan otomatik üretilen kod. Manuel düzenlenmemeli.

- `iec104_manager.pb.go` — Message tipleri
- `iec104_manager_grpc.pb.go` — gRPC client/server stubları

---

## Bağlantı Katmanını Taşımak İçin

Bağlantı katmanını başka bir projeye almak için şu paketleri kopyalayın:

```
internal/apci/     — Frame, sequence, timer (pure, bağımlılık yok)
internal/asdu/     — ASDU encode/decode (pure, bağımlılık yok)
internal/conn/     — TCP connection layer (apci + clock bağımlılığı)
internal/clock/    — Time abstraction (stdlib bağımlılığı)
```

`station/` ve `command/` paketleri uygulama katmanıdır; bağlantı katmanı değildir. Sadece üzerinde çalışmak istiyorsanız ekleyin.

### Minimum Çalışan Örnek

```go
package main

import (
    "context"
    "net"
    "time"

    "internal/apci"
    "internal/asdu"
    "internal/clock"
    "internal/conn"
)

type myHandler struct{}

func (h myHandler) OnASDU(raw []byte) {
    // Gelen ASDU'yu parse et
    p := asdu.DefaultParams()
    header, body, err := p.ParseHeader(raw)
    if err != nil {
        return
    }
    points, err := p.DecodeMonitor(header, body)
    // ... points'i işle
}

func (h myHandler) OnU(fn apci.UFunction) {
    // U-frame kontrol mesajı
}

func (h myHandler) OnClosed(err error) {
    // Bağlantı kapandı
}

func main() {
    ctx := context.Background()

    // TCP bağlantı
    d := &net.Dialer{Timeout: 30 * time.Second}
    sock, err := d.DialContext(ctx, "tcp", "192.168.1.100:2404")
    if err != nil {
        panic(err)
    }

    // Connection oluştur
    c, err := conn.New(sock, conn.Config{
        Params: apci.DefaultParams(),
        Clock:  clock.System(),
        Handler: myHandler{},
    })
    if err != nil {
        panic(err)
    }

    // STARTDT handshake
    if err := c.Start(ctx); err != nil {
        panic(err)
    }

    // ASDU gönder (örnek: genel sorgulama)
    p := asdu.DefaultParams()
    gi, _ := p.AppendInterrogation(nil, 1, 0, asdu.CauseActivation, asdu.QOIStation)
    if err := c.SendASDU(ctx, gi); err != nil {
        panic(err)
    }

    // ... bekle ve verileri al

    // Kapanış
    c.Shutdown(ctx)
}
```

---

## Test Dosyaları

Her pakette `_test.go` dosyaları bulunur:

| Paket | Test Dosyaları |
|-------|---------------|
| `apci/` | `frame_test.go`, `params_test.go`, `seq_test.go`, `timers_test.go`, `purity_test.go` |
| `asdu/` | `control_test.go`, `cot_test.go`, `fuzz_test.go`, `header_test.go`, `monitor_test.go`, `quality_test.go`, `system_test.go`, `time_test.go`, `purity_test.go` |
| `conn/` | `conn_test.go`, `control_test.go`, `arch_test.go` |
| `station/` | `anomaly_test.go`, `backoff_test.go`, `cache_test.go`, `chatter_test.go`, `clock_test.go`, `command_test.go`, `event_test.go`, `gi_test.go`, `integration_test.go`, `state_test.go`, `station_test.go` |
| `command/` | `command_test.go`, `dialogue_test.go`, `outcome_test.go` |
| `clock/` | `clock_test.go` |

`purity_test.go` dosyaları paketin pure olduğunu (dış bağımlılık içermediğini) compile-time'da doğrular.

---

## Önemli Tasarım Kararları

1. **Pure Paketler:** `apci` ve `asdu` paketleri tamamen pure. Network, log, context yok. Bu sayede golden test ile byte-by-byte doğrulama yapılabilir.

2. **Single Writer Rule:** `writeLoop` tek yazar. N(S) ve N(R) tek goroutine'de atanır, mutex gerekmez.

3. **Sequence Number Wrap:** 15-bit sequence number 32767'den 0'a wrap eder. Karşılaştırmalar modular aritmetik (`seqDistance`) ile yapılır.

4. **Indeterminate ≠ Failed:** Komut gönderildi ama cevap gelmediyse `Indeterminate` döner. Otomatik retry yapılmaz çünkü sahada output hareket etmiş olabilir.

5. **STARTDT Zorunlu:** Handshake olmadan controlled station I-frame göndermez.

6. **GI Bracket:** Actcon değil, actterm cache'in tamamlandığını gösterir.

7. **DoubleCommand: 1=OFF, 2=ON:** Ters yazılırsa breaker yanlış yöne gider, frame geçerli olur.
