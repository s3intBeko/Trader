# Deep Trader — Versiyon Gecmisi

## v3 — Zarar Toleransi + Hard Stop-Loss (2026-04-14)

### Problem
13 saatlik paper trading verisinde (1750 islem, +$33K kar):
- 728 zararli islemin 552'si hafif zarar (-%5 altinda)
- Bu hafif zararlarin %63'u 15dk icinde kara donuyordu
- Erken cikis nedeniyle $190K potansiyel kar kacirilmis

### Yeni Cikis Mantigi
Zarar toleransi sistemi: hafif zararlarda pozisyonu kapatmak yerine bekle.

| Zarar Bandi | Davranis |
|-------------|----------|
| %0 ile -%5 arasi | Tolere et, kapatma. Toparlanma sansi %63 |
| -%5 ile -%15 arasi | Sinyal/momentum kaybi varsa kapat, yoksa bekle |
| -%15 altinda | Hard stop-loss, hemen kapat |

Trailing stop ve kar koruma aynen devam ediyor:
- PnL %3-8: peak'ten %50 geri cekilirse kapat
- PnL %8-15: peak'ten %35 geri cekilirse kapat
- PnL %15+: peak'ten %25 geri cekilirse kapat

### Karar Oncelik Sirasi
1. Hard stop-loss (-%15) → hemen cik
2. Trailing stop (karda ise) → kari koru
3. Ters sinyal → karda veya agir zararda cik, hafif zararda tolere et
4. Sinyal zayifladi → karda veya agir zararda cik, hafif zararda tolere et
5. Momentum kaybi → karda veya agir zararda cik, hafif zararda tolere et

### Geriye Donuk Simulasyon
| Metrik | v2.2 (gercek) | v3 (simule) |
|--------|---------------|-------------|
| Toplam PnL | +$33,174 | +$223,832 |
| Hafif zararli islemler | -$31,711 | +$157,957 |

### Teknik
- `internal/signal/tracker.go`: lightLossMax (-5%), heavyLossMax (-15%), karar mantigi guncellendi
- v2.2 bug fixleri dahil (trailing stop fiyat beslemesi, cikis fiyati)

---

## v2 — Kademeli Trailing Stop + Net PnL (2026-04-13)

### Yeni Ozellikler
- **Kademeli trailing stop:** Kar buyudukce koruma sikilasir
  - PnL %0-3: trailing yok, pozisyon nefes alsin
  - PnL %3-8: peak'ten %50 geri cekilirse kapat
  - PnL %8-15: peak'ten %35 geri cekilirse kapat
  - PnL %15+: peak'ten %25 geri cekilirse kapat
- **Dashboard net PnL:** Fee dahil gercek kar/zarar gosterimi
  - Giris fee + tahmini cikis fee hesaplanir
  - Yesil/kirmizi gostergeler ile anlik yon
  - Pozisyon suresi gosterimi
- **Canli fiyat takibi:** Trade event'lerinden anlik fiyat dashboard'a yansir

### Duzeltmeler
- TREND_FOLLOW esigi 0.60 → 0.75 (fiyat hareketi + hacim kriteri eklendi)
- Tracker minimum bekleme 3 → 6 cycle (30sn, erken cikisi onler)
- Signal Engine giris sinyalinde fiyat loglaniyor

### Teknik
- `internal/signal/tracker.go`: PnL bazli kademeli trailing stop
- `internal/web/server.go`: Net PnL, fee gosterimi, canli fiyat
- `internal/signal/rules.go`: TREND_FOLLOW ek kriterler

---

## v1 — Temel Paper Trading Engine (2026-04-13)

### Ozellikler
- **91 sembol** TimescaleDB'den dinamik yukleme ($20M+ depth verisi olanlar)
- **Paper Router:** Collector'in DB'ye yazdigi verileri poll ederek okur (WS degil)
- **Analyzer:** Order book delta, trade flow imbalance, spoof detection, volume ratio, konsolidasyon tespiti
- **Signal Engine:** PUMP / DUMP / TREND_FOLLOW / NO_ENTRY sinyal siniflandirmasi
  - Kural bazli skorlama sistemi
  - 60 saniyelik duplicate sinyal cooldown'u
  - Sinyal gucu takibi (acik pozisyonlar icin)
- **Paper Executor:** 
  - 5x kaldirac destegi (config'den ayarlanabilir)
  - Taker fee %0.04 (pozisyon buyuklugu uzerinden)
  - Gunluk kayip limiti (%5)
- **Web Dashboard:** Port 8888, 2sn yenileme
  - Bakiye, PnL, acik pozisyonlar, son sinyaller, islem gecmisi
- **DB kayit:** paper_signals ve paper_trades tablolarina tum veriler kaydedilir
- **Systemd servisi:** CT 192.168.0.89 uzerinde otomatik restart

### Mimari
```
[Collector] → [TimescaleDB] → [Paper Router] → [Analyzer] → [Signal Engine] → [Paper Executor]
                                                                    ↑                    ↓
                                                              [Signal Tracker] ←── [Position Hook]
                                                                    ↓
                                                              [Web Dashboard :8888]
```

### Konfigurasyon
- DB: 192.168.0.238:5432/deeptrader_market
- Kaldirac: 5x
- Pozisyon buyuklugu: Sermayenin %10'u ($1000 teminat → $5000 pozisyon)
- Stop-loss: %2 (kaldiracli risk)
- Trade flow pencereleri: 30s, 1m, 5m, 15m
- Analyzer emit araligi: 5s

### Dosya Yapisi
```
deep-trader/
├── cmd/trader/main.go           # Giris noktasi
├── config/config.yaml           # Konfigurasyon
├── internal/
│   ├── analyzer/                # Order book, trade flow, spoof, volume
│   ├── config/config.go         # Config struct'lari
│   ├── executor/                # Paper, backtest, live executor
│   ├── models/events.go         # Tum veri modelleri
│   ├── router/                  # Paper (DB poll), backtest, live (WS)
│   ├── signal/                  # Rules, engine, tracker
│   ├── store/timescale.go       # TimescaleDB sorgulari
│   └── web/server.go            # Dashboard
└── VERSION.md                   # Bu dosya
```
