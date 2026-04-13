package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/deep-trader/internal/models"
)

const maxSignalHistory = 200
const maxTradeHistory = 100

type Dashboard struct {
	port    int
	server  *http.Server
	logger  *zap.Logger

	mu            sync.RWMutex
	signals       []models.SignalEvent
	positions     map[string]*models.Position
	trades        []models.PaperTrade
	balance       float64
	initialBal    float64
	startTime     time.Time
	mode          string
	symbols       []string
	eventCount    int64
	lastEventTime time.Time
}

func NewDashboard(port int, mode string, symbols []string, initialBalance float64, logger *zap.Logger) *Dashboard {
	return &Dashboard{
		port:       port,
		logger:     logger,
		signals:    make([]models.SignalEvent, 0),
		positions:  make(map[string]*models.Position),
		trades:     make([]models.PaperTrade, 0),
		balance:    initialBalance,
		initialBal: initialBalance,
		startTime:  time.Now(),
		mode:       mode,
		symbols:    symbols,
	}
}

func (d *Dashboard) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/signals", d.handleSignals)
	mux.HandleFunc("/api/positions", d.handlePositions)
	mux.HandleFunc("/api/trades", d.handleTrades)

	d.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", d.port),
		Handler: mux,
	}

	d.logger.Info("web dashboard baslatiliyor",
		zap.Int("port", d.port),
		zap.String("url", fmt.Sprintf("http://0.0.0.0:%d", d.port)),
	)

	go func() {
		if err := d.server.ListenAndServe(); err != http.ErrServerClosed {
			d.logger.Error("web server hatasi", zap.Error(err))
		}
	}()

	return nil
}

func (d *Dashboard) Stop() {
	if d.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.server.Shutdown(ctx)
	}
}

// AddSignal — yeni sinyal ekler
func (d *Dashboard) AddSignal(s models.SignalEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.signals = append(d.signals, s)
	if len(d.signals) > maxSignalHistory {
		d.signals = d.signals[len(d.signals)-maxSignalHistory:]
	}
}

// UpdatePosition — pozisyon gunceller
func (d *Dashboard) UpdatePosition(symbol string, pos *models.Position) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if pos == nil {
		delete(d.positions, symbol)
	} else {
		d.positions[symbol] = pos
	}
}

// AddTrade — tamamlanan islemi ekler
func (d *Dashboard) AddTrade(t models.PaperTrade) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trades = append(d.trades, t)
	if len(d.trades) > maxTradeHistory {
		d.trades = d.trades[len(d.trades)-maxTradeHistory:]
	}
}

// UpdateBalance — bakiye gunceller
func (d *Dashboard) UpdateBalance(balance float64) {
	d.mu.Lock()
	d.balance = balance
	d.mu.Unlock()
}

// IncrementEvents — event sayacini arttirir
func (d *Dashboard) IncrementEvents() {
	d.mu.Lock()
	d.eventCount++
	d.lastEventTime = time.Now()
	d.mu.Unlock()
}

