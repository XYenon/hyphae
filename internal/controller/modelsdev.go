package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const modelsDevURL = "https://models.dev/api.json"

type modelsDevCatalog map[string]modelsDevProvider

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Limit struct {
		Context int64 `json:"context"`
	} `json:"limit"`
	Cost *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
}

// ModelDevInfo holds the data fetched from models.dev for a given model.
type ModelDevInfo struct {
	ContextWindow int64
	InputPrice    float64 // USD per million input tokens; 0 if unknown
	OutputPrice   float64 // USD per million output tokens; 0 if unknown
}

// ModelDevCatalog is a fetched models.dev catalog that supports repeated lookups
// without refetching. Use it when enriching a whole list of models at once.
type ModelDevCatalog struct {
	catalog modelsDevCatalog
}

var (
	catalogMu     sync.Mutex
	cachedCatalog *ModelDevCatalog
)

// FetchModelDevCatalog returns the models.dev catalog, fetching it once per process
// and caching the result. The catalog is effectively static (pricing and context
// windows change rarely), so every model-picker open after the first reuses the
// cached copy instead of re-downloading it. Failures are not cached, so a fetch
// that failed while offline is retried on the next call. Returns nil on error;
// Lookup on a nil catalog safely yields zero values.
func FetchModelDevCatalog(ctx context.Context) *ModelDevCatalog {
	catalogMu.Lock()
	cached := cachedCatalog
	catalogMu.Unlock()
	if cached != nil {
		return cached
	}

	// Fetch outside the lock (network I/O); a cold-cache race just fetches twice
	// and keeps the last result, which is harmless.
	cat := fetchModelDevCatalog(ctx)
	if cat == nil {
		return nil
	}
	catalogMu.Lock()
	cachedCatalog = cat
	catalogMu.Unlock()
	return cat
}

// fetchModelDevCatalog performs the uncached HTTP fetch of the models.dev catalog.
func fetchModelDevCatalog(ctx context.Context) *ModelDevCatalog {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "hyphae")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var catalog modelsDevCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil
	}
	return &ModelDevCatalog{catalog: catalog}
}

// Lookup returns context window and pricing for the given model ID, or zero
// values when the catalog is nil or the model is not found.
func (c *ModelDevCatalog) Lookup(modelID string) ModelDevInfo {
	if c == nil {
		return ModelDevInfo{}
	}
	return lookupModelDevInfo(c.catalog, modelID)
}

// FetchModelDevInfo fetches the models.dev catalog and returns context window
// and pricing for the given model ID. Returns zero values on error or not found.
func FetchModelDevInfo(ctx context.Context, modelID string) ModelDevInfo {
	return FetchModelDevCatalog(ctx).Lookup(modelID)
}

func lookupModelDevInfo(catalog modelsDevCatalog, modelID string) ModelDevInfo {
	id := strings.ToLower(modelID)

	if info, ok := findInCatalog(catalog, id); ok {
		return info
	}

	// Strip provider prefix (e.g. "anthropic/claude-opus-4" → "claude-opus-4")
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		if info, ok := findInCatalog(catalog, id[idx+1:]); ok {
			return info
		}
	}

	return ModelDevInfo{}
}

func findInCatalog(catalog modelsDevCatalog, id string) (ModelDevInfo, bool) {
	for _, p := range catalog {
		for name, m := range p.Models {
			if strings.ToLower(name) != id {
				continue
			}
			info := ModelDevInfo{ContextWindow: m.Limit.Context}
			if m.Cost != nil {
				info.InputPrice = m.Cost.Input
				info.OutputPrice = m.Cost.Output
			}
			return info, true
		}
	}
	return ModelDevInfo{}, false
}
