// Package factory computes Talos Image Factory schematic IDs and registers schematics.
package factory

import (
	"context"
	"fmt"
	"net/http"
	"time"

	factoryclient "github.com/siderolabs/image-factory/pkg/client"
)

// Client registers schematics with an Image Factory.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// New returns a client for the Image Factory at host, e.g. factory.talos.dev.
func New(host string) *Client {
	return &Client{BaseURL: "https://" + host, HTTPClient: &http.Client{Timeout: 10 * time.Second}}
}

// Submit registers the schematic with the factory and returns the ID the factory assigned. The
// factory is idempotent: the same schematic always yields the same ID, the one LocalID computes.
// A factory only serves images for schematics it knows, so a new schematic must be submitted once.
func (c *Client) Submit(ctx context.Context, s map[string]any) (string, error) {
	parsed, err := ParseSchematic(s)
	if err != nil {
		return "", err
	}

	client, err := factoryclient.New(c.BaseURL, factoryclient.WithClient(*c.HTTPClient))
	if err != nil {
		return "", err
	}

	id, _, err := client.SchematicCreate(ctx, *parsed)
	if err != nil {
		return "", fmt.Errorf("submitting schematic to %s: %w", c.BaseURL, err)
	}

	return id, nil
}