// API handlers
func (d *Dashboard) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	totalPnL := d.balance - d.initialBal
	pnlPct := 0.0
	if d.initialBal > 0 {
		pnlPct = totalPnL / d.initialBal * 100
	}

	winCount := 0
	lossCount := 0
	for _, t := range d.trades {
		if t.PnL > 0 {
			winCount++
		} else {
			lossCount++
		}
	}

	winRate := 0.0
	if winCount+lossCount > 0 {
		winRate = float64(winCount) / float64(winCount+lossCount) * 100
	}

	status := map[string]interface{}{
		"mode":            d.mode,
		"uptime":          time.Since(d.startTime).String(),
		"start_time":      d.startTime.Format(time.RFC3339),
		"symbols":         d.symbols,
		"symbol_count":    len(d.symbols),
		"balance":         d.balance,
		"initial_balance": d.initialBal,
		"total_pnl":       totalPnL,
		"pnl_pct":         pnlPct,
		"open_positions":  len(d.positions),
		"total_trades":    len(d.trades),
		"total_signals":   len(d.signals),
		"win_count":       winCount,
		"loss_count":      lossCount,
		"win_rate":        winRate,
		"event_count":     d.eventCount,
		"last_event":      d.lastEventTime.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (d *Dashboard) handleSignals(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Son N sinyal (ters sira)
	signals := make([]models.SignalEvent, len(d.signals))
	copy(signals, d.signals)
	for i, j := 0, len(signals)-1; i < j; i, j = i+1, j-1 {
		signals[i], signals[j] = signals[j], signals[i]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(signals)
}

func (d *Dashboard) handlePositions(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d.positions)
}

func (d *Dashboard) handleTrades(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	trades := make([]models.PaperTrade, len(d.trades))
	copy(trades, d.trades)
	for i, j := 0, len(trades)-1; i < j; i, j = i+1, j-1 {
		trades[i], trades[j] = trades[j], trades[i]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(trades)
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

const indexHTML = `<!DOCTYPE html>
<html lang="tr">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Deep Trader — Dashboard</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { background: #0a0e17; color: #e0e6ed; font-family: 'JetBrains Mono', 'Fira Code', monospace; font-size: 14px; }
.header { background: #111827; padding: 16px 24px; border-bottom: 1px solid #1e293b; display: flex; justify-content: space-between; align-items: center; }
.header h1 { color: #22d3ee; font-size: 20px; font-weight: 700; }
.header .mode { background: #065f46; color: #34d399; padding: 4px 12px; border-radius: 4px; font-size: 12px; font-weight: 600; }
.header .mode.live { background: #7f1d1d; color: #f87171; }
.grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; padding: 20px 24px; }
.card { background: #111827; border: 1px solid #1e293b; border-radius: 8px; padding: 16px; }
.card .label { color: #64748b; font-size: 11px; text-transform: uppercase; letter-spacing: 1px; }
.card .value { font-size: 28px; font-weight: 700; margin-top: 4px; }
.card .value.green { color: #22c55e; }
.card .value.red { color: #ef4444; }
.card .value.cyan { color: #22d3ee; }
.card .sub { color: #64748b; font-size: 12px; margin-top: 4px; }
.section { padding: 0 24px 20px; }
.section h2 { color: #94a3b8; font-size: 14px; margin-bottom: 12px; text-transform: uppercase; letter-spacing: 1px; }
table { width: 100%; border-collapse: collapse; background: #111827; border: 1px solid #1e293b; border-radius: 8px; overflow: hidden; }
th { background: #0f172a; color: #64748b; font-size: 11px; text-transform: uppercase; letter-spacing: 1px; padding: 10px 12px; text-align: left; }
td { padding: 8px 12px; border-top: 1px solid #1e293b; font-size: 13px; }
tr:hover { background: #1e293b; }
.signal-pump { color: #22c55e; font-weight: 700; }
.signal-dump { color: #ef4444; font-weight: 700; }
.signal-trend { color: #f59e0b; font-weight: 700; }
.signal-noentry { color: #64748b; }
.pnl-pos { color: #22c55e; }
.pnl-neg { color: #ef4444; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; }
.dot.green { background: #22c55e; }
.dot.red { background: #ef4444; }
.dot.yellow { background: #f59e0b; }
.status-bar { background: #111827; padding: 8px 24px; border-top: 1px solid #1e293b; display: flex; gap: 24px; font-size: 12px; color: #64748b; position: fixed; bottom: 0; width: 100%; }
</style>
</head>
<body>
<div class="header">
  <h1>DEEP TRADER</h1>
  <div id="mode" class="mode">PAPER</div>
</div>

<div class="grid">
  <div class="card">
    <div class="label">Bakiye</div>
    <div class="value cyan" id="balance">$0.00</div>
    <div class="sub" id="initial-bal">Baslangic: $0</div>
  </div>
  <div class="card">
    <div class="label">Toplam PnL</div>
    <div class="value" id="pnl">$0.00</div>
    <div class="sub" id="pnl-pct">0.00%</div>
  </div>
  <div class="card">
    <div class="label">Acik Pozisyonlar</div>
    <div class="value cyan" id="open-pos">0</div>
    <div class="sub" id="total-trades">0 islem tamamlandi</div>
  </div>
  <div class="card">
    <div class="label">Kazanma Orani</div>
    <div class="value" id="win-rate">-</div>
    <div class="sub" id="win-loss">0W / 0L</div>
  </div>
</div>

<div class="section">
  <h2>Acik Pozisyonlar</h2>
  <table>
    <thead><tr><th>Sembol</th><th>Taraf</th><th>Giris Fiyat</th><th>Giris Zamani</th><th>Sinyal</th></tr></thead>
    <tbody id="positions-body"><tr><td colspan="5" style="color:#64748b;text-align:center">Acik pozisyon yok</td></tr></tbody>
  </table>
</div>

<div class="section">
  <h2>Son Sinyaller</h2>
  <table>
    <thead><tr><th>Zaman</th><th>Sembol</th><th>Sinyal</th><th>Guven</th><th>Kaynak</th><th>Sebepler</th></tr></thead>
    <tbody id="signals-body"><tr><td colspan="6" style="color:#64748b;text-align:center">Sinyal bekleniyor...</td></tr></tbody>
  </table>
</div>

<div class="section">
  <h2>Son Islemler</h2>
  <table>
    <thead><tr><th>Zaman</th><th>Sembol</th><th>Taraf</th><th>Giris</th><th>Cikis</th><th>PnL</th></tr></thead>
    <tbody id="trades-body"><tr><td colspan="6" style="color:#64748b;text-align:center">Henuz islem yok</td></tr></tbody>
  </table>
</div>

<div class="status-bar">
  <span><span class="dot green" id="status-dot"></span> <span id="uptime">0s</span></span>
  <span>Events: <span id="event-count">0</span></span>
  <span>Son event: <span id="last-event">-</span></span>
  <span>Semboller: <span id="symbol-count">0</span></span>
</div>

<script>
function signalClass(s) {
  switch(s) {
    case 'PUMP': return 'signal-pump';
    case 'DUMP': return 'signal-dump';
    case 'TREND_FOLLOW': return 'signal-trend';
    default: return 'signal-noentry';
  }
}
function formatTime(t) {
  if (!t || t === '0001-01-01T00:00:00Z') return '-';
  return new Date(t).toLocaleTimeString('tr-TR', {hour:'2-digit',minute:'2-digit',second:'2-digit'});
}
function formatPnL(v) {
  const cls = v >= 0 ? 'pnl-pos' : 'pnl-neg';
  return '<span class="'+cls+'">$'+v.toFixed(2)+'</span>';
}

async function refresh() {
  try {
    const [status, signals, positions, trades] = await Promise.all([
      fetch('/api/status').then(r=>r.json()),
      fetch('/api/signals').then(r=>r.json()),
      fetch('/api/positions').then(r=>r.json()),
      fetch('/api/trades').then(r=>r.json()),
    ]);

    // Status cards
    document.getElementById('mode').textContent = status.mode.toUpperCase();
    document.getElementById('mode').className = 'mode' + (status.mode === 'live' ? ' live' : '');
    document.getElementById('balance').textContent = '$' + status.balance.toFixed(2);
    document.getElementById('initial-bal').textContent = 'Baslangic: $' + status.initial_balance.toFixed(0);

    const pnlEl = document.getElementById('pnl');
    pnlEl.textContent = (status.total_pnl >= 0 ? '+$' : '-$') + Math.abs(status.total_pnl).toFixed(2);
    pnlEl.className = 'value ' + (status.total_pnl >= 0 ? 'green' : 'red');
    document.getElementById('pnl-pct').textContent = (status.pnl_pct >= 0 ? '+' : '') + status.pnl_pct.toFixed(2) + '%';

    document.getElementById('open-pos').textContent = status.open_positions;
    document.getElementById('total-trades').textContent = status.total_trades + ' islem tamamlandi';

    const wr = document.getElementById('win-rate');
    wr.textContent = status.total_trades > 0 ? status.win_rate.toFixed(1) + '%' : '-';
    wr.className = 'value ' + (status.win_rate >= 50 ? 'green' : status.total_trades > 0 ? 'red' : 'cyan');
    document.getElementById('win-loss').textContent = status.win_count + 'W / ' + status.loss_count + 'L';

    document.getElementById('uptime').textContent = status.uptime;
    document.getElementById('event-count').textContent = status.event_count.toLocaleString();
    document.getElementById('last-event').textContent = formatTime(status.last_event);
    document.getElementById('symbol-count').textContent = status.symbol_count;

    // Positions
    const posKeys = Object.keys(positions || {});
    const posBody = document.getElementById('positions-body');
    if (posKeys.length === 0) {
      posBody.innerHTML = '<tr><td colspan="5" style="color:#64748b;text-align:center">Acik pozisyon yok</td></tr>';
    } else {
      posBody.innerHTML = posKeys.map(k => {
        const p = positions[k];
        return '<tr><td>'+p.symbol+'</td><td>'+(p.side==='long'?'<span class="pnl-pos">LONG</span>':'<span class="pnl-neg">SHORT</span>')+'</td><td>$'+p.entry_price.toFixed(2)+'</td><td>'+formatTime(p.entry_time)+'</td><td><span class="'+signalClass(p.signal)+'">'+p.signal+'</span></td></tr>';
      }).join('');
    }

    // Signals
    const sigBody = document.getElementById('signals-body');
    if (!signals || signals.length === 0) {
      sigBody.innerHTML = '<tr><td colspan="6" style="color:#64748b;text-align:center">Sinyal bekleniyor...</td></tr>';
    } else {
      sigBody.innerHTML = signals.slice(0, 50).map(s =>
        '<tr><td>'+formatTime(s.timestamp)+'</td><td>'+s.symbol+'</td><td><span class="'+signalClass(s.signal)+'">'+s.signal+'</span></td><td>'+(s.confidence*100).toFixed(0)+'%</td><td>'+s.source+'</td><td style="max-width:300px;overflow:hidden;text-overflow:ellipsis">'+(s.reasons||[]).join(', ')+'</td></tr>'
      ).join('');
    }

    // Trades
    const trBody = document.getElementById('trades-body');
    if (!trades || trades.length === 0) {
      trBody.innerHTML = '<tr><td colspan="6" style="color:#64748b;text-align:center">Henuz islem yok</td></tr>';
    } else {
      trBody.innerHTML = trades.slice(0, 50).map(t =>
        '<tr><td>'+formatTime(t.exit_time)+'</td><td>'+t.symbol+'</td><td>'+(t.side==='long'?'<span class="pnl-pos">LONG</span>':'<span class="pnl-neg">SHORT</span>')+'</td><td>$'+t.entry_price.toFixed(2)+'</td><td>$'+t.exit_price.toFixed(2)+'</td><td>'+formatPnL(t.pnl)+'</td></tr>'
      ).join('');
    }

    document.getElementById('status-dot').className = 'dot green';
  } catch(e) {
    document.getElementById('status-dot').className = 'dot red';
  }
}

setInterval(refresh, 2000);
refresh();
</script>
</body>
</html>`
