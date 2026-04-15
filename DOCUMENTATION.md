# Deep Trader — Teknik Dokumantasyon

**Versiyon:** v4.5 | **Tarih:** 2026-04-15 | **Dil:** Go 1.22

---

## 1. Proje Ozeti

Deep Trader, Binance USDT-M Futures piyasalarinda order book derinligi, trade flow ve piyasa yapisina dayali sinyal ureten bir kripto trading motorudur. Sistem uc modda calisir: **Backtest** (gecmis veri), **Paper** (canli simülasyon), **Live** (gercek islem — henuz implement edilmedi).

### Temel Ozellikler
- 75-91 sembol esanli izleme ($20M+ hacimli)
- Kural bazli sinyal motoru: PUMP, DUMP, TREND_FOLLOW
- Event-time backtest: dinamik tarihsel sembol evreni + ham `agg_trades`
- Sliding-window trade flow imbalance
- Kademeli trailing stop ile kar koruma
- Zarar toleransi (hafif zararlarda toparlanma bekleme)
- Hard stop-loss ile likidasyon simülasyonu
- Gercekci fee hesaplama (taker %0.04)
- Web dashboard (port 8888) ile canli izleme, backtest'te simule zaman
- Tum sinyaller ve islemler TimescaleDB'ye kaydedilir

### Backtest Notlari
- Sembol evreni, secilen araliktaki ilk `depth_snapshots.time` degerine gore olusur.
- Trade backtest'i 1 saniyelik kaba bucket yerine ham `agg_trades` satirlariyla calisir.
- `backtest.speed`, gercek zaman carpani olarak uygulanir: `100.0` = `100x realtime`, `<= 0` = sinirsiz hiz.

---

## 2. Mimari

```
[Binance Collector] (ayri sistem, dokunulmaz)
        |
        v
[TimescaleDB 950GB] ─── depth_snapshots, agg_trades, klines, funding_rates ...
        |
        ├── Paper Modu: [Paper Router] DB'den poll + acilista depth prime
        ├── Backtest Modu: [Backtest Router] zaman sirali chunk merge + speed pacing
        └── Live Modu: [Live Router] Binance WebSocket (henuz aktif degil)
                |
                v
        [Analyzer] ─── Order Book Delta, Sliding Trade Flow, Spoof Detection, Volume
                |
                v
        [Signal Engine] ─── Kural motoru (PUMP/DUMP/TREND_FOLLOW/NO_ENTRY)
                |            + Sinyal cooldown (60sn)
                |            + Hard stop cooldown (kademeli)
                |
                v
        [Signal Tracker] ─── Acik pozisyon izleme
                |              Trailing stop, zarar toleransi, stale timeout
                |              Hard stop-loss, likidasyon
                |
                v
        [Paper Executor] ─── Teminat yonetimi, fee hesaplama
                |              Pozisyon acma/kapama
                |
                v
        [Web Dashboard :8888] ─── Canli bakiye, PnL, pozisyonlar, sinyaller
        [TimescaleDB] ─── paper_signals, paper_trades tablolari
```

---

## 3. Dosya Yapisi

