# Deep Trader — Bug ve Iyilestirme Listesi

**Kaynak:** Code review + double-check (v4.4, 2026-04-15)

---

## P0 — Acil Duzeltilmesi Gerekenler

### BUG-000: DrainPendingExits cooldown bypass
- **Dosya:** `internal/signal/engine.go:70-97`
- **Sorun:** DrainPendingExits pozisyonu tracker'dan siliyor. Ayni loop iterasyonunda ayni sembol icin HasPosition() false donuyor ve yeni entry sinyali uretiliyor. Cooldown henuz set edilmemis (executor'da exit islendikten sonra set ediliyor).
- **Akis:** DrainPendingExits sil → HasPosition false → sinyal uret → executor exit isle → cooldown set et → AMA entry zaten gonderilmis!
- **Etki:** Hard stop cooldown (10m/30m) tamamen bypass ediliyor. Tam da korunmasi gereken senaryoda.
- **Cozum:** DrainPendingExits'te pozisyon silindikten sonra cooldown'u da hemen set et. Veya drain edilen sembolleri ayni iterasyonda entry'den haric tut.

### BUG-001: VolumeAnalyzer hic resetlenmiyor
- **Dosya:** `internal/analyzer/volume.go:51-55`
- **Sorun:** `AddVolume()` ile `curVolumes` suresiz birikiyor. `ResetCurrent()` metodu var ama hicbir yerden cagrilmiyor.
- **Etki:** Saatler gectikce VolumeRatio sonsuza yaklasir, volume bazli tum sinyaller anlamsizlasir. PUMP sinyalinin cok fazla tetiklenmesinin bir sebebi bu olabilir.
- **Cozum:** `curVolumes`'u periyodik olarak sifirla (emit_interval veya 1dk pencereler ile).

### BUG-002: TREND_FOLLOW yon belirsizligi — her zaman LONG
- **Dosya:** `internal/signal/rules.go:93-99`, `internal/executor/paper.go:242-244`
- **Sorun:** TREND_FOLLOW sinyali yon belirlemeden uretiyor. Executor'da `DUMP degilse long` mantigi var. Dusus trendinde bile LONG aciliyor.
- **Etki:** Dusus piyasasinda TREND_FOLLOW zarara girer. Su an karli cikmasi piyasa yukseliste oldugu icin.
- **Cozum:** Fiyat degisimi veya funding rate yonune gore side belirle (pozitif = long, negatif = short).

### BUG-003: GitHub repo PUBLIC — DB sifresi acikta
- **Sorun:** `config.yaml` dosyasinda `password: deeptrader2024` plaintext. Repo public olarak olusturuldu.
- **Etki:** Herkes DB'ye erisebilir.
- **Cozum:** GitHub'da repo'yu private yap. Config'i .gitignore'a ekle. Sifreleri env variable'a tasi.

---

## P1 — Onemli Duzeltmeler

### BUG-004: SavePaperTrade quantity=0 ve balanceAfter=0
- **Dosya:** `internal/store/timescale.go:327`, `cmd/trader/main.go:218`
- **Sorun:** quantity `0.0` hardcoded yaziliyor. Ayrica `balanceAfter` main.go'da `0` geciriliyor. Root cause: `models.PaperTrade` struct'inda Quantity field'i yok (Position'da var ama trade'e aktarilmiyor).
- **Etki:** DB'deki `paper_trades.quantity` ve `balance_after` her zaman 0. Pozisyon buyuklugu ve bakiye takibi yapilamaz.
- **Cozum:** PaperTrade struct'ina Quantity ekle, executor'da doldur, store'da yaz. balanceAfter'i de executor'dan gec.

### BUG-009: Executor duplicate-position guard yok
- **Dosya:** `internal/executor/paper.go:186-244`
- **Sorun:** Entry sinyali geldiginde mevcut pozisyon kontrolu yapilmiyor. Ayni sembol icin positions[symbol] overwrite edilir, eski margin kaydi kaybolur, lockedMargin cift sayilir.
- **Etki:** Kalici locked margin leak'i. Bakiye gercekten kullanilabirden az gorunur.
- **Cozum:** Entry oncesi `pe.positions[signal.Symbol]` varsa reddet veya once kapat.

