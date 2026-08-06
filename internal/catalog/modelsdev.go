package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// maxCatalogBytes caps a catalog download. models.dev's api.json is roughly
// 3.5 MB today; this leaves generous headroom while refusing to buffer an
// unbounded response.
const maxCatalogBytes = 32 << 20

// fetchModelsDev downloads and parses the models.dev catalog.
//
// The endpoint is public, so no credentials are sent.
func fetchModelsDev(ctx context.Context, opts Options) (map[string]Provider, error) {
	body, err := getJSON(ctx, opts.HTTPClient, opts.ModelsDevURL)
	if err != nil {
		return nil, fmt.Errorf("fetching models.dev: %w", err)
	}
	providers, err := parseModelsDev(body)
	if err != nil {
		return nil, err
	}
	opts.Logger.Debug("catalog: loaded models.dev", "providers", len(providers))
	return providers, nil
}

// parseModelsDev decodes the models.dev shape: a top-level object keyed by
// provider ID.
func parseModelsDev(body []byte) (map[string]Provider, error) {
	var raw map[string]Provider
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing the models.dev catalog: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("the models.dev catalog is empty")
	}

	// The model map is keyed by ID, but entries also carry an id field. Trust
	// the key, since that is what a caller will look up by.
	for pid, p := range raw {
		if p.ID == "" {
			p.ID = pid
		}
		for mid, m := range p.Models {
			m.ID = mid
			m.ProviderID = pid
			p.Models[mid] = m
		}
		raw[pid] = p
	}
	return raw, nil
}

// getJSON performs a plain unauthenticated GET with a size cap.
func getJSON(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %d %s", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes))
}