```
deep-trader/
├── cmd/trader/
│   └── main.go                     # Giris noktasi, flag parsing, dependency injection
├── config/
│   ├── config.yaml                 # Ana konfigurasyon (paper mod icin)
│   ├── backtest.yaml               # Backtest konfigurasyon
│   └── bt_*.yaml                   # A/B test konfigurasyonlari
├── internal/
│   ├── config/
│   │   └── config.go               # Config struct tanimlari, YAML okuma
│   ├── models/
│   │   └── events.go               # Tum veri modelleri (MarketEvent, SignalEvent, Position...)
│   ├── store/
│   │   └── timescale.go            # TimescaleDB baglantisi, sorgular, tablo olusturma
│   ├── router/
│   │   ├── router.go               # DataRouter interface
│   │   ├── paper.go                # Paper modu: DB'den periyodik poll
│   │   ├── backtest.go             # Backtest: zaman sirali gecmis veri akisi + speed pacing
│   │   └── live.go                 # Live: Binance WebSocket (henuz aktif degil)
│   ├── analyzer/
│   │   ├── analyzer.go             # Ana koordinator, cache yonetimi
│   │   ├── orderbook.go            # Order book delta, buyuk emir tespiti, BidAskRatio
│   │   ├── tradeflow.go            # Sliding-window trade flow imbalance
│   │   ├── volume.go               # 7 gunluk ortalama hacim, volume ratio
│   │   └── spoof.go                # Spoof tespit (30sn'den kisa yasayan buyuk emirler)
│   ├── signal/
│   │   ├── types.go                # SignalType enum (PUMP, DUMP, TREND_FOLLOW, NO_ENTRY)
│   │   ├── rules.go                # Kural motoru, skorlama sistemi
│   │   ├── engine.go               # Sinyal koordinatoru, cooldown, sure bazli confirmation
│   │   └── tracker.go              # Acik pozisyon izleme, cikis kararlari
│   ├── executor/
│   │   ├── executor.go             # Executor interface
│   │   ├── paper.go                # Paper trading: teminat, fee, likidasyon
│   │   └── backtest.go             # Backtest executor (eski, artik paper kullanilir)
│   └── web/
│       └── server.go               # Dashboard: HTTP API + HTML/JS frontend (backtest-time aware)
├── DOCUMENTATION.md                # Bu dosya
├── VERSION.md                      # Versiyon gecmisi
├── COLLECTOR_NOTES.md              # Collector bilgileri
├── go.mod / go.sum                 # Go bagimliliklari
└── .gitignore
```

---

## 4. Veritabani

### 4.1 Baglanti Bilgileri

```
Host:     192.168.0.238 (CT 200 — Proxmox LXC)
Port:     5432
Database: deeptrader_market
User:     deeptrader
Password: deeptrader2024

Go DSN:
  postgres://deeptrader:deeptrader2024@192.168.0.238:5432/deeptrader_market

psql:
  PGPASSWORD=deeptrader2024 psql -h 192.168.0.238 -U deeptrader -d deeptrader_market
```

### 4.2 Collector Tablolari (Deep Trader OKUR, YAZMAZ)

| Tablo | Aciklama | Veri Araligi | Satir |
|-------|----------|-------------|-------|
| `agg_trades` | Tick-level islemler | 2022-03 ~ simdi | ~21B |
| `depth_snapshots` | Top 20 bid/ask (array) | 2026-04-10 ~ simdi | ~130M |
| `klines_1m` | 1dk OHLCV mumlar | 2022-03 ~ simdi | ~153M |
| `klines_1d` | Gunluk OHLCV (continuous aggregate) | klines_1m'den | otomatik |
| `funding_rates` | Funding rate (8 saatte bir) | 2022-02 ~ simdi | ~457K |
| `market_metrics` | OI + L/S ratio (5dk poll) | 2022-03 ~ simdi | ~32M |
| `book_depth_hist` | Tarihsel derinlik (CSV import) | 2023-01 ~ 2026-03 | ~2.8B |
| `mark_price_klines` | Mark/index fiyat (CDN backfill) | 2020-01 ~ 2026-03 | ~330M |
| `depth_diffs` | Order book degisim (WS live) | 2026-04-13 ~ simdi | yeni |
| `book_tickers` | Best bid/ask (WS live) | 2026-04-13 ~ simdi | yeni |
| `mark_prices` | Anlik mark price (WS live) | 2026-04-13 ~ simdi | yeni |
| `liquidations` | Likidasyon eventleri (WS live) | 2026-04-13 ~ simdi | ~8K |

### 4.3 Deep Trader Tablolari (otomatik olusturulur)

