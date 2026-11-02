package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

func TestMain(m *testing.M) {
	// The collector keeps its append handle open for the process lifetime.
	// Match the NVIDIA tests' stable temp-file convention on Windows.
	os.Setenv("GNVIPI_STATS_FILE", filepath.Join(os.TempDir(), "gnvipi-core-test-stats.jsonl"))
	os.Exit(m.Run())
}

type captureRecorder struct {
	calls atomic.Int32
	done  chan Completion
}

func (r *captureRecorder) Record(c Completion) {
	r.calls.Add(1)
	r.done <- c
}

func receiveCompletion(t *testing.T, r *captureRecorder) Completion {
	t.Helper()
	select {
	case c := <-r.done:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete")
		return Completion{}
	}
}

func TestRequestCompletionRecordsOneTerminalOutcome(t *testing.T) {
	upstreamErr := errors.New("upstream failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"upstream_error", upstreamErr},
		{"client_cancel", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &captureRecorder{done: make(chan Completion, 8)}
			rc := NewRequestContext("terminal-" + tc.name)
			rc.ClientModel = "provider/client-model"
			rc.ResolvedProvider = "provider"
			rc.UpstreamModel = "actual-model"
			rc.Start = time.Now().Add(-time.Second)
			rc.FirstByte = rc.Start.Add(25 * time.Millisecond)
			rc.Stream = true
			if !rc.BeginRecording(t.Context(), r) {
				t.Fatal("first recorder was rejected")
			}
			usage := ir.Usage{InputTokens: 7, OutputTokens: 3}
			if !rc.Complete(usage, tc.err) || rc.Complete(ir.Usage{}, errors.New("late error")) {
				t.Fatal("terminal outcome was not recorded exactly once")
			}
			c := receiveCompletion(t, r)
			if !errors.Is(c.Err, tc.err) || c.Usage != usage || c.TTFB != 25*time.Millisecond || c.Duration < time.Second {
				t.Fatalf("completion=%+v", c)
			}
			if c.Request.ID != "terminal-"+tc.name || c.Request.ResolvedProvider != "provider" || c.Request.UpstreamModel != "actual-model" || r.calls.Load() != 1 {
				t.Fatalf("request=%+v calls=%d", c.Request, r.calls.Load())
			}
		})
	}
}

func TestRequestCancellationRecordsWithoutExplicitCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r := &captureRecorder{done: make(chan Completion, 8)}
	rc := NewRequestContext("cancel")
	rc.BeginRecording(ctx, r)
	cancel()
	c := receiveCompletion(t, r)
	if !errors.Is(c.Err, context.Canceled) || rc.Complete(ir.Usage{}, nil) || r.calls.Load() != 1 {
		t.Fatalf("completion=%+v calls=%d", c, r.calls.Load())
	}
}

func TestRequestSuccessDisarmsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &captureRecorder{done: make(chan Completion, 8)}
	rc := NewRequestContext("success-before-cancel")
	rc.BeginRecording(ctx, r)
	rc.Complete(ir.Usage{InputTokens: 2}, nil)
	cancel()
	c := receiveCompletion(t, r)
	if c.Err != nil || rc.Complete(ir.Usage{}, ctx.Err()) || r.calls.Load() != 1 {
		t.Fatalf("completion=%+v calls=%d", c, r.calls.Load())
	}
}

func TestRequestCanceledBeforeSuccessCannotRecordSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &captureRecorder{done: make(chan Completion, 8)}
	rc := NewRequestContext("already-canceled")
	rc.BeginRecording(ctx, r)
	rc.Complete(ir.Usage{}, nil)
	if c := receiveCompletion(t, r); !errors.Is(c.Err, context.Canceled) {
		t.Fatalf("canceled request recorded success: %+v", c)
	}
}

