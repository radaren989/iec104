# github.com/radaren989/iec104 — kütüphane geliştirme hedefleri.
#
# Bu dosya KANONİK giriş noktasıdır (CI ve Unix). Windows için aynı hedefleri
# çalıştıran ince bir sarmalayıcı vardır: .\make.ps1 <hedef>
# İkisi de aynı `go` komutlarını çağırır; davranış farkı olmamalıdır. Bir hedefi
# burada değiştirirsen make.ps1'i de aynı commit'te güncelle.
#
# POSIX kabuk varsayar (sh/bash). Windows'ta make.ps1 kullan.

MODULE  := github.com/radaren989/iec104

GO         ?= go
TEST_FLAGS ?= -race -count=1

# --- Araç yolu ----------------------------------------------------------------
# `go install` ikilileri $(go env GOBIN)'e, o boşsa $(go env GOPATH)/bin'e düşer —
# ve o dizin çoğu ortamda PATH'te DEĞİLDİR (taze bir WSL/Ubuntu en sık karşılaşılan
# hâli). Sonuç kafa karıştırıcı oluyor: `make tools` her şeyi kurup başarıyla
# bitiyor, hemen ardından `make check` "golangci-lint bulunamadı" diyor.
#
# Bu yüzden dizini burada, her recipe için PATH'in BAŞINA ekliyoruz. Başına,
# çünkü sonuna eklemek PATH'te duran eski bir sürümün (örn. bir v1 golangci-lint)
# gölgelemesine izin verirdi. Kullanıcının kabuk yapılandırmasına dokunulmuyor;
# etki yalnızca bu make çağrısı boyunca sürüyor.
#
# `go env` boş dönebilir (go kurulu değil, ya da bir toolchain indirmesi
# başarısız). O durumda hiçbir şey eklenmiyor: koşulsuz bir `$(...)/bin` ifadesi
# boş GOPATH ile `/bin` üretir ve onu PATH'in başına koymak sistem ikililerinin
# sırasını değiştirirdi.
GO_TOOL_BIN := $(shell $(GO) env GOBIN 2>/dev/null)
ifeq ($(GO_TOOL_BIN),)
GO_GOPATH := $(shell $(GO) env GOPATH 2>/dev/null)
ifneq ($(GO_GOPATH),)
GO_TOOL_BIN := $(GO_GOPATH)/bin
endif
endif
ifneq ($(GO_TOOL_BIN),)
export PATH := $(GO_TOOL_BIN):$(PATH)
endif

# --- Araç sürümleri -----------------------------------------------------------
GOLANGCI_LINT_VERSION ?= latest


.DEFAULT_GOAL := help
.PHONY: help build test test-norace golden fuzz capture capture-update live refsim vet lint fmt fmt-check tidy check clean tools

help: ## Hedefleri listele
	@echo "github.com/radaren989/iec104 — hedefler:"
	@grep -E '^[a-z][a-z-]*:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Kalite kapısı: make check"

build: ## Bütün paketleri derle (kütüphane — binary yok)
	$(GO) build ./...

# -race cgo ISTER ("go: -race requires cgo") ve cgo bir C derleyicisi ister.
# Linux imajlarında gcc çoğu zaman vardır; Windows'ta yoktur (bkz. make.ps1
# Assert-RaceToolchain). CGO_ENABLED açıkça 1: kurulu bir derleyici yokken Go'nun
# varsayılanı 0'dır ve -race anlaşılmaz bir hatayla düşer.
test: ## Bütün testler + race detector
	CGO_ENABLED=1 $(GO) test ./... $(TEST_FLAGS)

# Kaçış kapısı, BİLEREK `check`in dışında ve bilerek gürültülü: bir C derleyicisi
# kurulana kadar testlerin hiç koşmamasından iyidir, kalite kapısının yerine geçmez.
test-norace: ## Testler race detector OLMADAN — kalite kapısı DEĞİL
	@echo "UYARI: -race KAPALI. Kalite kapısını karşılamaz; commit öncesi 'make test' koş."
	$(GO) test ./... -count=1

golden: ## Yalnızca protokol katmanının golden testleri
	$(GO) test ./apci/... ./asdu/... -run Golden -v -count=1

# Gerçek bir cihaza (simülatör ya da saha RTU'su) karşı koşan tek test.
#
# `check`in içinde DEĞİL, çünkü kalite kapısının bir cihazın açık olmasına bağlı
# olması onu kapı olmaktan çıkarır. Ama build tag ile de ayrılmadı: tag'li dosya
# derlenmez, vet edilmez, lint edilmez — sessizce çürür ve tam ihtiyaç duyulduğu
# gün bozuk bulunur. Bu dosya her derlemede derleniyor, yalnızca cihaz yoksa
# kendini atlıyor.
#
#   make live IEC104_SIM_ADDR=192.168.1.50:2404
#
# Ayarlanabilirler (hepsi opsiyonel): IEC104_SIM_CASDU (varsayılan 1),
# IEC104_SIM_ZONE (cihazın saatinin ayarlı olduğu IANA bölgesi),
# IEC104_SIM_CYCLES (kaç kez koparılacak, varsayılan 10),
# IEC104_SIM_WATCH (spontane veriyi ne kadar izleyeceği, varsayılan 15s),
# IEC104_SIM_IOAS (cache'ten geri okunacak adresler, "1,2,1001").
#
# -v şart: testin asıl çıktısı cihazın hangi tipleri gönderdiği ve GI'ın ne
# kadar sürdüğü — ikisi de t.Log ile basılıyor, çünkü ikisi de assert edilecek
# bir şey değil, öğrenilecek bir şey.
live: ## Gerçek bir cihaza karşı entegrasyon testi (IEC104_SIM_ADDR gerekir)
	@if [ -z "$$IEC104_SIM_ADDR" ]; then \
		echo "IEC104_SIM_ADDR ayarlı değil. Örnek: make live IEC104_SIM_ADDR=127.0.0.1:2404"; \
		exit 1; \
	fi
	CGO_ENABLED=1 $(GO) test ./test/live -run TestAgainstALiveDevice -v -count=1 -race -timeout 20m

