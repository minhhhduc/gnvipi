// probehard scans the embedded playground_models.json registry and reports
// which models no longer serve a usable /playground page. Output is a JSON
// array on stdout — copy straight into the "deleted_id" field of
// playground_models.json, or pipe through `jq` for a quick diff.
//
//	go run ./cmd/probehard [-concurrency=16] [-timeout=8s] [-include-active]
//
// -include-active also prints the models that ARE still alive (one
// "alive_id" line per id) so you can see coverage at a glance.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/models"
)

type probe struct {
	model string
	url   string
}

type probeResult struct {
	model string
	alive bool
}

func main() {
	concurrency := flag.Int("concurrency", 16, "parallel HEAD probes")
	timeout := flag.Duration("timeout", 8*time.Second, "per-probe timeout")
	includeActive := flag.Bool("include-active", false, "also print alive model ids (one per line, prefixed alive_id:)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	probes := make([]probe, 0, 200)
	for _, g := range models.AllGroups() {
		for _, m := range g.Models {
			if m.Slug == "" {
				continue
			}
			probes = append(probes, probe{
				model: m.Model,
				url:   "https://build.nvidia.com/" + g.Publisher + "/" + m.Slug + "/playground",
			})
		}
	}
	if len(probes) == 0 {
		log.Fatal("no models in registry")
	}
	fmt.Fprintf(os.Stderr, "probing %d models with concurrency=%d timeout=%s\n",
		len(probes), *concurrency, *timeout)

	results := make([]probeResult, len(probes))
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	for i, p := range probes {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p probe) {
			defer wg.Done()
			defer func() { <-sem }()
			probeCtx, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			results[i] = probeResult{model: p.model, alive: captcha.ProbePlaygroundAlive(probeCtx, p.url)}
		}(i, p)
	}
	wg.Wait()

	var dead, alive []string
	for _, r := range results {
		if r.alive {
			alive = append(alive, r.model)
		} else {
			dead = append(dead, r.model)
		}
	}
	if *includeActive {
		for _, id := range alive {
			fmt.Printf("alive_id: %s\n", id)
		}
	}
	quoted := make([]string, len(dead))
	for i, id := range dead {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	fmt.Printf("[\n  %s\n]\n", strings.Join(quoted, ",\n  "))
	fmt.Fprintf(os.Stderr, "alive=%d retired=%d total=%d\n", len(alive), len(dead), len(probes))
}