```sql
-- Sinyal kayitlari
CREATE TABLE IF NOT EXISTS paper_signals (
    id              SERIAL,
    run_id          TEXT,           -- "paper_1776233718246" veya "backtest_..."
    run_mode        TEXT,           -- "paper" | "backtest"
    time            TIMESTAMPTZ DEFAULT NOW(),
    symbol          TEXT,
    signal          TEXT,           -- PUMP | DUMP | TREND_FOLLOW | NO_ENTRY
    confidence      DOUBLE PRECISION,
    source          TEXT,           -- "rules" | "tracker" | "tracker-urgent"
    reasons         TEXT[],
    mid_price       DOUBLE PRECISION,
    bid_ask_ratio   DOUBLE PRECISION,
    trade_imbalance DOUBLE PRECISION,
    volume_ratio    DOUBLE PRECISION,
    is_consolidating BOOLEAN,
    funding_rate    DOUBLE PRECISION
);

-- Islem kayitlari
CREATE TABLE IF NOT EXISTS paper_trades (
    id              SERIAL,
    run_id          TEXT,
    run_mode        TEXT,
    symbol          TEXT,
    side            TEXT,           -- "long" | "short"
    signal          TEXT,
    entry_time      TIMESTAMPTZ,
    exit_time       TIMESTAMPTZ,
    entry_price     DOUBLE PRECISION,
    exit_price      DOUBLE PRECISION,
    quantity        DOUBLE PRECISION,
    pnl             DOUBLE PRECISION,
    balance_after   DOUBLE PRECISION,
    reasons         TEXT[]          -- cikis sebebi
);

-- Backtest sonuclari (eski format, artik paper_trades kullanilir)
CREATE TABLE IF NOT EXISTS backtest_results (
    run_id       TEXT,
    symbol       TEXT,
    entry_time   TIMESTAMPTZ,
    exit_time    TIMESTAMPTZ,
    signal       TEXT,
    entry_price  DOUBLE PRECISION,
    exit_price   DOUBLE PRECISION,
    pnl          DOUBLE PRECISION,
    confidence   DOUBLE PRECISION,
    params       JSONB,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);
```

### 4.4 Yeni Ortamda Database Kurulumu

Yeni bir makinede deploy icin TimescaleDB gereklidir:

```bash
# 1. PostgreSQL + TimescaleDB kurulumu (Debian/Ubuntu)
sudo apt install postgresql postgresql-client
sudo add-apt-repository ppa:timescale/timescaledb-ppa
sudo apt install timescaledb-2-postgresql-16
sudo timescaledb-tune

# 2. Database ve kullanici olustur
sudo -u postgres psql
CREATE USER deeptrader WITH PASSWORD 'deeptrader2024';
CREATE DATABASE deeptrader_market OWNER deeptrader;
\c deeptrader_market
CREATE EXTENSION IF NOT EXISTS timescaledb;

# 3. Deep Trader tablolari otomatik olusturulur (uygulama basladiginda)
# Collector tablolari ayri kurulur (collector dokumantasyonuna bakin)
```

### 4.5 Faydali Sorgular

```sql
-- Canli paper performans
SELECT signal, count(*), round(sum(pnl)::numeric,2) as pnl
FROM paper_trades WHERE run_mode='paper' AND run_id = (
    SELECT run_id FROM paper_trades WHERE run_mode='paper'
    ORDER BY exit_time DESC LIMIT 1
) GROUP BY signal ORDER BY pnl DESC;

-- Son 20 islem
SELECT symbol, side, signal, round(pnl::numeric,2) as pnl,
       extract(epoch from exit_time - entry_time)::int as sure_sn,
       reasons[1] as sebep
FROM paper_trades WHERE run_mode='paper'
ORDER BY exit_time DESC LIMIT 20;

-- Sembol bazli performans
SELECT symbol, count(*) as adet, round(sum(pnl)::numeric,2) as pnl
FROM paper_trades WHERE run_mode='paper'
GROUP BY symbol ORDER BY pnl DESC;

-- Cikis tipi analizi
SELECT
  CASE
    WHEN reasons[1] LIKE '%trailing%' THEN 'trailing_stop'
    WHEN reasons[1] LIKE '%hard stop%' THEN 'hard_stop'
    WHEN reasons[1] LIKE '%LIKID%' THEN 'likidasyon'
    WHEN reasons[1] LIKE '%stale%' THEN 'stale'
    WHEN reasons[1] LIKE '%ters sinyal%' THEN 'ters_sinyal'
    WHEN reasons[1] LIKE '%momentum%' THEN 'momentum'
    WHEN reasons[1] LIKE '%zayifladi%' THEN 'sinyal_zayif'
    ELSE 'diger'
  END as tip,
  count(*) as adet,
  round(sum(pnl)::numeric,2) as pnl
FROM paper_trades WHERE run_mode='paper'
GROUP BY 1 ORDER BY adet DESC;

-- Backtest vs Paper karsilastirma
SELECT run_mode, count(*) as trade, round(sum(pnl)::numeric,2) as pnl
FROM paper_trades GROUP BY run_mode;
```

