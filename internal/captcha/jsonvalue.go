package captcha

import (
	"encoding/json"
	"fmt"
)

// parseStringArray decodes a JSON array of strings from a chromedp.Value
// passed through chromedp.Evaluate. Truncates / pads to expected length
// n so widget-count drift between renders does not crash the batch path.
func parseStringArray(raw string, n int) ([]string, error) {
	if raw == "" {
		out := make([]string, n)
		return out, nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil, fmt.Errorf("decode batch json: %w", err)
	}
	if len(arr) < n {
		out := make([]string, n)
		copy(out, arr)
		return out, nil
	}
	return arr[:n], nil
}
