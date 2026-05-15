// Package summary — cost.go estimates the ¥ cost of one Chat call from
// prompt/completion token counts and the prices configured under
// config.LLMConfig. Returns the same shape (¥ + display text) the API
// and UI consume; no rounding magic — it's purely cosmetic.
package summary

import (
	"fmt"

	"scribe-web/internal/config"
)

// EstimateCost converts prompt + completion token counts into a yuan
// amount and a human-readable string. Returns (0, "") when pricing is
// not configured so the API/UI can show "N/A" with a hint to fill in
// price_input_per_mtok / price_output_per_mtok.
func EstimateCost(promptTok, completionTok int, cfg *config.LLMConfig) (float64, string) {
	if cfg == nil || (cfg.PriceInputPerMTok <= 0 && cfg.PriceOutputPerMTok <= 0) {
		return 0, ""
	}
	const million = 1_000_000.0
	cost := float64(promptTok)*cfg.PriceInputPerMTok/million +
		float64(completionTok)*cfg.PriceOutputPerMTok/million
	return cost, formatYuan(cost)
}

// formatYuan picks a precision that doesn't drown sub-cent values: 4
// decimals below ¥0.01, 3 below ¥1, 2 above. Always prefixed with ¥.
func formatYuan(v float64) string {
	switch {
	case v <= 0:
		return "¥0"
	case v < 0.01:
		return fmt.Sprintf("¥%.4f", v)
	case v < 1:
		return fmt.Sprintf("¥%.3f", v)
	default:
		return fmt.Sprintf("¥%.2f", v)
	}
}
