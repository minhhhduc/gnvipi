// Package redact filters configured credentials and common authentication
// forms before diagnostics, errors or persistent statistics reach a sink.
package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

var authPattern = regexp.MustCompile(`(?i)((?:authorization|proxy-authorization|x-api-key|api[_-]?key)\s*["']?\s*[:=]\s*["']?\s*)(?:bearer\s+|basic\s+)?(?:\[redacted\]|[^\s"'\\,;\]}&]+)`)
var keyPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`)
var userinfoPattern = regexp.MustCompile(`(?i)((?:https?|socks5h?)://)[^/@\s"'?#]+@`)

type state struct {
	raw      map[string]struct{}
	replacer *strings.Replacer
	mask     string
}

// Redactor publishes immutable replacement tables. Update retains previous
// credentials until process exit so a rotation cannot expose an in-flight key.
type Redactor struct {
	mu      sync.Mutex
	known   map[string]struct{}
	current atomic.Pointer[state]
}

var Default Redactor

func (r *Redactor) Update(keys ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known == nil {
		r.known = make(map[string]struct{})
	}
	changed := false
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, found := r.known[key]; !found {
			r.known[key] = struct{}{}
			changed = true
		}
	}
	if changed {
		r.current.Store(buildState(r.known))
	}
}

func buildState(keys map[string]struct{}) *state {
	s := &state{raw: make(map[string]struct{}, len(keys))}
	variants := make(map[string]struct{}, 4*len(keys))
	for key := range keys {
		s.raw[key] = struct{}{}
		encoded, err := json.Marshal(key)
		if err != nil {
			panic(err)
		} // A Go string has no fallible JSON marshaler.
		for _, variant := range []string{key, url.QueryEscape(key), url.PathEscape(key), string(encoded[1 : len(encoded)-1])} {
			if variant != "" {
				variants[variant] = struct{}{}
			}
		}
	}
	for _, candidate := range []string{"[redacted]", "***", ""} {
		safe := true
		for key := range variants {
			if strings.Contains(candidate, key) {
				safe = false
				break
			}
		}
		if safe {
			s.mask = candidate
			break
		}
	}
	ordered := make([]string, 0, len(variants))
	for key := range variants {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) == len(ordered[j]) {
			return ordered[i] < ordered[j]
		}
		return len(ordered[i]) > len(ordered[j])
	})
	pairs := make([]string, 0, 2*len(ordered))
	for _, key := range ordered {
		pairs = append(pairs, key, s.mask)
	}
	s.replacer = strings.NewReplacer(pairs...)
	return s
}

func render(text string, s *state) string {
	mask := "[redacted]"
	if s != nil {
		mask = s.mask
	}
	text = userinfoPattern.ReplaceAllString(text, "${1}"+mask+"@")
	text = authPattern.ReplaceAllString(text, "${1}"+mask)
	text = keyPattern.ReplaceAllString(text, mask)
	if s != nil {
		text = s.replacer.Replace(text)
	}
	return text
}

func (r *Redactor) Text(text string) string { return render(text, r.current.Load()) }

// HeaderValue retains authentication context when filtering header values.
func (r *Redactor) HeaderValue(name, value string) string {
	s := r.current.Load()
	if isAuthProperty(name) {
		if s != nil {
			return s.mask
		}
		return "[redacted]"
	}
	return render(value, s)
}

func isAuthProperty(name string) bool {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-")) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "apikey":
		return true
	}
	return false
}

// TextWithSecrets adds request-scoped credentials without mutating the global
// configured set. Empty extras use the allocation-free published snapshot.
func (r *Redactor) TextWithSecrets(text string, keys ...string) string {
	s := r.current.Load()
	if len(keys) == 0 {
		return render(text, s)
	}
	combined := make(map[string]struct{})
	if s != nil {
		for key := range s.raw {
			combined[key] = struct{}{}
		}
	}
	for _, key := range keys {
		if key != "" {
			combined[key] = struct{}{}
		}
	}
	if len(combined) == 0 {
		return render(text, s)
	}
	return render(text, buildState(combined))
}

// JSON preserves JSON value types and escapes while filtering data strings.
// Non-JSON upstream error bodies retain their original format after filtering.
func (r *Redactor) JSON(raw []byte) ([]byte, error) {
	if !json.Valid(raw) {
		return []byte(r.Text(string(raw))), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	clean, changed := scrubValue(value, r.current.Load(), false)
	if !changed {
		return raw, nil
	}
	return json.Marshal(clean)
}

func scrubValue(value any, s *state, sensitive bool) (any, bool) {
	changed := false
	switch v := value.(type) {
	case string:
		var clean string
		if sensitive {
			clean = "[redacted]"
			if s != nil {
				clean = s.mask
			}
		} else {
			clean = render(v, s)
		}
		return clean, clean != v
	case []any:
		for i := range v {
			clean, itemChanged := scrubValue(v[i], s, sensitive)
			v[i] = clean
			changed = changed || itemChanged
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			cleanKey := render(key, s)
			clean, itemChanged := scrubValue(item, s, sensitive || isAuthProperty(key))
			out[cleanKey] = clean
			changed = changed || itemChanged || cleanKey != key
		}
		return out, changed
	}
	return value, changed
}

type filteredWriter struct {
	r    *Redactor
	base io.Writer
}

// WrapWriter filters complete log records. Redact structured entries before
// formatting when a logger may split a record across multiple writes.
func (r *Redactor) WrapWriter(base io.Writer) io.Writer { return &filteredWriter{r: r, base: base} }
func (w *filteredWriter) Write(p []byte) (int, error) {
	original := string(p)
	clean := w.r.Text(original)
	if clean == original {
		return w.base.Write(p)
	}
	n, err := w.base.Write([]byte(clean))
	if err == nil && n != len(clean) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