---

## 5. Sinyal Motoru

### 5.1 PUMP Sinyali (Long Giris)

Skor >= 0.65 ile tetiklenir:

| Kriter | Skor | Aciklama |
|--------|------|----------|
| Trade flow guclu alis (imbalance >= 0.70) | +0.35 | Gercek alislar satislardan fazla |
| Order book alis baskin (bid/ask >= 2.0) | +0.25 | Alis emirleri satis emirlerinin 2 kati |
| Hacim patladi, fiyat az hareket etti (vol >= 5x, price <= %15) | +0.30 | Birikim asamas |
| Alis birikimi devam ediyor (bidDelta > 0, askDelta < 0) | +0.10 | Momentum |
| Spoof tespit edildi | -0.15 | Guven dusurme |

### 5.2 DUMP Sinyali (Short Giris)

Skor >= 0.65 ile tetiklenir:

| Kriter | Skor | Aciklama |
|--------|------|----------|
| Trade flow guclu satis (imbalance <= 0.30) | +0.35 | Gercek satislar alislardan fazla |
| Order book satis baskin (bid/ask <= 0.5) | +0.25 | Satis emirleri alis emirlerinin 2 kati |
| Destek eriyor (bidDelta < 0, askDelta > 0) | +0.25 | Alis destegi kayboluyor |
| Spoof tespit edildi | -0.15 | Guven dusurme |

### 5.3 TREND_FOLLOW Sinyali

Skor >= 0.60 ile tetiklenir:

| Kriter | Skor | Aciklama |
|--------|------|----------|
| Konsolidasyon yok (7 gun, >%5 hareket) | +0.25 | Piyasa flat degil |
| Funding rate yuksek (>%0.1) | +0.15 | Trend gucu gostergesi |
| Fiyat hareketi guclu (>%2) | +0.20 | Aktif hareket var |
| Hacim ortalamanin 2x uzerinde | +0.15 | Ilgi artmis |

### 5.4 Sinyal Onceligi

```
1. PUMP (skor >= 0.65) → Long gir
2. DUMP (skor >= 0.65) → Short gir
3. TREND_FOLLOW (skor >= 0.60) → Trend yonunde gir
4. NO_ENTRY → Islem yapma
```

---

## 6. Pozisyon Yonetimi

### 6.1 Giris

```
Baslangic bakiye:  $100,000
Teminat:           %2 = $2,000 (sabit, baslangic bakiyesinden)
Kaldirac:          10x
Pozisyon:          $2,000 x 10 = $20,000
Max pozisyon:      50 esanli
```

TREND_FOLLOW icin ozel ayarlar yapilandirılabilir:
```yaml
trend_follow_leverage: 20      # 20x kaldirac
trend_follow_position_pct: 0.05 # %5 teminat = $5,000
```

### 6.2 Cikis Kararlari (Oncelik Sirasi)

| # | Karar | Kosul | Aksiyon |
|---|-------|-------|---------|
| 0 | Likidasyon | PnL <= -%80 | HEMEN kapat, max zarar = teminat |
| 1 | Hard stop-loss | PnL <= -%15 | HEMEN kapat + cooldown (1st:10dk, 2nd:30dk) |
| 2 | Trailing stop | Peak'ten geri cekilme | Kademeli koruma (asagida detay) |
| 3 | Stale pozisyon | 1 saat gecmis + ±%5 PnL | Kapat, teminati serbest birak |
| 4 | Ters sinyal | Ters skor >= 0.60 | Karda veya -%5 altinda kapat, hafif zararda bekle |
| 5 | Sinyal zayif | Bizim skor < 0.30 | Karda veya -%5 altinda kapat, hafif zararda bekle |
| 6 | Momentum kaybi | Peak skor - suan >= 0.40 | Karda veya -%5 altinda kapat, hafif zararda bekle |

### 6.3 Kademeli Trailing Stop

