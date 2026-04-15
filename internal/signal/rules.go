package signal

import (
	"math"

	"github.com/deep-trader/internal/config"
	"github.com/deep-trader/internal/models"
)

type RuleEngine struct {
	cfg config.RulesConfig
}

func NewRuleEngine(cfg config.RulesConfig) *RuleEngine {
	return &RuleEngine{cfg: cfg}
}

// CalculateScores — pump/dump ham skorlarini hesaplar.
// Hem Evaluate hem de tracker tarafindan kullanilir (tek kaynak).
func (re *RuleEngine) CalculateScores(out models.AnalyzerOutput) (pumpScore, dumpScore float64) {
	ob := out.OrderBookMetrics
	tf := out.TradeFlow

	// PUMP
	if tf.Imbalance >= re.cfg.PumpImbalanceMin {
		pumpScore += 0.35
	}
	if ob.BidAskRatio >= re.cfg.BidAskRatioPump {
		pumpScore += 0.25
	}
	if out.VolumeRatio >= re.cfg.VolumeRatioMin && out.PriceChange <= re.cfg.PriceChangeMax {
		pumpScore += 0.30
	}
	if ob.BidDelta > 0 && ob.AskDelta < 0 {
		pumpScore += 0.10
	}
	if len(ob.SpoofSuspects) > 0 {
		pumpScore -= re.cfg.SpoofPenalty
	}

	// DUMP
	if tf.Imbalance <= re.cfg.DumpImbalanceMax {
		dumpScore += 0.35
	}
	if ob.BidAskRatio <= re.cfg.BidAskRatioDump {
		dumpScore += 0.25
	}
	if ob.BidDelta < 0 && ob.AskDelta > 0 {
		dumpScore += 0.25
	}
	if len(ob.SpoofSuspects) > 0 {
		dumpScore -= re.cfg.SpoofPenalty
	}

	// Multi-window verisi TradeFlowWindows'da mevcut ama skora eklenmez.
	// Confluence bonus (+0.10) giris esigini fiilen 0.55'e dusuruyordu
	// ve cok fazla zayif sinyal uretiyordu. Kaldirildi.

	return
}

// Evaluate — sinyal tipi, skor, sebepler ve yon dondurur.
func (re *RuleEngine) Evaluate(out models.AnalyzerOutput) (models.SignalType, float64, []string, string) {
	ob := out.OrderBookMetrics
	tf := out.TradeFlow

	pumpScore, dumpScore := re.CalculateScores(out)

	// Pump reasons
	var reasons []string
	if tf.Imbalance >= re.cfg.PumpImbalanceMin {
		reasons = append(reasons, "trade flow guclu alis baskisi")
	}
	if ob.BidAskRatio >= re.cfg.BidAskRatioPump {
		reasons = append(reasons, "order book alis tarafi baskin")
	}
	if out.VolumeRatio >= re.cfg.VolumeRatioMin && out.PriceChange <= re.cfg.PriceChangeMax {
		reasons = append(reasons, "hacim patladi, fiyat henuz az hareket etti")
	}
	if ob.BidDelta > 0 && ob.AskDelta < 0 {
		reasons = append(reasons, "alis birikimi devam ediyor")
	}
	if len(ob.SpoofSuspects) > 0 {
		reasons = append(reasons, "spoof tespit edildi, guven dusuruldu")
	}

	// Dump reasons
	var dumpReasons []string
	if tf.Imbalance <= re.cfg.DumpImbalanceMax {
		dumpReasons = append(dumpReasons, "trade flow guclu satis baskisi")
	}
	if ob.BidAskRatio <= re.cfg.BidAskRatioDump {
		dumpReasons = append(dumpReasons, "order book satis tarafi baskin")
	}
	if ob.BidDelta < 0 && ob.AskDelta > 0 {
		dumpReasons = append(dumpReasons, "destek eriyor")
	}

	// TREND KURALLARI
	trendScore := 0.0
	var trendReasons []string

	if !out.IsConsolidating {
		trendScore += 0.25
		trendReasons = append(trendReasons, "konsolidasyon yok, trend aktif")
	}
	if math.Abs(out.FundingRate) > 0.001 {
		trendScore += 0.15
		trendReasons = append(trendReasons, "funding rate yuksek, trend guclu")
	}
	if math.Abs(out.PriceChange) > 0.02 {
		trendScore += 0.20
		trendReasons = append(trendReasons, "fiyat hareketi guclu (>%2)")
	}
	if out.VolumeRatio > 2.0 {
		trendScore += 0.15
		trendReasons = append(trendReasons, "hacim ortalamanin 2x uzerinde")
	}

	// KARAR
	switch {
	case pumpScore >= 0.65:
		return models.SignalPump, pumpScore, reasons, "long"
	case dumpScore >= 0.65:
		return models.SignalDump, dumpScore, dumpReasons, "short"
	case trendScore >= 0.60:
		// TREND_FOLLOW her zaman long (paper ile ayni davranis)
		return models.SignalTrendFollow, trendScore, trendReasons, "long"
	default:
		return models.SignalNoEntry, 0, []string{"yeterli sinyal yok"}, ""
	}
}
