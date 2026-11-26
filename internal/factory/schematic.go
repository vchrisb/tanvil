// Package factory talks to the Talos Image Factory.
package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"sigs.k8s.io/yaml"
)

// DefaultURL is the public Talos Image Factory.
const DefaultURL = "https://factory.talos.dev"

// Client resolves schematics to their content-addressed IDs.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// New returns a client for the public Image Factory.
func New() *Client {
	return &Client{BaseURL: DefaultURL, HTTPClient: &http.Client{Timeout: 30 * time.Second}}
}

// SchematicID uploads the schematic and returns its ID. The factory is idempotent:
// the same schematic always yields the same ID.
func (c *Client) SchematicID(ctx context.Context, schematic map[string]any) (string, error) {
	body, err := yaml.Marshal(schematic)
	if err != nil {
		return "", fmt.Errorf("encoding schematic: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/schematics", bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/yaml")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("posting schematic to %s: %w", c.BaseURL, err)
	}

	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("image factory returned %s: %s", resp.Status, bytes.TrimSpace(respBody))
	}

	var result struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decoding image factory response: %w", err)
	}

	if result.ID == "" {
		return "", fmt.Errorf("image factory response has no schematic id")
	}

	return result.ID, nil
}
