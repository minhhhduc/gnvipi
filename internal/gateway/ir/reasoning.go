package ir

import "strings"

// DefaultEffortToBudget maps standard ReasoningEffort levels to default token budgets.
var DefaultEffortToBudget = map[ReasoningEffort]int{
	ReasoningEffortLow:    1024,
	ReasoningEffortMedium: 4096,
	ReasoningEffortHigh:   16384,
}

// ReasoningMappingConfig configures bidirectional mapping between effort and budget.
type ReasoningMappingConfig struct {
	EffortToBudget map[ReasoningEffort]int
}

var currentMapping = ReasoningMappingConfig{
	EffortToBudget: map[ReasoningEffort]int{
		ReasoningEffortLow:    1024,
		ReasoningEffortMedium: 4096,
		ReasoningEffortHigh:   16384,
	},
}

// NormalizeReasoningEffort normalizes an effort string into ReasoningEffort.
func NormalizeReasoningEffort(effort string) ReasoningEffort {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "minimal":
		return ReasoningEffortLow
	case "medium":
		return ReasoningEffortMedium
	case "high", "xhigh", "max":
		return ReasoningEffortHigh
	default:
		return ReasoningEffort(effort)
	}
}

// EffortToBudget converts an effort level to a token budget.
func EffortToBudget(effort ReasoningEffort) int {
	normalized := NormalizeReasoningEffort(string(effort))
	if b, ok := currentMapping.EffortToBudget[normalized]; ok && b > 0 {
		return b
	}
	if b, ok := DefaultEffortToBudget[normalized]; ok {
		return b
	}
	return DefaultEffortToBudget[ReasoningEffortMedium]
}

// BudgetToEffort converts a token budget to the nearest ReasoningEffort.
func BudgetToEffort(budget int) ReasoningEffort {
	if budget <= 2048 {
		return ReasoningEffortLow
	}
	if budget <= 8192 {
		return ReasoningEffortMedium
	}
	return ReasoningEffortHigh
}

// SetReasoningMapping configures the global effort-to-budget mapping table.
func SetReasoningMapping(cfg ReasoningMappingConfig) {
	if cfg.EffortToBudget != nil {
		currentMapping = cfg
	}
}

// ResetReasoningMapping resets the mapping table to default values.
func ResetReasoningMapping() {
	currentMapping = ReasoningMappingConfig{
		EffortToBudget: map[ReasoningEffort]int{
			ReasoningEffortLow:    1024,
			ReasoningEffortMedium: 4096,
			ReasoningEffortHigh:   16384,
		},
	}
}
