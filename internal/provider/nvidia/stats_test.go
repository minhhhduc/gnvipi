package nvidia

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStatsEventsRingAndTTFBAvg(t *testing.T) {
	s := &Stats{m: map[string]*statEntry{}}
	// ring cap
	for i := 0; i < eventsKeep+5; i++ {
		s.Observe("m", 1, 1, time.Second, 0, false, false)
	}
	ev := s.Events(eventsKeep + 10)
	if len(ev) != eventsKeep {
		t.Fatalf("ring kept %d events, want %d", len(ev), eventsKeep)
	}
	if !ev[0].Time.After(ev[len(ev)-1].Time) && !ev[0].Time.Equal(ev[len(ev)-1].Time) {
		t.Fatal("events not newest first")
	}
	// n larger than ring clamps; n=0 empty
	if got := len(s.Events(0)); got != 0 {
		t.Fatalf("Events(0) = %d", got)
	}

	// ttfb avg/min/max math
	s2 := &Stats{m: map[string]*statEntry{}}
	s2.Observe("x", 1, 1, 100*time.Millisecond, 10*time.Millisecond, true, false)
	s2.Observe("x", 1, 1, 100*time.Millisecond, 30*time.Millisecond, true, false)
	s2.Observe("x", 1, 1, 100*time.Millisecond, 0, false, false) // non-stream: no sample
	snap := s2.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot models = %d", len(snap))
	}
	got := snap[0]
	if got.TTFBAvg != 20 || got.TTFBMin != 10 || got.TTFBMax != 30 || got.TTFBLast != 30 {
		t.Fatalf("ttfb stats = avg %d min %d max %d last %d, want 20/10/30/30", got.TTFBAvg, got.TTFBMin, got.TTFBMax, got.TTFBLast)
	}
	// no samples -> all zero
	s3 := &Stats{m: map[string]*statEntry{}}
	s3.Observe("y", 1, 1, time.Second, 0, false, false)
	if y := s3.Snapshot()[0]; y.TTFBAvg != 0 || y.TTFBMin != 0 || y.TTFBMax != 0 {
		t.Fatalf("ttfb without samples = %+v", y)
	}
	// Series carries per-minute avg too
	ser := s2.Series(1)
	if len(ser) != 1 || ser[0].TTFBAvg != 20 {
		t.Fatalf("series ttfb avg = %+v", ser)
	}
}

func TestModelSeries(t *testing.T) {
	const n = 5
	s := &Stats{m: map[string]*statEntry{}}
	s.Observe("a", 2, 3, time.Second, 0, false, false)
	s.Observe("a", 1, 1, time.Second, 10*time.Millisecond, true, false)
	s.Observe("b", 4, 5, time.Second, 0, false, true) // error: still a request
	ms := s.ModelSeries(n)
	if len(ms) != 2 {
		t.Fatalf("models = %d, want 2", len(ms))
	}
	for _, want := range []struct {
		model string
		req   uint64
	}{{"a", 2}, {"b", 1}} {
		ser, ok := ms[want.model]
		if !ok {
			t.Fatalf("model %q absent", want.model)
		}
		if len(ser) != n {
			t.Fatalf("model %q len = %d, want %d", want.model, len(ser), n)
		}
		last := ser[n-1]
		if last.Req != want.req {
			t.Fatalf("model %q last req = %d, want %d", want.model, last.Req, want.req)
		}
	}
	if a := ms["a"][n-1]; a.Err != 0 || a.InTok != 3 || a.OutTok != 4 || a.TTFBAvg != 10 {
		t.Fatalf("model a last bucket = %+v", a)
	}
	if _, ok := ms["quiet"]; ok {
		t.Fatal("unobserved model present")
	}
	// global series is the sum
	if g := s.Series(n); g[n-1].Req != 3 {
		t.Fatalf("global last req = %d, want 3", g[n-1].Req)
	}
}

