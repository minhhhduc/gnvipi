package nvidia

import (
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
