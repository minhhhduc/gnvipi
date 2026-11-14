package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	modelsPagePath        = "/models?pageSize=200&filters=nimType%3Anim_type_preview%2CnimType%3Anim_type_upgrade_available"
	discoveryConcurrency  = 8
	discoveryPageMaxBytes = 2 << 20
)

var (
	modelLinkRE  = regexp.MustCompile(`href="/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)"`)
	functionIDRE = regexp.MustCompile(`\\"nvcfFunctionId\\":\\"([a-f0-9-]{36})\\"`)
	namespaceRE  = regexp.MustCompile(`\\"namespace\\":\\"([0-9a-z]+)\\"`)
)

// DiscoveryError reports an incomplete upstream catalog read. The previous
// catalog must stay untouched whenever it is returned.
type DiscoveryError struct {
	Op  string
	Err error
}

func (e *DiscoveryError) Error() string {
	return "nvidia catalog discovery: " + e.Op + ": " + e.Err.Error()
}

func (e *DiscoveryError) Unwrap() error { return e.Err }

// CatalogDiscoverer reads the live Playground catalog. BaseURL exists so tests
// can point it at a local server; production uses the zero value.
type CatalogDiscoverer struct {
	Client  *http.Client
	BaseURL string
}

// DiscoverCatalog fetches the live catalog with the production settings.
func DiscoverCatalog(ctx context.Context) ([]PlaygroundGroup, error) {
	return CatalogDiscoverer{}.Discover(ctx)
}

func (d CatalogDiscoverer) Discover(ctx context.Context) ([]PlaygroundGroup, error) {
	links, err := d.modelLinks(ctx)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, &DiscoveryError{Op: "models page", Err: errors.New("no model links found")}
	}

	rows := make([]*PlaygroundRow, len(links))
	errs := make([]error, len(links))
	sem := make(chan struct{}, discoveryConcurrency)
	var wg sync.WaitGroup
	for i, link := range links {
		wg.Add(1)
		go func(i int, link modelLink) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row, found, probeErr := d.probe(ctx, link.publisher, link.slug)
			if probeErr != nil {
				errs[i] = probeErr
				return
			}
			if found {
				rows[i] = &row
			}
		}(i, link)
	}
	wg.Wait()
	for _, probeErr := range errs {
		if probeErr != nil {
			return nil, probeErr
		}
	}

	byPublisher := make(map[string][]PlaygroundRow)
	for _, row := range rows {
		if row == nil {
			continue
		}
		publisher, _, _ := strings.Cut(row.Model, "/")
		byPublisher[publisher] = append(byPublisher[publisher], *row)
	}
	groups := make([]PlaygroundGroup, 0, len(byPublisher))
	for publisher, entries := range byPublisher {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Model < entries[j].Model })
		groups = append(groups, PlaygroundGroup{Publisher: publisher, Models: entries})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Publisher < groups[j].Publisher })
	if len(groups) == 0 {
		return nil, &DiscoveryError{Op: "playground pages", Err: errors.New("no playground models found")}
	}
	return groups, nil
}

type modelLink struct{ publisher, slug string }

func (d CatalogDiscoverer) modelLinks(ctx context.Context) ([]modelLink, error) {
	body, err := d.fetch(ctx, d.baseURL()+modelsPagePath)
	if err != nil {
		return nil, &DiscoveryError{Op: "models page", Err: err}
	}
	seen := make(map[modelLink]bool)
	var links []modelLink
	for _, match := range modelLinkRE.FindAllSubmatch(body, -1) {
		link := modelLink{publisher: string(match[1]), slug: string(match[2])}
		if skipModelLink(link) || seen[link] {
			continue
		}
		seen[link] = true
		links = append(links, link)
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].publisher != links[j].publisher {
			return links[i].publisher < links[j].publisher
		}
		return links[i].slug < links[j].slug
	})
	return links, nil
}

// skipModelLink filters navigation and asset links that share the
// "/publisher/slug" shape on the models page.
func skipModelLink(link modelLink) bool {
	switch link.publisher {
	case "", "models", "explore", "blueprints", "skills", "_next", "assets", "static":
		return true
	}
	switch link.slug {
	case "", "playground", "community":
		return true
	}
	return false
}

func (d CatalogDiscoverer) probe(ctx context.Context, publisher, slug string) (PlaygroundRow, bool, error) {
	pageURL := d.baseURL() + "/" + publisher + "/" + slug + "/playground"
	body, err := d.fetch(ctx, pageURL)
	if err != nil {
		return PlaygroundRow{}, false, &DiscoveryError{Op: publisher + "/" + slug, Err: err}
	}
	functionID := functionIDRE.FindSubmatch(body)
	if functionID == nil {
		// The models page lists inference-only entries (embed, rerank, parse)
		// that have no playground. They are not part of the callable catalog.
		return PlaygroundRow{}, false, nil
	}
	namespace := Namespace
	if match := namespaceRE.FindSubmatch(body); match != nil {
		namespace = string(match[1])
	}
	return PlaygroundRow{
		Model:      publisher + "/" + slug,
		Slug:       slug,
		Namespace:  namespace,
		FunctionID: string(functionID[1]),
	}, true, nil
}

func (d CatalogDiscoverer) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoveryPageMaxBytes))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (d CatalogDiscoverer) baseURL() string {
	if d.BaseURL != "" {
		return strings.TrimRight(d.BaseURL, "/")
	}
	return "https://build.nvidia.com"
}