### BUG-010: Paper Router LIMIT bottleneck
- **Dosya:** `internal/router/paper.go:97,158`
- **Sorun:** LIMIT 100 depth + LIMIT 500 trade per poll. 91 sembol x depth20@100ms = ~4,550 kayit/5sn. LIMIT 100 ile sadece %2 okunuyor.
- **Etki:** Veri kaybi, paper modu zamanla gercek zamandan geri kalir.
- **Cozum:** LIMIT kaldirip cursor-based pagination kullan (son okunan timestamp ile).

### BUG-011: Stale timeout backtest'te calismiyor
- **Dosya:** `internal/signal/tracker.go` (UpdatePrice ve Evaluate)
- **Sorun:** UpdatePrice'da `time.Since(entryTime)` wall clock kullanir. Evaluate'de `eventTime.Sub(entryTime)` kullanir ama entry time wall clock'tan geliyor, event time historical. Fark negatif cikiyor.
- **Etki:** Backtest'te stale timeout hic tetiklenmiyor. Paper modda dogru calisiyor.
- **Cozum:** Entry time'i event timestamp ile kaydet (zaten kismen yapildi ama tutarsiz).

### BUG-005: isDuplicate() cagrilmiyor — sinyal cooldown devre disi
- **Dosya:** `internal/signal/engine.go:199-213`
- **Sorun:** Confirm delay eklenirken isDuplicate cagrisi kaldirildi. Fonksiyon tanimi var ama Run() dongusunde cagrilmiyor.
- **Etki:** Ayni sembol icin ayni sinyal tekrar tekrar uretilir. HasPosition kontrolu yeni pozisyon acmayi onluyor ama gereksiz islem yukleri olusturuyor.
- **Cozum:** isDuplicate()'i confirm delay'den sonra tekrar ekle, veya tamamen sil.

### BUG-006: Kural motoru duplikasyonu
- **Dosya:** `internal/signal/tracker.go:calculateScores()` ve `internal/signal/rules.go:Evaluate()`
- **Sorun:** Neredeyse ayni skor hesaplama mantigi iki yerde. TREND_FOLLOW tracker'da eksik.
- **Etki:** Bir kural degisikligini iki yere uygulamak gerekiyor. Sync hatasi kacinilmaz.
- **Cozum:** Tek bir `calculateScores()` fonksiyonu olustur, her ikisi de onu kullansin.

### BUG-007: Restart'ta pozisyon kaybi
- **Sorun:** Tracker tamamen in-memory. Servis restart edilirse acik pozisyonlar kaybolur, teminatlari serbest kalmaz.
- **Etki:** Hayalet teminat kilitleri olusur. Yeni pozisyon acilamaz.
- **Cozum:** Baslangicta DB'den acik pozisyonlari yukle (paper_trades'ten exit_time IS NULL olanlari).

### BUG-008: Backtest event siralamasi bozuk
- **Dosya:** `internal/router/backtest.go:77-80`
- **Sorun:** Her chunk'ta ONCE tum depth, SONRA tum trade gonderiliyor. Zaman siralamasi korunmuyor.
- **Etki:** Backtest sonuclari gercek paper performansindan sapabilir. Paper modda sorun yok.
- **Cozum:** Depth ve trade'leri tek sorguda birlestirip zaman sirasina gore gonder (performans maliyeti var).

---

## P2 — Iyilestirmeler

### IMP-001: Multi-timeframe trade flow
- **Dosya:** `internal/analyzer/analyzer.go:151`
- **Sorun:** Config'de 4 pencere var (30s, 1m, 5m, 15m) ama sadece ilk pencere (30s) kullaniliyor.
- **Fayda:** Coklu zaman dilimi confluence sinyal kalitesini artirabilir. 5m ve 15m pencereleri gurultuyu azaltir.

