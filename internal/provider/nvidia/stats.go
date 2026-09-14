package nvidia

import (
	"bytes"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// ModelStats is one model's cumulative request/usage counters. Values are
// read snapshots — safe to hand to JSON without locks.
type ModelStats struct {
	Model     string     `json:"model"`
	Requests  uint64     `json:"requests"`
	Errors    uint64     `json:"errors"`
	Streamed  uint64     `json:"streamed"`
	InputTok  uint64     `json:"input_tokens"`
	OutputTok uint64     `json:"output_tokens"`
	FirstOK   *time.Time `json:"first_ok,omitempty"`
	LastOK    *time.Time `json:"last_ok,omitempty"`
	AvgMS     uint64     `json:"avg_ms"`
	LastMS    uint64     `json:"last_ms"`
	TTFBAvg   uint64     `json:"ttfb_avg_ms"`
	TTFBLast  uint64     `json:"ttfb_last_ms"`
	TTFBMin   uint64     `json:"ttfb_min_ms"`
	TTFBMax   uint64     `json:"ttfb_max_ms"`
}

type statEntry struct {
	requests, errors, streamed, inTok, outTok, msTotal, nMS, lastMS uint64
	ttfbTotal, ttfbN, ttfbLast, ttfbMin, ttfbMax                    uint64
	firstOK, lastOK                                                 time.Time
}

// MinuteStats is one minute of global activity for the admin charts.
type MinuteStats struct {
	Minute  time.Time `json:"minute"`
	Req     uint64    `json:"requests"`
	Err     uint64    `json:"errors"`
	InTok   uint64    `json:"input_tokens"`
	OutTok  uint64    `json:"output_tokens"`
	TTFBAvg uint64    `json:"ttfb_avg_ms"`
	TTFBSum uint64    `json:"-"`
	TTFBN   uint64    `json:"-"`
}

// seriesKeep caps the per-minute ring (charts read the last 60).
const seriesKeep = 120

// eventsKeep caps the per-request log (admin shows the last ~200 calls).
const eventsKeep = 200

// RequestEvent is one upstream call: what it cost, how long it ran, and
// whether it failed. Newest first when read.
type RequestEvent struct {
	Time    time.Time `json:"time"`
	Model   string    `json:"model"`
	In      uint64    `json:"input_tokens"`
	Out     uint64    `json:"output_tokens"`
	MS      uint64    `json:"ms"`
	TTFB    uint64    `json:"ttfb_ms"`
	Stream  bool      `json:"streamed"`
	Err     bool      `json:"error"`
}

// Stats collects per-model request counters and token usage scraped from
// upstream responses (the single point both stream and non-stream responses
// pass through). Lazy: a map guarded by one mutex, written once per request.
type Stats struct {
	mu     sync.Mutex
	m      map[string]*statEntry
	series []*MinuteStats  // per-minute global ring, newest last
	events []RequestEvent  // per-request ring, newest last
}

// bucket returns (creating if needed) the MinuteStats for t's minute.
// The slice is a ring capped at seriesKeep entries.
func (s *Stats) bucket(t time.Time) *MinuteStats {
	min := t.Truncate(time.Minute)
	if n := len(s.series); n > 0 && !s.series[n-1].Minute.Equal(min) {
		s.series = append(s.series, &MinuteStats{Minute: min})
		if len(s.series) > seriesKeep {
			s.series = s.series[len(s.series)-seriesKeep:]
		}
	}
	if len(s.series) == 0 {
		s.series = append(s.series, &MinuteStats{Minute: min})
	}
	return s.series[len(s.series)-1]
}

// startedAt is process start, exposed so the admin surface can show uptime.
var startedAt = time.Now()

// GlobalStats is the process-wide collector the admin surface reads.
var GlobalStats = &Stats{m: map[string]*statEntry{}}

// StartedAt returns the process start time.
func (s *Stats) StartedAt() time.Time { return startedAt }

// Observe records a finished upstream request. in/out are token counts
// scraped from the response (0 when unavailable). dur is wall time of the
// upstream call. ttfb is time-to-first-token for streams (0 for non-stream).
func (s *Stats) Observe(model string, in, out uint64, dur, ttfb time.Duration, streamed, isErr bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := uint64(dur.Milliseconds())
	tms := uint64(ttfb.Milliseconds())
	ev := RequestEvent{
		Time: time.Now(), Model: model, In: in, Out: out, MS: ms, TTFB: tms, Stream: streamed, Err: isErr,
	}
	s.events = append(s.events, ev)
	AppendLog(ev)
	if len(s.events) > eventsKeep {
		s.events = s.events[len(s.events)-eventsKeep:]
	}
	e := s.m[model]
	if e == nil {
		e = &statEntry{}
		s.m[model] = e
	}
	e.requests++
	b := s.bucket(time.Now())
	b.Req++
	if isErr {
		e.errors++
		b.Err++
		return
	}
	if e.firstOK.IsZero() {
		e.firstOK = time.Now()
	}
	e.lastOK = time.Now()
	if streamed {
		e.streamed++
	}
	e.inTok += in
	e.outTok += out
	b.InTok += in
	b.OutTok += out
	e.msTotal += ms
	e.nMS++
	e.lastMS = ms
	if streamed && tms > 0 {
		e.ttfbTotal += tms
		e.ttfbN++
		e.ttfbLast = tms
		if e.ttfbMin == 0 || tms < e.ttfbMin {
			e.ttfbMin = tms
		}
		if tms > e.ttfbMax {
			e.ttfbMax = tms
		}
		b.TTFBSum += tms
		b.TTFBN++
	}
}

// Snapshot returns all models sorted by requests desc.
func (s *Stats) Snapshot() []ModelStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ModelStats, 0, len(s.m))
	for model, e := range s.m {
		ms := ModelStats{
			Model:     model,
			Requests:  e.requests,
			Errors:    e.errors,
			Streamed:  e.streamed,
			InputTok:  e.inTok,
			OutputTok: e.outTok,
			LastMS:    e.lastMS,
		}
		if !e.firstOK.IsZero() {
			ms.FirstOK = &e.firstOK
		}
		if !e.lastOK.IsZero() {
			ms.LastOK = &e.lastOK
		}
		if e.nMS > 0 {
			ms.AvgMS = e.msTotal / e.nMS
		}
		if e.ttfbN > 0 {
			ms.TTFBAvg = e.ttfbTotal / e.ttfbN
			ms.TTFBLast = e.ttfbLast
			ms.TTFBMin = e.ttfbMin
			ms.TTFBMax = e.ttfbMax
		}
		out = append(out, ms)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}

