package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func findDocsProvidersPath(t *testing.T) string {
	t.Helper()
	// Check typical relative path from internal/gateway/provider
	candidates := []string{
		filepath.Join("..", "..", "..", "docs", "providers.md"),
		filepath.Join("docs", "providers.md"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// Walk upwards if not found in candidate paths
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current working directory: %v", err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "docs", "providers.md")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find docs/providers.md")
	return ""
}

type docCodeBlock struct {
	Language string
	Content  string
	Line     int
}

func extractFencedCodeBlocks(markdown string) []docCodeBlock {
	re := regexp.MustCompile("(?m)^```(json|yaml)\\s*\\n([\\s\\S]*?)\\n```")
	matches := re.FindAllStringSubmatchIndex(markdown, -1)
	var blocks []docCodeBlock
	for _, m := range matches {
		lang := markdown[m[2]:m[3]]
		content := strings.TrimSpace(markdown[m[4]:m[5]])
		line := strings.Count(markdown[:m[0]], "\n") + 1
		blocks = append(blocks, docCodeBlock{
			Language: lang,
			Content:  content,
			Line:     line,
		})
	}
	return blocks
}

func TestDocsProvidersExamplesValidate(t *testing.T) {
	docPath := findDocsProvidersPath(t)
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}

	blocks := extractFencedCodeBlocks(string(raw))
	if len(blocks) == 0 {
		t.Fatalf("no code blocks found in %s", docPath)
	}

	validatedProviders := 0
	validatedDocuments := 0

	for _, block := range blocks {
		t.Run("line_"+strings.TrimSpace(block.Language), func(t *testing.T) {
			content := block.Content

			if block.Language == "json" {
				// Snippet might be a Document (schema_version or providers array), a Provider, or a partial fragment.
				if strings.Contains(content, `"schema_version"`) || strings.Contains(content, `"providers_v2"`) || strings.Contains(content, `"providers"`) {
					// Validate as Document
					doc, err := DecodeDocument([]byte(content))
					if err != nil {
						t.Fatalf("line %d: DecodeDocument failed: %v\nContent:\n%s", block.Line, err, content)
					}
					if len(doc.Providers) == 0 {
						// Valid empty catalog skeleton
						validatedDocuments++
						return
					}
					for i, p := range doc.Providers {
						if err := p.Validate(); err != nil {
							t.Fatalf("line %d: Provider[%d] (%s) failed validation: %v", block.Line, i, p.ID, err)
						}
					}
					validatedDocuments++
					return
				}

				if strings.Contains(content, `"id"`) && strings.Contains(content, `"base_url"`) && strings.Contains(content, `"protocol"`) {
					// Validate as single Provider
					var p Provider
					if err := json.Unmarshal([]byte(content), &p); err != nil {
						t.Fatalf("line %d: json.Unmarshal Provider failed: %v\nContent:\n%s", block.Line, err, content)
					}
					if err := p.Validate(); err != nil {
						t.Fatalf("line %d: Provider (%s) failed validation: %v\nContent:\n%s", block.Line, p.ID, err, content)
					}
					validatedProviders++
					return
				}
			}

			if block.Language == "yaml" {
				if strings.Contains(content, "id:") && strings.Contains(content, "base_url:") && strings.Contains(content, "protocol:") {
					var rawYAML any
					if err := yaml.Unmarshal([]byte(content), &rawYAML); err != nil {
						t.Fatalf("line %d: yaml.Unmarshal Provider failed: %v\nContent:\n%s", block.Line, err, content)
					}
					jsonBytes, err := json.Marshal(rawYAML)
					if err != nil {
						t.Fatalf("line %d: json.Marshal from YAML failed: %v\nContent:\n%s", block.Line, err, content)
					}
					var p Provider
					if err := json.Unmarshal(jsonBytes, &p); err != nil {
						t.Fatalf("line %d: json.Unmarshal Provider from YAML failed: %v\nContent:\n%s", block.Line, err, content)
					}
					if err := p.Validate(); err != nil {
						t.Fatalf("line %d: Provider (%s) failed validation: %v\nContent:\n%s", block.Line, p.ID, err, content)
					}
					validatedProviders++
					return
				}
			}
		})
	}

	if validatedProviders < 5 {
		t.Errorf("expected at least 5 individual provider examples validated, got %d", validatedProviders)
	}
	if validatedDocuments < 2 {
		t.Errorf("expected at least 2 catalog document examples validated, got %d", validatedDocuments)
	}
	t.Logf("Successfully validated %d standalone provider examples and %d full document examples from %s",
		validatedProviders, validatedDocuments, docPath)
}
