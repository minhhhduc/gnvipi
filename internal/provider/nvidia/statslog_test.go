package nvidia

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

func TestStatsLogFrames(t *testing.T) {
	l := openStatsLogAt(t.TempDir() + "/stats.jsonl")
	t.Cleanup(l.close)
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 450; i++ {
		l.append(RequestEvent{Time: base.Add(time.Duration(i) * time.Second), Model: "m", MS: uint64(i)})
	}
	if got := l.framesCount(); got != 3 {
		t.Fatalf("framesCount = %d, want 3", got)
	}
	ev, total, err := l.readFrame(3)
	if err != nil || total != 3 {
		t.Fatalf("readFrame(3) = total %d, err %v", total, err)
	}
	if len(ev) != 50 {
		t.Fatalf("frame 3 has %d events, want 50", len(ev))
	}
	for i, e := range ev {
		if want := uint64(400 + i); e.MS != want {
			t.Fatalf("frame 3 event %d: MS = %d, want %d", i, e.MS, want)
		}
	}
	if _, _, err := l.readFrame(4); err == nil {
		t.Fatal("readFrame(4) = nil error, want out-of-range")
	}
	if _, _, err := l.readFrame(0); err == nil {
		t.Fatal("readFrame(0) = nil error, want out-of-range")
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
				l.append(RequestEvent{Time: time.Now(), Model: "m"})
			}
		}()
	}
	wg.Wait()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var ev RequestEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("line %d malformed: %v", n+1, err)
		}
		n++
	}
	if n != 200 {
		t.Fatalf("%d lines, want 200", n)
	}
}
