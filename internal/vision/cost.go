package vision

import (
	"fmt"

	"scribe-web/internal/config"
)

// EstimateCost converts VLM prompt + completion token counts into yuan.
// Providers that only return total_tokens cannot be priced accurately
// because input and output token rates may differ, so callers should show
// "N/A" when either side is unavailable.
func EstimateCost(promptTok, completionTok int, cfg *config.VLMConfig) (float64, string) {
	if promptTok <= 0 && completionTok <= 0 {
		return 0, ""
	}
	if cfg == nil || (cfg.PriceInputPerMTok <= 0 && cfg.PriceOutputPerMTok <= 0) {
		return 0, ""
	}
	const million = 1_000_000.0
	cost := float64(promptTok)*cfg.PriceInputPerMTok/million +
		float64(completionTok)*cfg.PriceOutputPerMTok/million
	return cost, formatYuan(cost)
}

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