# Bağımsız referans outstation (test/sim/outstation.py). Bir cihazla anlaşamadığımızda
# "kimin hatası" sorusunu cevaplayan tek şey: standarttan, ayrı dilde, ortak kod olmadan.
# MODE=strict|freyr|no-actcon|no-actterm|silent
MODE ?= strict
SIM_ADDR ?= 127.0.0.1:2404

refsim: ## Referans outstation'ı başlat (MODE=strict SIM_ADDR=127.0.0.1:2404)
	python3 test/sim/outstation.py --addr $(SIM_ADDR) --mode $(MODE)

# Fuzz hedeflerini sırayla koşturur. `go test -fuzz` aynı anda YALNIZCA BİR hedef
# alıyor, bu yüzden döngü şart — tek komutta hepsini vermek sessizce ilkini koşup
# geri kalanını atlar.
#
# Neden ayrı bir hedef: fuzz testi koşulmazsa yazılmamış sayılır. `check` bunları
# yalnızca seed korpusuyla çalıştırıyor (saniyeler); asıl arama bu hedefin işi ve
# CI'da gecelik koşacak. FUZZTIME ile süre ayarlanır.
FUZZTIME ?= 30s
FUZZ_PKGS ?= ./apci ./asdu

capture: ## Gerçek RTU yakalamasını yeniden oynat (test/pcap/)
	$(GO) test ./test/capture -v -count=1

capture-update: ## Yakalama özetini yeniden üret — DİFF'İ OKU, sonra commit'le
	$(GO) test ./test/capture -run SummaryMatchesGolden -update -count=1
	@echo "test/golden/capture-summary.txt güncellendi. 'git diff' ile NE değiştiğini oku."

fuzz: ## Bütün fuzz hedeflerini sırayla koştur (FUZZTIME=30s)
	@for pkg in $(FUZZ_PKGS); do \
		for target in $$($(GO) test $$pkg -list 'Fuzz.*' | grep '^Fuzz'); do \
			echo ">> $$pkg $$target ($(FUZZTIME))"; \
			$(GO) test $$pkg -run=XXX -fuzz="^$$target$$" -fuzztime=$(FUZZTIME) || exit 1; \
		done; \
	done
	@echo "fuzz: hepsi temiz"

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint (yapılandırma: .golangci.yml — T1.4)
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint bulunamadı; 'make tools' çalıştır."; exit 1; }
	golangci-lint run

# Biçimlendirilen kod: YALNIZCA bizim yazdığımız dizinler.
# Yeni bir üst düzey Go dizini eklenirse buraya da eklenir.
FMT_PATHS := $(wildcard apci asdu clock command conn station test)

fmt: ## gofmt + goimports ile biçimlendir (yazar)
	gofmt -w $(FMT_PATHS)
	@command -v goimports >/dev/null 2>&1 && goimports -w -local $(MODULE) $(FMT_PATHS) || \
		echo "uyarı: goimports yok, atlandı ('make tools')."

fmt-check: ## Biçim bozuksa başarısız ol (CI ve kalite kapısı için)
	@out="$$(gofmt -l $(FMT_PATHS) || true)"; \
	if [ -n "$$out" ]; then echo "gofmt gerekiyor:"; echo "$$out"; exit 1; fi
	@if command -v goimports >/dev/null 2>&1; then \
		out="$$(goimports -l -local $(MODULE) $(FMT_PATHS) || true)"; \
		if [ -n "$$out" ]; then echo "goimports gerekiyor:"; echo "$$out"; exit 1; fi; \
	else echo "uyarı: goimports yok, kontrol atlandı ('make tools')."; fi

tidy: ## go mod tidy (ağ erişimi gerektirir)
	$(GO) mod tidy


check: fmt-check vet lint test ## KALİTE KAPISI: biçim + vet + lint + test
	@echo "check: tamam"

# golangci-lint v2 AYRI bir modül yolunda: .../golangci-lint/v2/cmd/... . Eski yol
# v1'i kurar ve v2 şemasındaki .golangci.yml'yi okuyamaz. Kurduktan sonra hâlâ
# v1 hatası alıyorsan PATH'te eski bir ikili vardır: `which -a golangci-lint`.
tools: ## Geliştirme araçlarını kur (GOBIN'e)
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install golang.org/x/tools/cmd/goimports@latest

clean: ## Geçici dosyaları sil
	@true