func TestRequestCompletionConcurrentRequestsAndTerminalPaths(t *testing.T) {
	const requests = 64
	r := &captureRecorder{done: make(chan Completion, requests*4)}
	var wg sync.WaitGroup
	for i := range requests {
		ctx, cancel := context.WithCancel(t.Context())
		rc := NewRequestContext(fmt.Sprintf("concurrent-%d", i))
		rc.BeginRecording(ctx, r)
		wg.Add(3)
		go func() { defer wg.Done(); rc.MarkFirstByte(); rc.Complete(ir.Usage{InputTokens: 1}, nil) }()
		go func() { defer wg.Done(); rc.MarkFirstByte(); rc.Complete(ir.Usage{}, errors.New("upstream")) }()
		go func() { defer wg.Done(); cancel(); rc.Complete(ir.Usage{}, ctx.Err()) }()
	}
	wg.Wait()
	seen := make(map[string]bool)
	for range requests {
		c := receiveCompletion(t, r)
		if seen[c.Request.ID] {
			t.Fatalf("request %q completed twice", c.Request.ID)
		}
		seen[c.Request.ID] = true
	}
	if r.calls.Load() != requests {
		t.Fatalf("records=%d, want %d", r.calls.Load(), requests)
	}
}

func TestRequestFirstByteIsNotOverwritten(t *testing.T) {
	r := &captureRecorder{done: make(chan Completion, 8)}
	rc := NewRequestContext("first-byte")
	first := rc.Start.Add(10 * time.Millisecond)
	rc.FirstByte = first
	rc.BeginRecording(t.Context(), r)
	rc.MarkFirstByte()
	rc.Complete(ir.Usage{}, nil)
	rc.MarkFirstByte()
	if c := receiveCompletion(t, r); !c.Request.FirstByte.Equal(first) || c.TTFB != 10*time.Millisecond {
		t.Fatalf("first byte overwritten: %+v", c)
	}
}

func TestRequestContextLegacyDoesNotRecord(t *testing.T) {
	rc := NewRequestContext("legacy")
	ctx := WithRequestContext(t.Context(), rc)
	if rc.Complete(ir.Usage{}, nil) {
		t.Fatal("request was recorded without opting into normalized accounting")
	}
	if RequestContextFrom(ctx) != rc || RequestContextFrom(context.Background()) != nil {
		t.Fatal("context isolation failed")
	}
}

func TestDefaultRecorderAccountsIRUsageAndEscapesAccessLog(t *testing.T) {
	var output bytes.Buffer
	oldOutput, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOutput); log.SetFlags(oldFlags) })
	rc := NewRequestContext("")
	rc.ClientModel = "recorder-test/" + rc.ID
	rc.ResolvedProvider = "provider\n\"injected\""
	rc.UpstreamModel = "model\n\"injected\""
	rc.Stream = true
	rc.Start = time.Now().Add(-time.Second)
	rc.FirstByte = rc.Start.Add(40 * time.Millisecond)
	rc.BeginRecording(t.Context(), nil)
	usage := ir.Usage{InputTokens: 11, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2, ReasoningTokens: 1}
	rc.Complete(usage, nil)
	rc.Complete(usage, context.Canceled)
	var row *nvidia.ModelStats
	for _, s := range nvidia.GlobalStats.Snapshot() {
		if s.Model == rc.ClientModel {
			row = &s
		}
	}
	if row == nil || row.Requests != 1 || row.Errors != 0 || row.InputTok != 11 || row.OutputTok != 5 || row.TTFBLast != 40 {
		t.Fatalf("accounting=%+v", row)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("access log injection or duplicate record: %q", output.String())
	}
	var access struct {
		RequestID     string   `json:"request_id"`
		Provider      string   `json:"provider"`
		UpstreamModel string   `json:"upstream_model"`
		Usage         ir.Usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &access); err != nil {
		t.Fatalf("invalid structured access log: %v: %q", err, lines[0])
	}
	if access.RequestID != rc.ID || access.Provider != rc.ResolvedProvider || access.UpstreamModel != rc.UpstreamModel || access.Usage != usage {
		t.Fatalf("access=%+v", access)
	}
}

func TestDefaultRecorderAccountsFailureWithoutUnsignedTokenOverflow(t *testing.T) {
	rc := NewRequestContext("")
	rc.ClientModel = "recorder-error/" + rc.ID
	rc.BeginRecording(t.Context(), nil)
	rc.Complete(ir.Usage{InputTokens: -1, OutputTokens: -2}, context.Canceled)
	for _, event := range nvidia.GlobalStats.Events(200) {
		if event.Model == rc.ClientModel {
			if !event.Err || event.In != 0 || event.Out != 0 {
				t.Fatalf("failure event=%+v", event)
			}
			return
		}
	}
	t.Fatal("failed request was not accounted")
}