| Peak PnL | Geri Cekilme Esigi | Ornek |
|----------|-------------------|-------|
| %0 - %3 | Trailing yok | Nefes alsin |
| %3 - %8 | Peak'ten %50 | Peak %6 → %3'te kapat |
| %8 - %15 | Peak'ten %35 | Peak %12 → %7.8'de kapat |
| %15+ | Peak'ten %25 | Peak %20 → %15'te kapat |

### 6.4 Zarar Toleransi

| Zarar Bandi | Davranis | Sebep |
|-------------|----------|-------|
| %0 ile -%5 | Tolere et, kapatma | %63 toparlanma olasiligi |
| -%5 ile -%15 | Sinyal/momentum kaybi varsa kapat | Orta risk |
| -%15 alti | Hard stop-loss, hemen kapat | Yuksek risk |
| -%80 alti | Likidasyon | Teminat erimis |

### 6.5 Fee Hesaplama

```
Taker fee: %0.04 (pozisyon buyuklugu uzerinden)

Giris fee:  $20,000 x 0.0004 = $8.00
Cikis fee:  ~$20,000 x 0.0004 = ~$8.00
Toplam:     ~$16.00 per islem

Net PnL = Brut PnL - Giris Fee - Cikis Fee
Max Zarar = Teminat ($2,000) — likidasyon siniri
```

### 6.6 Hard Stop Cooldown

Ayni sembolde hard stop sonrasi bekleme suresi:

| Hard Stop # | Cooldown | Aciklama |
|-------------|----------|----------|
| 1. kez | 10 dakika | Kisa sogunma |
| 2+ kez | 30 dakika | Ciddi sorun, uzun dur |

---

## 7. Web Dashboard

### 7.1 Erisim

```
Paper mod:    http://192.168.0.89:8888
Backtest mod: http://localhost:9999 (farkli port)
```

### 7.2 API Endpoints

| Endpoint | Aciklama |
|----------|----------|
| `GET /` | Ana dashboard (HTML) |
| `GET /api/status` | Genel durum: bakiye, PnL, pozisyon sayisi, win rate |
| `GET /api/signals` | Son 200 sinyal listesi |
| `GET /api/positions` | Acik pozisyonlar: anlik fiyat, net PnL, PnL%, sure |
| `GET /api/trades` | Son 100 tamamlanan islem |

### 7.3 Dashboard Ozellikleri

- 2 saniyede bir otomatik yenileme
- Realized + Unrealized PnL ayri gosterim
- Acik pozisyonlarda yesil/kirmizi gostergeler (kar/zarar yonu)
- Fee dahil net PnL hesaplama
- Pozisyon suresi gosterimi

---

## 8. Calistirma

### 8.1 Gereksinimler

- Go 1.22+
- TimescaleDB (PostgreSQL + TimescaleDB extension)
- Collector'in aktif calismasi (depth_snapshots, agg_trades verileri)

### 8.2 Derleme

```bash
cd deep-trader

# Lokal calistirma icin
go build -o bin/deep-trader ./cmd/trader

# Linux sunucu icin cross-compile
GOOS=linux GOARCH=amd64 go build -o bin/deep-trader ./cmd/trader
```

### 8.3 Paper Trading (Canli Simulasyon)

```bash
# Dogrudan calistirma
./bin/deep-trader --mode=paper --config=config/config.yaml

# Systemd servisi olarak (onerilen)
# /etc/systemd/system/deep-trader.service
[Unit]
Description=Deep Trader — Paper Trading Engine
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/deep-trader
ExecStart=/opt/deep-trader/bin/deep-trader --mode=paper --config=/opt/deep-trader/config/config.yaml
Restart=always
RestartSec=10
StandardOutput=append:/opt/deep-trader/logs/server.log
StandardError=append:/opt/deep-trader/logs/server.err

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable deep-trader
systemctl start deep-trader
```

### 8.4 Backtest (Gecmis Veri Testi)

```bash
# 5 gunluk backtest
./bin/deep-trader --mode=backtest \
  --start=2026-04-10 --end=2026-04-15 \
  --config=config/backtest.yaml

# Sonuclar DB'de: paper_trades WHERE run_mode='backtest'
```

### 8.5 Live Trading (Henuz Implement Edilmedi)

