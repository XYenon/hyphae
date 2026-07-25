package controller

import (
	"context"

	"github.com/aleksanaa/hyphae/internal/agent"
	"github.com/aleksanaa/hyphae/internal/config"
)

// AddEndpoint appends an endpoint to the config and saves it.
func (c *Controller) AddEndpoint(name, baseURL, apiKey, providerType string) error {
	c.cfg.Endpoints = append(c.cfg.Endpoints, config.Endpoint{
		Name:    name,
		BaseURL: baseURL,
		APIKey:  apiKey,
		Type:    providerType,
	})
	return c.cfg.Save()
}

// UpdateEndpoint replaces the endpoint named origName in place (keeping its
// position) with the given fields and saves. Falls back to appending when
// origName is not found.
func (c *Controller) UpdateEndpoint(origName, name, baseURL, apiKey, providerType string) error {
	for i, ep := range c.cfg.Endpoints {
		if ep.Name == origName {
			c.cfg.Endpoints[i] = config.Endpoint{Name: name, BaseURL: baseURL, APIKey: apiKey, Type: providerType}
			return c.cfg.Save()
		}
	}
	return c.AddEndpoint(name, baseURL, apiKey, providerType)
}

// RemoveEndpoint deletes an endpoint by name from the config and saves it.
func (c *Controller) RemoveEndpoint(name string) error {
	eps := c.cfg.Endpoints
	for i, ep := range eps {
		if ep.Name == name {
			c.cfg.Endpoints = append(eps[:i], eps[i+1:]...)
			break
		}
	}
	return c.cfg.Save()
}

// SwitchModel makes m the model for the active session only: it rebuilds that
// session's agent, records the model against it, updates the default for new
// sessions (config identity), and saves. Other open sessions are untouched.
// Pricing/context carried by m seed the display immediately; any gaps are filled
// from models.dev in the background.
func (c *Controller) SwitchModel(m Model) {
	c.cfg.ActiveEndpointName = m.Endpoint
	c.cfg.Model = m.ID

	c.mu.Lock()
	activeID := c.mgr.ActiveID()
	if activeID != "" {
		c.sessionModels[activeID] = m
		c.sessionAgents[activeID] = c.agentForModel(m)
	}
	c.mu.Unlock()

	if activeID != "" {
		if m.ContextWindow > 0 {
			c.emit(Event{Kind: EvContextWindow, ContextWindow: m.ContextWindow})
		}
		go c.EnrichSessionAsync(activeID)
		if c.st != nil {
			go c.st.UpdateSessionModel(activeID, m.ID, m.Endpoint)                               //nolint:errcheck
			go c.st.UpdateSessionPricing(activeID, m.ContextWindow, m.InputPrice, m.OutputPrice) //nolint:errcheck
		}
	}
	c.cfg.Save() //nolint:errcheck
}

// ListModels returns the models available at a given endpoint. Pricing is left
// zero; call EnrichPricing to fill context window and pricing from models.dev.
func (c *Controller) ListModels(ctx context.Context, ep config.Endpoint) ([]Model, error) {
	ag := agent.New(ep.ProviderType(), ep.BaseURL, ep.APIKey, "")
	raw, err := ag.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Model, len(raw))
	for i, m := range raw {
		out[i] = Model{Endpoint: ep.Name, ID: m.ID, ContextWindow: m.ContextWindow}
	}
	return out, nil
}

// EnrichPricing fills context window and pricing for models from the models.dev
// catalog (fetched once). Existing non-zero context windows are kept; pricing is
// filled where the catalog has it. Returns models for chaining.
func (c *Controller) EnrichPricing(ctx context.Context, models []Model) []Model {
	cat := FetchModelDevCatalog(ctx)
	for i := range models {
		info := cat.Lookup(models[i].ID)
		if models[i].ContextWindow <= 0 {
			models[i].ContextWindow = info.ContextWindow
		}
		if info.InputPrice > 0 {
			models[i].InputPrice = info.InputPrice
		}
		if info.OutputPrice > 0 {
			models[i].OutputPrice = info.OutputPrice
		}
	}
	return models
}

// ListSessions returns a lightweight summary of all saved sessions, across every
// work directory.
func (c *Controller) ListSessions() ([]SessionSummary, error) {
	if c.st == nil {
		return nil, nil
	}
	rows, err := c.st.ListSessions()
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, len(rows))
	for i, r := range rows {
		s := SessionSummary{
			ID:           r.ID,
			Title:        r.Title,
			UpdatedAt:    r.UpdatedAt,
			WorkDir:      r.WorkDir,
			PromptTokens: r.LastPromptTokens,
		}
		// Pull the context window the same way resume does — from the session's
		// stored record, where the models.dev-derived value was persisted. The
		// lightweight ListSessions query omits it, so read the full row per entry.
		if full, err := c.st.GetSession(r.ID); err == nil {
			s.ContextWindow = full.ContextWindow
		}
		out[i] = s
	}
	return out, nil
}

// EnrichSessionAsync fills any missing context window / pricing for one session's
// model from models.dev. It updates only that session's record — never its
// identity, and never any other session — emits EvContextWindow when the value
// becomes known and the session is still active, and persists the pricing. Runs
// synchronously; call with `go`. No-ops if the model is already fully known or
// its identity changed under us (e.g. a model switch mid-fetch).
func (c *Controller) EnrichSessionAsync(sessionID string) {
	c.mu.Lock()
	m, ok := c.sessionModels[sessionID]
	c.mu.Unlock()
	if !ok || m.ID == "" {
		return
	}
	if m.ContextWindow > 0 && m.InputPrice > 0 && m.OutputPrice > 0 {
		return
	}

	info := FetchModelDevInfo(c.ctx, m.ID)
	ctxWindow := info.ContextWindow
	if ctxWindow <= 0 && m.ContextWindow <= 0 {
		// models.dev has no entry (e.g. a local Ollama model); ask the endpoint
		// itself. No-ops (returns 0, no request) for providers that can't report it.
		ctxWindow = c.agentForModel(m).ModelContextWindow(c.ctx)
	}
	if ctxWindow <= 0 && info.InputPrice == 0 && info.OutputPrice == 0 {
		return
	}

	c.mu.Lock()
	cur, ok := c.sessionModels[sessionID]
	if !ok || cur.ID != m.ID {
		c.mu.Unlock()
		return
	}
	changed := false
	if ctxWindow > 0 && cur.ContextWindow != ctxWindow {
		cur.ContextWindow = ctxWindow
		changed = true
	}
	if info.InputPrice > 0 && cur.InputPrice != info.InputPrice {
		cur.InputPrice = info.InputPrice
		changed = true
	}
	if info.OutputPrice > 0 && cur.OutputPrice != info.OutputPrice {
		cur.OutputPrice = info.OutputPrice
		changed = true
	}
	if changed {
		c.sessionModels[sessionID] = cur
	}
	isActive := c.mgr.ActiveID() == sessionID
	c.mu.Unlock()

	if !changed {
		return
	}
	if isActive && cur.ContextWindow > 0 {
		c.emit(Event{Kind: EvContextWindow, ContextWindow: cur.ContextWindow})
	}
	if c.st != nil {
		go c.st.UpdateSessionPricing(sessionID, cur.ContextWindow, cur.InputPrice, cur.OutputPrice) //nolint:errcheck
	}
}