func TestMain(m *testing.M) {
	// Observe persists events; every package run gets an isolated log.
	dir, err := os.MkdirTemp("", "gnvipi-stats-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("GNVIPI_STATS_FILE", filepath.Join(dir, "stats.jsonl")); err != nil {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			log.Printf("stats test cleanup: %v", cleanupErr)
		}
		panic(err)
	}
	code := m.Run()
	if statLog != nil {
		statLog.close()
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("stats test cleanup: %v", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func TestStatsLogFrames(t *testing.T) {
	opts := DefaultStatsLogOptions()
	opts.ReadInterval = time.Nanosecond
	l := boundedStatsLog(t, filepath.Join(t.TempDir(), "stats.jsonl"), opts)
	t.Cleanup(l.close)
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 450; i++ {
		l.append(&RequestEvent{Time: base.Add(time.Duration(i) * time.Second), Model: "m", MS: uint64(i)})
	}
	if got := l.framesCount(); got != 3 {
		t.Fatalf("framesCount = %d, want 3", got)
	}
	// frame 1 = newest entries, newest-first
	ev, total, err := readStatsFrame(t, l, 1)
	if err != nil || total != 3 {
		t.Fatalf("readFrame(1) = total %d, err %v", total, err)
	}
	if len(ev) != 200 {
		t.Fatalf("frame 1 has %d events, want 200", len(ev))
	}
	for i, e := range ev {
		if want := uint64(449 - i); e.MS != want {
			t.Fatalf("frame 1 event %d: MS = %d, want %d", i, e.MS, want)
		}
	}
	if ev3, _, err := readStatsFrame(t, l, 3); err != nil || len(ev3) != 50 || ev3[0].MS != 49 {
		t.Fatalf("readFrame(3) = %d events/oldest, err %v", len(ev3), err)
	}
	if _, _, err := l.readFrame(4); err == nil {
		t.Fatal("readFrame(4) = nil error, want out-of-range")
	}
	if _, _, err := l.readFrame(0); err == nil {
		t.Fatal("readFrame(0) = nil error, want out-of-range")
	}
}

func TestRequestEventAttributionSerialization(t *testing.T) {
	ev := RequestEvent{
		Time: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), Model: "m",
		In: 1, Out: 2, MS: 3, TTFB: 4, Stream: true, Err: false,
		RequestID: "req-1", UserID: "user-1", KeyID: "key-1",
		ProviderID: "prov-1", Status: 200,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"request_id": `"req-1"`, "user_id": `"user-1"`, "key_id": `"key-1"`,
		"provider_id": `"prov-1"`, "status": `200`,
	}
	for key, value := range want {
		if string(got[key]) != value {
			t.Fatalf("%s = %s, want %s (line %s)", key, got[key], value, line)
		}
	}
}

func TestRequestEventLegacyLineStillParses(t *testing.T) {
	// A line written before the attribution fields existed.
	legacy := `{"time":"2026-01-01T00:00:00Z","model":"m","input_tokens":1,"output_tokens":2,"ms":3,"ttfb_ms":4,"streamed":true,"error":false}`
	var ev RequestEvent
	if err := json.Unmarshal([]byte(legacy), &ev); err != nil {
		t.Fatalf("legacy line no longer parses: %v", err)
	}
	if ev.Model != "m" || ev.In != 1 || ev.Out != 2 || ev.MS != 3 || ev.TTFB != 4 || !ev.Stream || ev.Err {
		t.Fatalf("legacy fields changed: %+v", ev)
	}
	if ev.RequestID != "" || ev.UserID != "" || ev.KeyID != "" || ev.ProviderID != "" || ev.Status != 0 {
		t.Fatalf("missing attribution not zero: %+v", ev)
	}
}

func TestRequestEventOmitsEmptyAttribution(t *testing.T) {
	line, err := json.Marshal(RequestEvent{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"request_id", "user_id", "key_id", "provider_id", "status"} {
		if _, ok := got[key]; ok {
			t.Fatalf("empty %s was not omitted: %s", key, line)
		}
	}
}

// The persisted event is an allowlist: no prompt, body, header or credential
// field may ever appear in the JSONL record.
func TestRequestEventJSONHasNoSensitiveFields(t *testing.T) {
	ev := RequestEvent{
		Time: time.Now(), Model: "m", In: 1, Out: 2, MS: 3, TTFB: 4,
		RequestID: "req", UserID: "user", KeyID: "key", ProviderID: "prov", Status: 200,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"time": true, "model": true, "input_tokens": true, "output_tokens": true,
		"ms": true, "ttfb_ms": true, "streamed": true, "error": true,
		"request_id": true, "user_id": true, "key_id": true, "provider_id": true,
		"status": true,
	}
	for key := range got {
		if !allowed[key] {
			t.Fatalf("unexpected field %q in persisted event: %s", key, line)
		}
	}
	if len(got) != len(allowed) {
		t.Fatalf("persisted event has %d fields, want %d: %s", len(got), len(allowed), line)
	}
}

func TestObserveMetaCarriesAttribution(t *testing.T) {
	s := &Stats{m: map[string]*statEntry{}}
	s.ObserveMeta(EventMeta{RequestID: "req", UserID: "user", KeyID: "key", ProviderID: "prov", Status: 200},
		"m", 1, 2, time.Second, 0, false, false)
	ev := s.Events(1)[0]
	if ev.RequestID != "req" || ev.UserID != "user" || ev.KeyID != "key" || ev.ProviderID != "prov" || ev.Status != 200 {
		t.Fatalf("attribution not carried: %+v", ev)
	}
}

func TestStatsLogConcurrentAppends(t *testing.T) {
	path := t.TempDir() + "/stats.jsonl"
	l := openStatsLogAt(path)
	t.Cleanup(l.close)
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				l.append(&RequestEvent{Time: time.Now(), Model: "m"})
			}
		}()
	}
	wg.Wait()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close stats log: %v", err)
		}
	}()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var ev RequestEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("line %d malformed: %v", n+1, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 200 {
		t.Fatalf("%d lines, want 200", n)
	}
}