```bash
# Gelecekte:
./bin/deep-trader --mode=live --config=config/config.yaml
# Binance WebSocket'e dogrudan baglanir
# Binance API ile gercek emir acar
```

---

## 9. Deploy

### 9.1 Mevcut Ortam

```
Paper Trading:  CT 192.168.0.89 (Proxmox LXC)
TimescaleDB:    CT 192.168.0.238
Collector:      Ayri container (dokunulmaz)
```

### 9.2 Yeni Ortama Deploy

```bash
# 1. Binary'yi sunucuya kopyala
scp bin/deep-trader root@SUNUCU_IP:/opt/deep-trader/bin/

# 2. Config dosyasini kopyala ve duzenle
scp config/config.yaml root@SUNUCU_IP:/opt/deep-trader/config/
# config.yaml'daki database bilgilerini guncelle

# 3. Dizinleri olustur
ssh root@SUNUCU_IP "mkdir -p /opt/deep-trader/{bin,config,logs}"

# 4. Systemd servisi olustur (yukaridaki ornek)

# 5. Baslat
ssh root@SUNUCU_IP "systemctl start deep-trader"
```

### 9.3 Guncelleme (Deploy)

```bash
# 1. Derle
GOOS=linux GOARCH=amd64 go build -o bin/deep-trader ./cmd/trader

# 2. Durdur + Kopyala + Baslat
ssh root@192.168.0.89 "systemctl stop deep-trader"
scp bin/deep-trader root@192.168.0.89:/opt/deep-trader/bin/
scp config/config.yaml root@192.168.0.89:/opt/deep-trader/config/
ssh root@192.168.0.89 "systemctl start deep-trader"
```

---

## 10. Konfigurasyon Referansi

```yaml
mode: paper                       # backtest | paper | live

database:
  host: 192.168.0.238
  port: 5432
  name: deeptrader_market
  user: deeptrader
  password: deeptrader2024
  max_conns: 20

symbols:
  min_market_cap_usd: 20000000    # $20M minimum
  exclude_stables: true
  exclude_leverage: true
  max_symbols: 100

websocket:                        # sadece live modda kullanilir
  binance_url: wss://stream.binance.com:9443
  depth_level: 20
  depth_interval: 100ms
  reconnect_max_backoff: 30s

analyzer:
  large_order_threshold_usd: 100000  # $100K buyuk emir esigi
  spoof_max_lifetime: 30s            # 30sn'den kisa = spoof
  trade_flow_windows:                # sliding imbalance pencereleri
    - 30s
    - 1m
    - 5m
    - 15m
  emit_interval: 5s                  # cikti araligi
  consolidation_days: 7              # konsolidasyon penceresi
  consolidation_threshold: 0.05      # %5'ten az = konsolidasyon

signal:
  ml_weight: 0.0                     # 0 = sadece kural motoru
  ml_model_path: ml/models/latest.onnx
  stale_timeout: 1h                  # 1 saat sonra ±%5 kapat
  loss_cooldown: 0s                  # genel zarar cooldown (kapali)
  confirm_delay: 0s                  # ayni yonlu sinyal bu sure boyunca surerse emit edilir
  hard_stop_cooldown_1: 10m          # 1. hard stop sonrasi 10dk bekle
  hard_stop_cooldown_2: 30m          # 2+ hard stop sonrasi 30dk bekle
  rules:
    pump_imbalance_min: 0.70
    dump_imbalance_max: 0.30
    bid_ask_ratio_pump: 2.0
    bid_ask_ratio_dump: 0.50
    volume_ratio_min: 5.0
    price_change_max: 0.15
    spoof_penalty: 0.15
    consolidation_days: 7
    consolidation_threshold: 0.05

backtest:
  start_time: "2024-01-01T00:00:00Z"
  end_time: "2025-01-01T00:00:00Z"
  speed: 100.0                       # 100 = 100x realtime, <=0 = sinirsiz hiz

web:
  port: 8888

executor:
  initial_balance_usd: 100000
  leverage: 10
  max_position_pct: 0.02             # %2 = $2,000 sabit teminat
  max_positions: 50
  stop_loss_pct: 0.01
  daily_loss_limit_pct: 0.05
  taker_fee_pct: 0.0004              # %0.04
  maker_fee_pct: 0.0002              # %0.02 (kullanilmiyor)
  trend_follow_leverage: 0           # 0 = normal leverage
  trend_follow_position_pct: 0       # 0 = normal pct
```