// Series returns the last n minutes of global activity (n<=seriesKeep),
// oldest first. Gaps between minutes are zero-filled up to n minutes.
func (s *Stats) Series(n int) []MinuteStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > seriesKeep {
		n = seriesKeep
	}
	now := time.Now().Truncate(time.Minute)
	out := make([]MinuteStats, 0, n)
	byMin := make(map[time.Time]MinuteStats, len(s.series))
	for _, b := range s.series {
		byMin[b.Minute] = *b
	}
	for i := n - 1; i >= 0; i-- {
		min := now.Add(-time.Duration(i) * time.Minute)
		out = append(out, byMin[min]) // zero MinuteStats when the minute was quiet
	}
	for i := range out {
		out[i].Minute = now.Add(-time.Duration(n-1-i) * time.Minute)
		if out[i].TTFBN > 0 {
			out[i].TTFBAvg = out[i].TTFBSum / out[i].TTFBN
		}
	}
	return out
}

// RecordResponse scrapes usage from a raw upstream response body (JSON or SSE)
// and records the call — for traffic that bypasses the nvidia executor, e.g.
// admin-added custom providers.
func (s *Stats) RecordResponse(model string, body []byte, dur time.Duration, streamed, isErr bool) {
	in, out := scrapeUsage(body)
	s.Observe(model, in, out, dur, 0, streamed, isErr)
}

// Events returns the last n request events, newest first.
func (s *Stats) Events(n int) []RequestEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.events) {
		n = len(s.events)
	}
	out := make([]RequestEvent, 0, n)
	for i := len(s.events) - 1; i >= len(s.events)-n; i-- {
		out = append(out, s.events[i])
	}
	return out
}

// scrapeUsage pulls token counts out of an upstream response. Non-stream:
// chat.completion JSON with a top-level usage. Stream: SSE data: lines whose
// chunk carries usage (include_usage=1 is forced in preparePayload). Returns
// 0,0 when no usage is found — stats must never break the request path.
func scrapeUsage(raw []byte) (in, out uint64) {
	// Fast path: plain JSON body (non-stream).
	if i, o := scrapeUsageObj(raw); i != 0 || o != 0 {
		return i, o
	}
	// Stream: last data: line with a usage object wins.
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if i, o := scrapeUsageObj(payload); i != 0 || o != 0 {
			in, out = i, o
		}
	}
	return in, out
}

// scrapeUsageObj decodes one JSON object and pulls usage tokens (OpenAI shape
// prompt_tokens/completion_tokens; NVIDIA sometimes emits them as strings).
func scrapeUsageObj(raw []byte) (in, out uint64) {
	var obj struct {
		Usage *struct {
			PromptTokens     json.Number `json:"prompt_tokens"`
			CompletionTokens json.Number `json:"completion_tokens"`
			InputTokens      json.Number `json:"input_tokens"`
			OutputTokens     json.Number `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &obj) != nil || obj.Usage == nil {
		return 0, 0
	}
	for _, n := range []struct {
		v json.Number
		t *uint64
	}{
		{obj.Usage.PromptTokens, &in},
		{obj.Usage.InputTokens, &in},
		{obj.Usage.CompletionTokens, &out},
		{obj.Usage.OutputTokens, &out},
	} {
		if u, err := n.v.Int64(); err == nil && u > 0 {
			*n.t = uint64(u)
		}
	}
	return in, out
}
