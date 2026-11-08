// Package tokencount provides protocol-neutral token count estimation.
package tokencount

import (
	"encoding/json"
	"fmt"
	"unicode"
)

// EstimateText counts approximate tokens in text using ASCII run chunking (4 chars per token)
// and non-ASCII characters (1 token each).
func EstimateText(text string) int {
	count := 0
	asciiRun := 0
	flushASCII := func() {
		if asciiRun > 0 {
			count += (asciiRun + 3) / 4
			asciiRun = 0
		}
	}
	for _, char := range text {
		switch {
		case unicode.IsSpace(char):
			flushASCII()
		case char <= unicode.MaxASCII:
			asciiRun++
		default:
			flushASCII()
			count++
		}
	}
	flushASCII()
	return count
}

// EstimateJSON traverses an arbitrary JSON value and estimates token count.
func EstimateJSON(value any) int {
	switch typed := value.(type) {
	case string:
		return EstimateText(typed)
	case []any:
		count := 1
		for _, item := range typed {
			count += EstimateJSON(item)
		}
		return count
	case map[string]any:
		count := 2
		for key, item := range typed {
			switch key {
			case "model", "role", "type":
				continue
			default:
				count += EstimateJSON(item)
			}
		}
		return count
	default:
		return 0
	}
}

// EstimateRequest decodes a JSON payload and estimates token count, returning at least 1.
func EstimateRequest(payload []byte) (int, error) {
	var request any
	if err := json.Unmarshal(payload, &request); err != nil {
		return 0, fmt.Errorf("decode token count request: %w", err)
	}
	count := EstimateJSON(request)
	if count == 0 {
		return 1, nil
	}
	return count, nil
}