---

## 11. Backtest Sonuclari

### v4.3 — 5 Gunluk Backtest (10-15 Nisan 2026)

| Ayar | Trade | PnL | TREND_FOLLOW | PUMP | DUMP |
|------|-------|-----|-------------|------|------|
| Hard stop cooldown (10m/30m) | 112 | +$57,166 | +$58,164 | -$1,576 | +$577 |
| Baseline (cooldown yok) | 45 | +$62 | -$486 | +$382 | +$166 |

### Test Edilen ve Reddedilen Ozellikler

| Ozellik | Sonuc | Karar |
|---------|-------|-------|
| Loss cooldown (30m/1h/3h) | Karli firsatlar kacirildi | REDDEDILDI |
| Confirm delay (5s/10s/20s) | Gec giris, kar kaybi | REDDEDILDI |
| Imbalance filtresi | PUMP iyilesti ama toplam dusus | REDDEDILDI |
| Stale timeout 1h | +%50 iyilesme | KABUL EDILDI |
| Hard stop cooldown | +$57K iyilesme | KABUL EDILDI |
| Zarar toleransi -%5 | +$190K iyilesme | KABUL EDILDI |

---

## 12. Bilinen Kisitlamalar

1. **Servis restart'ta acik pozisyonlar kayboluyor** — tracker bellekte, DB'den yuklenmiyor
2. **Live mod henuz implement edilmedi** — WebSocket router var ama Binance API emir acma yok
3. **Ayni sembolde tek pozisyon** — ayni anda long ve short acilamaz
4. **ML katmani pasif** — ml_weight=0, ONNX model entegrasyonu hazir ama egitilmis model yok
5. **Backtest yavas** — 5 gunluk veri ~30dk suruyor (emit_interval=30s ile)
6. **Depth verisi sadece $20M+ semboller** — kucuk coinlerde order book analizi yok

---

## 13. Collector Bagimliligi

Deep Trader bagimsiz calisamaz — asagidaki verileri Collector'in toplamasini bekler:

| Veri | Gereklilik | Kaynak |
|------|------------|--------|
| `depth_snapshots` | ZORUNLU | Collector WS depth20@100ms |
| `agg_trades` | ZORUNLU | Collector WS aggTrade |
| `klines_1m` | ZORUNLU | Collector WS kline_1m |
| `funding_rates` | ONEMLI | Collector REST 8h polling |
| `klines_1d` | ONEMLI | TimescaleDB continuous aggregate |

Collector olmadan Deep Trader sinyal uretmez. Collector ayri bir sistemdir ve dokunulmamalidir.

---

## 14. Versiyon Gecmisi

| Tag | Tarih | Degisiklik |
|-----|-------|------------|
| v1 | 2026-04-13 | Temel paper trading engine |
| v2 | 2026-04-13 | Kademeli trailing stop + net PnL |
| v2.1 | 2026-04-13 | Trailing stop gercek zamanli fiyat |
| v2.2 | 2026-04-13 | Trailing stop cikis fiyati fix |
| v3 | 2026-04-14 | Zarar toleransi (-%5 bekle) |
| v3.1 | 2026-04-14 | Gercekci teminat yonetimi |
| v3.2 | 2026-04-14 | DB run izolasyonu + backtest fix |
| v4 | 2026-04-14 | Stale timeout + confirm delay + backtest optimizasyonu |
| v4.1 | 2026-04-14 | TREND_FOLLOW 0.60 esik |
| v4.2 | 2026-04-15 | Stale timeout fix + likidasyon |
| v4.3 | 2026-04-15 | Hard stop cooldown (10m/30m) |
| v4.4 | 2026-04-15 | Sabit teminat fix (compound bug) |
| v4.5 | 2026-04-15 | Event-time backtest + sliding trade flow + spoof wiring |

---

*Deep Trader — Order Book Intelligence Engine*
*v4.4 — Nisan 2026*
