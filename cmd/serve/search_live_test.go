package main

// Live smoke test: runs the real gateway pipeline against live backends on
// port 8081 and reports intent, backend chain, adapted queries, statuses,
// result counts, top-5 results, and fallbacks — the spec's smoke-report items.
//
// Run with:  go test ./cmd/serve/ -run TestLiveSearchSmoke -v -count=1
// It is skipped unless the gateway is actually listening on :8081, so the
// default suite never depends on the network.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLiveSearchSmoke(t *testing.T) {
	base := "http://localhost:8081"
	if _, err := http.Post(base+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Skipf("gateway not running on %s (%v) — start it with the :8081 smoke server first", base, err)
	}

	queries := []string{
		"arxiv 2608.14210",
		"CISG article 79 exemption case law",
		"UNCLOS article 76 continental shelf",
		"document level legal machine translation",
	}

	for _, q := range queries {
		t.Run(fmt.Sprintf("query=%q", q), func(t *testing.T) {
			intent := deriveSearchContext(q)
			t.Logf("intent: type=%s instrument=%q provision=%q arxiv=%q doi=%q",
				intent.Type, intent.Instrument, intent.Provision, intent.ArxivID, intent.DOI)
			t.Logf("keywords=%v phrases=%v", intent.Keywords, intent.Phrases)

			chain := searchGatewayInstance.routeBackends(intent)
			t.Logf("backend chain: %s", strings.Join(chain, " -> "))
			for _, name := range chain {
				b, ok := searchGatewayInstance.backends[name]
				if !ok {
					continue
				}
				plan := b.Adapt(intent)
				t.Logf("  plan[%s]: identifier=%q max=%d queries=%v", name, plan.Identifier, plan.MaxResults, plan.Queries)
			}

			body, err := callSearchMCP(base, q)
			if err != nil {
				t.Fatalf("mcp call failed: %v", err)
			}
			lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
			shown := 0
			for _, line := range lines {
				if line == "" {
					continue
				}
				shown++
				if shown <= 5 {
					t.Logf("  %s", line)
				}
			}
			t.Logf("mcp returned %d result line(s)", shown)
		})
	}
}

func callSearchMCP(base, query string) (string, error) {
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "search_web", "arguments": map[string]any{"query": query}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reqReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", bytes.NewReader(req))
	if err != nil {
		return "", err
	}
	reqReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(reqReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("bad response %q: %w", string(raw), err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("rpc error: %s", out.Error.Message)
	}
	if out.Result.IsError {
		return "", fmt.Errorf("tool error: %s", out.Result.Content[0].Text)
	}
	if len(out.Result.Content) == 0 {
		return "", fmt.Errorf("empty content")
	}
	return out.Result.Content[0].Text, nil
}
