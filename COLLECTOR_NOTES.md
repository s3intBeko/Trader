# Collector Bilgileri

## Stream Dagitimi

| Stream | Semboller | Frekans |
|--------|-----------|---------|
| aggTrade | TUM (~291) | Gercek zamanli |
| kline_1m | TUM (~291) | Her dakika |
| forceOrder | TUM (~291) | Likidasyon oldugunda |
| markPrice@1s | TUM (~291) | Her saniye |
| bookTicker | TUM (~291) | Gercek zamanli |
| depth20@100ms | Sadece $20M+ | 10/sn |
| depth@500ms | Sadece $20M+ | 2/sn |

## Onemli Notlar

- **depth_snapshots/depth_diffs:** Sadece $20M+ 24h hacimli semboller (depth_min_volume: 20000000). Tum semboller icin toplamak veri miktarini cok artirir.
- **liquidations:** Normal calisiyor, @forceOrder sadece likidasyon gerceklestiginde event gonderiyor — cogu sembolde nadir. 7.8K satir normal.
- **depth_diffs:** Aktif toplanıyor, $20M+ semboller icin.
- **Yeni sembol eklenmesi:** Otomatik DEGIL. Collector baslangicta dynamic modda Binance'tan listeyi cekiyor ama restart olmadan yeni sembol eklenmiyor. Restart edince otomatik bulunur.

## Deep Trader Icin Etkileri

- Signal Engine'in depth gerektiren analizi (OrderBook Delta, Spoof Detection) sadece $20M+ semboller icin calisir — bu sorun degil, zaten kucuk coinlerde depth verisi anlamli degil.
- config.yaml'daki `min_market_cap_usd: 20000000` ($20M) esigi collector'in depth esigi ile uyumlu.
- Collector restart edildiginde yeni semboller otomatik eklenir, ama araya veri kaybi olur.
