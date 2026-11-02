package nvidia

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func boundedStatsLog(t *testing.T, path string, opts StatsLogOptions) *statsLog {
	t.Helper()
	l := openStatsLogAt(path)
	t.Cleanup(l.close)
	if err := l.configure(opts); err != nil {
		t.Fatal(err)
	}
	return l
}

func smallStatsOptions() StatsLogOptions {
	opts := DefaultStatsLogOptions()
	opts.MaxBytes = 2048
	opts.SegmentBytes = 512
	opts.ReadInterval = time.Nanosecond
	return opts
}

func statsDiskBytes(t *testing.T, path string) int64 {
	t.Helper()
	paths, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	return total
}

// Wait for the configured read cadence, including clocks coarser than 1ns.
func readStatsFrame(t *testing.T, l *statsLog, n int) ([]RequestEvent, int, error) {
	t.Helper()
	l.mu.Lock()
	ready := l.readAt.Add(l.opts.ReadInterval)
	l.mu.Unlock()
	for time.Now().Before(ready) {
		select {
		case <-t.Context().Done():
			t.Fatal("timed out waiting for stats read cadence")
		case <-time.After(time.Millisecond):
		}
	}
	return l.readFrame(n)
}

func statsHistory(t *testing.T, l *statsLog) []RequestEvent {
	t.Helper()
	var events []RequestEvent
	for n, total := 1, l.framesCount(); n <= total; n++ {
		frame, gotTotal, err := readStatsFrame(t, l, n)
		if err != nil || gotTotal != total {
			t.Fatalf("frame %d: total %d, err %v", n, gotTotal, err)
		}
		events = append(events, frame...)
	}
	return events
}

// Removing append-time eviction must fail even when nobody reads the dashboard.
func TestStatsLogSizeRotationWithoutReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := smallStatsOptions()
	l := boundedStatsLog(t, path, opts)
	for i := range 100 {
		l.append(&RequestEvent{Time: time.Now(), Model: "m", MS: uint64(i)})
		if got := statsDiskBytes(t, path); got > opts.MaxBytes {
			t.Fatalf("append %d: disk bytes %d exceed %d", i, got, opts.MaxBytes)
		}
	}
	files, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("small segment size did not rotate")
	}
	for _, p := range append(files, path) {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > opts.SegmentBytes {
			t.Fatalf("segment %s has %d bytes, cap %d", p, fi.Size(), opts.SegmentBytes)
		}
	}
	events := statsHistory(t, l)
	if len(events) == 0 || events[0].MS != 99 || len(events) >= 100 {
		t.Fatalf("expected newest retained suffix, got %+v", events)
	}
	for i := 1; i < len(events); i++ {
		if events[i].MS+1 != events[i-1].MS {
			t.Fatalf("retained suffix is not contiguous: %+v", events)
		}
	}
}

// Losing the serial rotation lock causes malformed records, duplicates, or cap overshoot.
func TestStatsLogConcurrentRotationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := smallStatsOptions()
	opts.MaxBytes = 64 * 1024
	l := boundedStatsLog(t, path, opts)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 30 {
				l.append(&RequestEvent{Time: time.Now(), Model: fmt.Sprintf("%d/%d", g, i)})
			}
		}(g)
	}
	wg.Wait()
	l.close()
	l = boundedStatsLog(t, path, opts)
	events := statsHistory(t, l)
	if len(events) != 240 {
		t.Fatalf("restart recovered %d events, want 240", len(events))
	}
	seen := make(map[string]bool)
	for _, ev := range events {
		if seen[ev.Model] {
			t.Fatalf("duplicate event %q", ev.Model)
		}
		seen[ev.Model] = true
	}
	l.append(&RequestEvent{Time: time.Now(), Model: "after-restart"})
	if got := statsHistory(t, l)[0].Model; got != "after-restart" {
		t.Fatalf("newest event after restart = %q", got)
	}
	if got := statsDiskBytes(t, path); got > opts.MaxBytes {
		t.Fatalf("concurrent rotation used %d bytes, cap %d", got, opts.MaxBytes)
	}
}

func TestStatsLogTimeRotationAndRetentionInvalidatesCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := smallStatsOptions()
	l := boundedStatsLog(t, path, opts)
	l.append(&RequestEvent{Time: time.Now(), Model: "old"})
	l.mu.Lock()
	l.segments[len(l.segments)-1].created = time.Now().Add(-opts.RotateInterval - time.Second)
	l.mu.Unlock()
	l.append(&RequestEvent{Time: time.Now(), Model: "new"})
	if got := len(statsHistory(t, l)); got != 2 {
		t.Fatalf("time rotation lost events: %d", got)
	}
	l.mu.Lock()
	l.segments[0].modified = time.Now().Add(-opts.Retention - time.Second)
	l.mu.Unlock()
	if got := statsHistory(t, l); len(got) != 1 || got[0].Model != "new" {
		t.Fatalf("retention returned stale cached history: %+v", got)
	}
	files, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expired archived segment remains: %v", files)
	}
}

func TestStatsLogRetentionOnAppendAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := smallStatsOptions()
	l := boundedStatsLog(t, path, opts)
	l.append(&RequestEvent{Time: time.Now(), Model: "expired"})
	l.close()
	old := time.Now().Add(-opts.Retention - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	l = boundedStatsLog(t, path, opts)
	if got := l.framesCount(); got != 0 {
		t.Fatalf("restart exposed %d expired frames", got)
	}
	l.append(&RequestEvent{Time: time.Now(), Model: "fresh"})
	l.mu.Lock()
	l.segments[len(l.segments)-1].modified = old
	l.mu.Unlock()
	l.append(&RequestEvent{Time: time.Now(), Model: "after-retention"})
	if got := statsHistory(t, l); len(got) != 1 || got[0].Model != "after-retention" {
		t.Fatalf("append did not expire old log: %+v", got)
	}
}

func TestStatsLogOversizedLegacyAndRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := smallStatsOptions()
	var raw strings.Builder
	for i := range 100 {
		line, err := json.Marshal(RequestEvent{Time: time.Now(), Model: "legacy", MS: uint64(i)})
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(line)
		raw.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(raw.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	l := boundedStatsLog(t, path, opts)
	if got := statsDiskBytes(t, path); got > opts.MaxBytes {
		t.Fatalf("configure left legacy file over cap: %d", got)
	}
	events := statsHistory(t, l)
	if len(events) == 0 || events[0].MS != 99 {
		t.Fatalf("legacy normalization did not retain newest complete records: %+v", events)
	}
	l.append(&RequestEvent{Time: time.Now(), Model: strings.Repeat("x", int(opts.SegmentBytes))})
	if got := statsDiskBytes(t, path); got > opts.MaxBytes {
		t.Fatalf("oversized record broke disk cap: %d", got)
	}
	if got := statsHistory(t, l); len(got) != len(events) || got[0].MS != 99 {
		t.Fatalf("oversized record was retained: %+v", got)
	}
	l.append(&RequestEvent{Time: time.Now(), Model: "normal"})
	if got := statsHistory(t, l)[0].Model; got != "normal" {
		t.Fatalf("oversized record prevented subsequent append: %q", got)
	}
}

func TestStatsLogCorruptAndPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	line, err := json.Marshal(RequestEvent{Model: "first"})
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte("\nnot-json\n \n"), line...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	l := boundedStatsLog(t, path, smallStatsOptions())
	l.append(&RequestEvent{Model: "second"})
	events := statsHistory(t, l)
	if len(events) != 2 || events[0].Model != "second" || events[1].Model != "first" {
		t.Fatalf("corrupt/partial lines changed usable history: %+v", events)
	}
}

func TestStatsLogBoundedHistoryReadRate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := DefaultStatsLogOptions()
	opts.ReadInterval = time.Hour
	l := boundedStatsLog(t, path, opts)
	for i := range 201 {
		l.append(&RequestEvent{Model: "m", MS: uint64(i)})
	}
	first, _, err := l.readFrame(1)
	if err != nil || first[0].MS != 200 {
		t.Fatalf("first read: %+v, %v", first, err)
	}
	first[0].MS = 999 // caller must not be able to mutate the cached response.
	l.append(&RequestEvent{Model: "m", MS: 201})
	cached, _, err := l.readFrame(1)
	if err != nil || cached[0].MS != 200 {
		t.Fatalf("repeated poll bypassed read interval or shared cache: %+v, %v", cached, err)
	}
	if _, _, err := l.readFrame(2); !errors.Is(err, ErrStatsReadLimited) {
		t.Fatalf("history switching rate limit = %v", err)
	}
}

func TestStatsLogLazyAndInvalidOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "stats.jsonl")
	opts := smallStatsOptions()
	l := boundedStatsLog(t, path, opts)
	if got := l.framesCount(); got != 0 {
		t.Fatalf("empty history has %d frames", got)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("read/configure created log directory: %v", err)
	}
	for _, bad := range []StatsLogOptions{
		{},
		{MaxBytes: 512, SegmentBytes: 513, RotateInterval: time.Hour, Retention: time.Hour, ReadInterval: time.Second},
		{MaxBytes: 512, SegmentBytes: 256, RotateInterval: -time.Second, Retention: time.Hour, ReadInterval: time.Second},
		{MaxBytes: 512, SegmentBytes: 256, RotateInterval: time.Hour, Retention: time.Hour, ReadInterval: 720 * time.Hour},
	} {
		if err := l.configure(bad); err == nil {
			t.Fatalf("accepted invalid options: %+v", bad)
		}
	}
	l.append(&RequestEvent{Model: "still-valid"})
	if got := statsHistory(t, l)[0].Model; got != "still-valid" {
		t.Fatalf("invalid configure changed working configuration: %q", got)
	}
}

// Removing the maintenance worker must leave an expired file behind when idle.
func TestStatsLogIdleRetentionWithoutDashboard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := smallStatsOptions()
	opts.Retention = 20 * time.Millisecond
	l := boundedStatsLog(t, path, opts)
	l.append(&RequestEvent{Model: "idle"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("idle expired stats file was not removed without reads/appends")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStatsLogConcurrentEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	opts := smallStatsOptions()
	l := boundedStatsLog(t, path, opts)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 30 {
				l.append(&RequestEvent{Model: fmt.Sprintf("%d/%d", g, i)})
			}
		}(g)
	}
	wg.Wait()
	if got := statsDiskBytes(t, path); got > opts.MaxBytes {
		t.Fatalf("concurrent eviction exceeded disk cap: %d", got)
	}
	l.close()
	l = boundedStatsLog(t, path, opts)
	events := statsHistory(t, l)
	if len(events) == 0 || len(events) >= 240 {
		t.Fatalf("expected bounded non-empty retained history, got %d events", len(events))
	}
	seen := make(map[string]bool)
	for _, ev := range events {
		if seen[ev.Model] {
			t.Fatalf("duplicate retained record %q", ev.Model)
		}
		var g, i int
		if _, err := fmt.Sscanf(ev.Model, "%d/%d", &g, &i); err != nil || g < 0 || g >= 8 || i < 0 || i >= 30 {
			t.Fatalf("corrupt retained record: %+v", ev)
		}
		seen[ev.Model] = true
	}
}