### IMP-002: Order book delta moving average
- **Dosya:** `internal/analyzer/orderbook.go:97-98`
- **Sorun:** BidDelta/AskDelta sadece "simdiki - onceki snapshot" farki. Tek snapshot arasi cok gurultulu.
- **Fayda:** Zaman pencereli moving average daha guvenilir sinyal uretir.

### IMP-003: Config'den credential cikarma
- **Sorun:** DB sifresi config.yaml'da plaintext.
- **Cozum:** `${DB_PASSWORD}` env variable destegi zaten config.go'da var, config.yaml'da kullan.

### IMP-004: Dashboard authentication
- **Sorun:** Web dashboard'da auth yok, ayni agdaki herkes erisebilir.
- **Cozum:** Basit basic auth veya token bazli erisim.

### IMP-005: Test suite
- **Sorun:** Hicbir unit test yok.
- **Fayda:** Ozellikle signal/tracker mantigi icin testler kritik. Kural degisikliklerinde regression onlenir.

---

## P3 — Temizlik

### CLN-001: Dead code temizligi
- `executor/backtest.go` — tamamen dead code (main.go backtest'i PaperExecutor'a yonlendiriyor)
- `store/timescale.go:56-116` — backfillDepthQuery, backfillTradeQuery, backfillQuery kullanilmiyor
- `store/timescale.go:119-173` — StreamBacktestEvents kullanilmiyor
- `signal/engine.go:199-213` — isDuplicate() cagrilmiyor (BUG-005 ile birlikte ele alinacak)
- `analyzer/tradeflow.go:137-147` — LastPrice() her zaman 0 donduruyor
- `analyzer/volume.go:58-62` — ResetCurrent() cagrilmiyor (BUG-001 ile birlikte ele alinacak)

### CLN-002: ~~Race condition riskleri~~ GERI CEKILDI
- ~~SpoofDetector race condition~~ → YANLIS. Tek goroutine, sirasel islem. Race yok.
- ~~Nested lock / PaperExecutor~~ → YANLIS. Lock siralamasi tutarli (pe.mu → st.mu). Deadlock riski yok.

### CLN-003: Paper Router LIMIT
- `internal/router/paper.go` — LIMIT 100 depth + 500 trade per poll. Yuksek frekansta veri kaybi olabilir.
- Cozum: Son okunan timestamp'e gore cursor-based pagination.

---

## Durum Takibi

| ID | Durum | Oncelik | Tarih |
|----|-------|---------|-------|
| BUG-000 | ACIK | P0 | 2026-04-15 |
| BUG-001 | ACIK | P0 | 2026-04-15 |
| BUG-002 | ACIK | P0 | 2026-04-15 |
| BUG-003 | ACIK | P0 | 2026-04-15 |
| BUG-004 | ACIK | P1 | 2026-04-15 |
| BUG-005 | ACIK | P1 | 2026-04-15 |
| BUG-006 | ACIK | P1 | 2026-04-15 |
| BUG-007 | ACIK | P1 | 2026-04-15 |
| BUG-008 | ACIK | P1 | 2026-04-15 |
| BUG-009 | ACIK | P1 | 2026-04-15 |
| BUG-010 | ACIK | P1 | 2026-04-15 |
| BUG-011 | ACIK | P1 | 2026-04-15 |
| IMP-001 | ACIK | P2 | 2026-04-15 |
| IMP-002 | GERI CEKILDI | - | 2026-04-15 |
| IMP-003 | ACIK | P2 | 2026-04-15 |
| IMP-004 | ACIK | P2 | 2026-04-15 |
| IMP-005 | ACIK | P2 | 2026-04-15 |
| CLN-001 | ACIK | P3 | 2026-04-15 |
| CLN-002 | GERI CEKILDI | - | 2026-04-15 |
| CLN-003 | ACIK | P3 | 2026-04-15 |
