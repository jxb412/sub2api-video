package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"log/slog"
)

// ModelCatalogEntry describes one model that can be advertised without shipping
// a new image. It mirrors the fields the /v1/models response and the pricing
// table need, so a JSON file hosted anywhere can extend the built-in catalogue.
type ModelCatalogEntry struct {
	ID          string `json:"id"`
	Platform    string `json:"platform,omitempty"`
	OwnedBy     string `json:"owned_by,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Mode        string `json:"mode,omitempty"` // chat / image_generation / video_generation
	Created     int64  `json:"created,omitempty"`
}

// modelCatalogFile is the on-disk document used both as the cached copy of the
// remote catalogue and as the operator-editable override file.
type modelCatalogFile struct {
	Version     int                 `json:"version"`
	SyncedAt    string              `json:"synced_at,omitempty"`
	Models      []ModelCatalogEntry `json:"models"`
	VideoModels []ModelCatalogEntry `json:"video_models,omitempty"`
}

// ModelCatalog is a hot-reloadable model list.
//
// Why it exists: every new model used to require rebuilding the image because
// the catalogue was a compiled-in slice. The catalogue now prefers, in order:
//
//	1. the last successfully fetched remote document (cached on disk),
//	2. the on-disk override file,
//	3. the compiled-in defaults.
//
// A background refresher polls the configured URL, so publishing a model is a
// JSON edit on the hosting side instead of a release.
type ModelCatalog struct {
	url      string
	path     string
	interval time.Duration
	client   *http.Client

	mu      sync.RWMutex
	entries map[string]ModelCatalogEntry // keyed by lowercased model id
	synced  time.Time
	lastErr string
}

const defaultModelCatalogInterval = 6 * time.Hour

// NewModelCatalog builds a catalogue bound to a remote URL and a cache path.
// Passing an empty URL disables networking; the cache file still applies.
func NewModelCatalog(url, cachePath string, interval time.Duration) *ModelCatalog {
	if interval <= 0 {
		interval = defaultModelCatalogInterval
	}
	c := &ModelCatalog{
		url:      strings.TrimSpace(url),
		path:     strings.TrimSpace(cachePath),
		interval: interval,
		client:   &http.Client{Timeout: 30 * time.Second},
		entries:  make(map[string]ModelCatalogEntry),
	}
	c.loadFromDisk()
	return c
}

// Start refreshes immediately and then on every interval until ctx is done.
func (c *ModelCatalog) Start(ctx context.Context) {
	if c == nil {
		return
	}
	if c.url != "" {
		if err := c.Refresh(ctx); err != nil {
			slog.Warn("model_catalog.initial_refresh_failed", "url", c.url, "error", err)
		}
		go func() {
			ticker := time.NewTicker(c.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := c.Refresh(context.WithoutCancel(ctx)); err != nil {
						slog.Warn("model_catalog.refresh_failed", "url", c.url, "error", err)
					}
				}
			}
		}()
	}
}

// Refresh fetches and applies the remote document.
func (c *ModelCatalog) Refresh(ctx context.Context) error {
	if c == nil || c.url == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("catalog http %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var doc modelCatalogFile
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("catalog decode: %w", err)
	}
	c.apply(doc)
	c.mu.Lock()
	c.synced = time.Now().UTC()
	c.lastErr = ""
	c.mu.Unlock()
	c.persist(payload)
	slog.Info("model_catalog.refreshed", "models", len(c.entries), "url", c.url)
	return nil
}

// IDsForPlatform returns catalogue ids for a platform ("" means every platform).
func (c *ModelCatalog) IDsForPlatform(platform string) []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	platform = strings.ToLower(strings.TrimSpace(platform))
	out := make([]string, 0, len(c.entries))
	for _, entry := range c.entries {
		if platform != "" && strings.ToLower(entry.Platform) != "" && strings.ToLower(entry.Platform) != platform {
			continue
		}
		out = append(out, entry.ID)
	}
	sort.Strings(out)
	return out
}

// EntriesForMode returns entries filtered by mode (chat / image_generation /
// video_generation); an empty mode means "any".
func (c *ModelCatalog) EntriesForMode(mode string) []ModelCatalogEntry {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	mode = strings.ToLower(strings.TrimSpace(mode))
	out := make([]ModelCatalogEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		if mode != "" && strings.ToLower(entry.Mode) != mode {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *ModelCatalog) apply(doc modelCatalogFile) {
	entries := make(map[string]ModelCatalogEntry, len(doc.Models)+len(doc.VideoModels))
	add := func(list []ModelCatalogEntry) {
		for _, entry := range list {
			id := strings.TrimSpace(entry.ID)
			if id == "" {
				continue
			}
			entry.ID = id
			entries[strings.ToLower(id)] = entry
		}
	}
	add(doc.Models)
	add(doc.VideoModels)
	c.mu.Lock()
	c.entries = entries
	c.mu.Unlock()
}

func (c *ModelCatalog) loadFromDisk() {
	if c == nil || c.path == "" {
		return
	}
	payload, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var doc modelCatalogFile
	if err := json.Unmarshal(payload, &doc); err != nil {
		return
	}
	c.apply(doc)
	slog.Info("model_catalog.loaded_from_cache", "models", len(c.entries), "path", c.path)
}

func (c *ModelCatalog) persist(payload []byte) {
	if c == nil || c.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}

// ModelCatalogIntervalFromEnv parses MODEL_CATALOG_INTERVAL ("6h", "30m", "600s").
// Invalid or empty values fall back to the default so a typo never disables the
// refresher silently for long.
func ModelCatalogIntervalFromEnv(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultModelCatalogInterval
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := time.ParseDuration(raw + "s"); err == nil && seconds > 0 {
		return seconds
	}
	slog.Warn("model_catalog.invalid_interval", "value", raw)
	return defaultModelCatalogInterval
}
