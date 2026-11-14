// Package models holds the registry of NVIDIA build.nvidia.com playground
// models and their reverse-engineered NVCF invocation parameters.
//
// The build.nvidia.com playground calls
//
//	POST https://buildapi.ngc.nvidia.com/v2/predict/models/{namespace}/{slug}
//	              nv-function-id: {functionID}
//	              nv-captcha-token: <hCaptcha token>
//
// Each playground model has its own {slug} and nv-function-id. The registry has
// no built-in entries: in database mode it is loaded from the nvidia_models
// table populated by the last rescan, and in file mode from the catalog saved
// in -models-file. The discovery path is in discover.go:
//
//	https://build.nvidia.com/models?pageSize=200
//
// An empty registry is valid: the admin console then offers a rescan.
package models

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Namespace is the NVCF namespace embedded in every playground predict URL.
// All known sandbox models currently share this one namespace.
const Namespace = "qc69jvmznzxy"

// ModelInfo is the per-model NVCF invocation data needed to call the
// (anonymous, hCaptcha-gated) playground predict endpoint.
type ModelInfo struct {
	// Slug is the model name segment used in the predict URL path
	// (everything after the last "/"). E.g. "glm-5.2" for "z-ai/glm-5.2".
	Slug string
	// Namespace is the NVCF namespace for this model. Currently always
	// Namespace, but kept per-model in case NVIDIA splits sandboxes.
	Namespace string
	// FunctionID is the nv-function-id header value (per-model UUID).
	FunctionID string
}

// PredictEndpoint returns the predict URL for a model info.
func (m ModelInfo) PredictEndpoint() string {
	return "https://buildapi.ngc.nvidia.com/v2/predict/models/" + m.Namespace + "/" + m.Slug
}

// PlaygroundGroup is the public view of one publisher block in the registry.
// Exported so cmd/serve's champion candidate sampler can iterate without
// needing to re-parse the saved catalog.
type PlaygroundGroup struct {
	Publisher string          `json:"publisher"`
	Models    []PlaygroundRow `json:"models"`
}

// PlaygroundRow is one model entry inside a PlaygroundGroup.
type PlaygroundRow struct {
	Model      string `json:"model"`
	Slug       string `json:"slug"`
	Namespace  string `json:"namespace"`
	FunctionID string `json:"function_id"`
}

// registry is the runtime catalog of supported build.nvidia.com playground
// models. Startup publishes the stored catalog (database table or saved file),
// and a rescan replaces it atomically so in-flight request handlers never see
// a partial map.
//
// Function ids are almost always unique per model, but NVIDIA aliases some
// backend versions to one NVCF function. That is fine: the predict endpoint is
// keyed by namespace/slug, which IS unique per registry key.
var (
	registryMu sync.RWMutex
	registry   = map[string]ModelInfo{}
)

// Snapshot returns a copy of the current catalog for iteration.
func Snapshot() map[string]ModelInfo {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make(map[string]ModelInfo, len(registry))
	for id, info := range registry {
		out[id] = info
	}
	return out
}

// Get returns one catalog entry.
func Get(model string) (ModelInfo, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	info, ok := registry[model]
	return info, ok
}

// Replace publishes a complete catalog. Callers validate it first.
func Replace(next map[string]ModelInfo) {
	owned := make(map[string]ModelInfo, len(next))
	for id, info := range next {
		owned[id] = info
	}
	registryMu.Lock()
	registry = owned
	registryMu.Unlock()
}

// AllGroups returns the current registry grouped by publisher. The result is
// a copy so callers can sort / shuffle without mutating package state.
func AllGroups() []PlaygroundGroup {
	byPublisher := make(map[string][]PlaygroundRow)
	for id, info := range Snapshot() {
		publisher, slug, found := strings.Cut(id, "/")
		if !found {
			continue
		}
		byPublisher[publisher] = append(byPublisher[publisher], PlaygroundRow{
			Model:      id,
			Slug:       slug,
			Namespace:  info.Namespace,
			FunctionID: info.FunctionID,
		})
	}
	groups := make([]PlaygroundGroup, 0, len(byPublisher))
	for publisher, rows := range byPublisher {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
		groups = append(groups, PlaygroundGroup{Publisher: publisher, Models: rows})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Publisher < groups[j].Publisher })
	return groups
}

// ErrUnknownModel is returned when model is not in the registry.
type ErrUnknownModel struct{ Model string }

func (e *ErrUnknownModel) Error() string {
	return fmt.Sprintf("models: unknown playground model %q (not in the NVIDIA catalog; rescan it from the admin console)", e.Model)
}

// Lookup returns the ModelInfo for model, or an *ErrUnknownModel if it is not
// in the registry.
func Lookup(model string) (ModelInfo, error) {
	info, ok := Get(model)
	if !ok {
		return ModelInfo{}, &ErrUnknownModel{Model: model}
	}
	return info, nil
}
