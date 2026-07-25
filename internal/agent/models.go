package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ModelInfo holds a model ID and its context window size (0 if unknown).
type ModelInfo struct {
	ID            string
	ContextWindow int64
}

// ListModels returns the models available at the agent's endpoint. goai has no
// model-listing API, so this calls the provider's native list endpoint directly.
// ContextWindow is populated when the provider reports it; otherwise it is 0 and
// the caller fills it from the models.dev catalog.
func (a *Agent) ListModels(ctx context.Context) ([]ModelInfo, error) {
	switch a.providerType {
	case "anthropic":
		return a.listAnthropicModels(ctx)
	case "google":
		return a.listGoogleModels(ctx)
	case "ollama":
		return a.listOllamaModels(ctx)
	default: // "openai" or ""
		return a.listOpenAIModels(ctx)
	}
}

// httpGetJSON performs a GET and decodes the JSON body into out.
func httpGetJSON(ctx context.Context, urlStr string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("models list: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// httpPostJSON performs a POST with a JSON body and decodes the JSON response.
func httpPostJSON(ctx context.Context, urlStr string, reqBody, out any) error {
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// listOpenAIModels queries the OpenAI-compatible GET {baseURL}/models endpoint.
// context_length is an OpenAI-compatible extension (OpenRouter and others).
func (a *Agent) listOpenAIModels(ctx context.Context) ([]ModelInfo, error) {
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	headers := map[string]string{}
	if a.apiKey != "" {
		headers["Authorization"] = "Bearer " + a.apiKey
	}
	if err := httpGetJSON(ctx, strings.TrimRight(a.baseURL, "/")+"/models", headers, &body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, len(body.Data))
	for i, m := range body.Data {
		out[i] = ModelInfo{ID: m.ID, ContextWindow: m.ContextLength}
	}
	return out, nil
}

// listAnthropicModels queries the Anthropic GET /v1/models endpoint. Anthropic
// does not report a context window, so ContextWindow is left 0 (filled later
// from the models.dev catalog).
func (a *Agent) listAnthropicModels(ctx context.Context) ([]ModelInfo, error) {
	base := a.baseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	headers := map[string]string{
		"x-api-key":         a.apiKey,
		"anthropic-version": "2023-06-01",
	}
	if err := httpGetJSON(ctx, strings.TrimRight(base, "/")+"/v1/models", headers, &body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, len(body.Data))
	for i, m := range body.Data {
		out[i] = ModelInfo{ID: m.ID}
	}
	return out, nil
}

// listOllamaModels queries the Ollama GET /api/tags endpoint, which lists the
// models pulled on the local server, then fills each model's context window from
// POST /api/show (tags does not report it). A show failure leaves ContextWindow 0
// (filled later from the models.dev catalog).
func (a *Agent) listOllamaModels(ctx context.Context) ([]ModelInfo, error) {
	base := strings.TrimRight(a.baseURL, "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := httpGetJSON(ctx, base+"/api/tags", nil, &body); err != nil {
		return nil, err
	}
	// Each model's context window needs its own /api/show call; fetch them
	// concurrently (bounded) so a long model list does not serialize into one slow
	// request after another. Each goroutine writes its own slice slot.
	out := make([]ModelInfo, len(body.Models))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, m := range body.Models {
		out[i].ID = m.Name
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i].ContextWindow = ollamaContextWindow(ctx, base, name)
		}(i, m.Name)
	}
	wg.Wait()
	return out, nil
}

// ModelContextWindow returns the context window for the agent's model by asking
// the provider directly, for providers whose models are absent from the
// models.dev catalog. Only Ollama (via /api/show) is supported; others return 0.
func (a *Agent) ModelContextWindow(ctx context.Context) int64 {
	if a.providerType != "ollama" {
		return 0
	}
	base := strings.TrimRight(a.baseURL, "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	return ollamaContextWindow(ctx, base, a.modelID)
}

// ollamaContextWindow reads a model's context length from POST /api/show. Ollama
// reports it under a per-architecture key ("<arch>.context_length") in model_info;
// the value is returned, or 0 if the request fails or the key is absent.
func ollamaContextWindow(ctx context.Context, base, model string) int64 {
	var body struct {
		ModelInfo map[string]json.RawMessage `json:"model_info"`
	}
	if err := httpPostJSON(ctx, base+"/api/show", map[string]any{"model": model}, &body); err != nil {
		return 0
	}
	for k, v := range body.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			var n int64
			if json.Unmarshal(v, &n) == nil {
				return n
			}
		}
	}
	return 0
}

// listGoogleModels queries the Gemini GET /v1beta/models endpoint. Model names
// come back as "models/<id>"; the prefix is stripped. inputTokenLimit is the
// context window.
func (a *Agent) listGoogleModels(ctx context.Context) ([]ModelInfo, error) {
	base := a.baseURL
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	var body struct {
		Models []struct {
			Name            string `json:"name"`
			InputTokenLimit int64  `json:"inputTokenLimit"`
		} `json:"models"`
	}
	urlStr := strings.TrimRight(base, "/") + "/v1beta/models?key=" + url.QueryEscape(a.apiKey)
	if err := httpGetJSON(ctx, urlStr, nil, &body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, len(body.Models))
	for i, m := range body.Models {
		out[i] = ModelInfo{ID: strings.TrimPrefix(m.Name, "models/"), ContextWindow: m.InputTokenLimit}
	}
	return out, nil
}
