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

func (re *RuleEngine) Evaluate(out models.AnalyzerOutput) (models.SignalType, float64, []string) {
	ob := out.OrderBookMetrics
	tf := out.TradeFlow

	var reasons []string

	// PUMP KURALLARI
	pumpScore := 0.0

	if tf.Imbalance >= re.cfg.PumpImbalanceMin {
		pumpScore += 0.35
		reasons = append(reasons, "trade flow guclu alis baskisi")
	}
	if ob.BidAskRatio >= re.cfg.BidAskRatioPump {
		pumpScore += 0.25
		reasons = append(reasons, "order book alis tarafi baskin")
	}
	if out.VolumeRatio >= re.cfg.VolumeRatioMin && out.PriceChange <= re.cfg.PriceChangeMax {
		pumpScore += 0.30
		reasons = append(reasons, "hacim patladi, fiyat henuz az hareket etti")
	}
	if ob.BidDelta > 0 && ob.AskDelta < 0 {
		pumpScore += 0.10
		reasons = append(reasons, "alis birikimi devam ediyor")
	}

	// Spoof tespit edilmisse guven dusur
	if len(ob.SpoofSuspects) > 0 {
		pumpScore -= re.cfg.SpoofPenalty
		reasons = append(reasons, "spoof tespit edildi, guven dusuruldu")
	}

	// DUMP KURALLARI
	dumpScore := 0.0
	var dumpReasons []string

	if tf.Imbalance <= re.cfg.DumpImbalanceMax {
		dumpScore += 0.35
		dumpReasons = append(dumpReasons, "trade flow guclu satis baskisi")
	}
	if ob.BidAskRatio <= re.cfg.BidAskRatioDump {
		dumpScore += 0.25
		dumpReasons = append(dumpReasons, "order book satis tarafi baskin")
	}
	if ob.BidDelta < 0 && ob.AskDelta > 0 {
		dumpScore += 0.25
		dumpReasons = append(dumpReasons, "destek eriyor")
	}

	if len(ob.SpoofSuspects) > 0 {
		dumpScore -= re.cfg.SpoofPenalty
	}

	// TREND KURALLARI
	trendScore := 0.0
	var trendReasons []string

	if !out.IsConsolidating {
		trendScore += 0.40
		trendReasons = append(trendReasons, "konsolidasyon yok, trend aktif")
	}
	if math.Abs(out.FundingRate) > 0.001 {
		trendScore += 0.20
		trendReasons = append(trendReasons, "funding rate yuksek, trend guclu")
	}

	// KARAR
	switch {
	case pumpScore >= 0.65:
		return models.SignalPump, pumpScore, reasons
	case dumpScore >= 0.65:
		return models.SignalDump, dumpScore, dumpReasons
	case trendScore >= 0.60:
		return models.SignalTrendFollow, trendScore, trendReasons
	default:
		return models.SignalNoEntry, 0, []string{"yeterli sinyal yok"}
	}
}
