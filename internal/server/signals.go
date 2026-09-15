package server

import (
	"strconv"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

// buildSignalRows lays out the evidence behind one score's noise number:
// the five weighted signals, in the same order score.NoiseScore sums them
// (so Contribution values on the page add up to the displayed noise score
// exactly the way the formula does), followed by the verdict/confidence-
// only signals that route a verdict without adding to it.
//
// Weight and Contribution come from the server's OWN current config, not
// from whatever weights were active at scan time (a score row stores only
// its final noise_score, not the weights that produced it -- see
// store.Score). For an operator who edits noisefloor.yaml between scans
// this can, in principle, drift from the number `scan` actually printed;
// in practice both come from the same config file, and re-scanning is how
// a changed weight ever takes effect anyway.
func buildSignalRows(signals map[string]float64, w config.Weights) []signalRow {
	rows := []signalRow{
		{Name: "short_lived_rate", Value: pctOf(signals, "short_lived_rate"), Weight: w.ShortLivedRate, Weighted: true},
		{Name: "silenced_rate", Value: pctOf(signals, "silenced_rate"), Weight: w.SilencedRate, Weighted: true},
		{Name: "flap_rate", Value: pctOf(signals, "flap_rate"), Weight: w.FlapRate, Weighted: true},
		{Name: "cofire_ratio", Value: pctOf(signals, "cofire_ratio"), Weight: w.CofireRatio, Weighted: true},
		{Name: "offhours_rate", Value: pctOf(signals, "offhours_rate"), Weight: w.OffhoursRate, Weighted: true},
	}
	for i := range rows {
		// pctOf, above, already rendered "-" for this row's Value when the
		// signal is missing OR NaN (a rule scored before the signal
		// existed, or a map that failed to deserialise). Either way there
		// is no measurement to multiply by the weight: signals[name] reads
		// 0 for a missing key, silently printing a contribution of "0.0"
		// that looks like a measured zero rather than "not measured", and
		// NaN for a present-but-NaN one, printing "NaN". Falling back to
		// Weighted=false makes the template print "-" for both weight and
		// contribution instead, exactly as it already does for a signal
		// this table doesn't weight at all -- Value and Contribution never
		// disagree about whether this row has a number.
		if rows[i].Value == "-" {
			rows[i].Weighted = false
			continue
		}
		rows[i].Contribution = 100 * rows[i].Weight * signals[rows[i].Name]
	}

	rows = append(rows,
		signalRow{Name: "concentration", Value: pctOf(signals, "concentration")},
		signalRow{Name: "pending_churn", Value: pctOf(signals, "pending_churn")},
		signalRow{Name: "fires", Value: itoaFloat(signals["fires"])},
		signalRow{Name: "unique_fingerprints", Value: itoaFloat(signals["unique_fingerprints"])},
		signalRow{Name: "p50_duration", Value: formatDuration(durationOf(signals, "p50_duration_s"))},
		signalRow{Name: "p90_duration", Value: formatDuration(durationOf(signals, "p90_duration_s"))},
	)
	return rows
}

func itoaFloat(f float64) string {
	return strconv.Itoa(int(f))
}
