package factory

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if r.Method != http.MethodPost || r.URL.Path != "/schematics" {
			http.Error(w, "unexpected request", http.StatusBadRequest)

			return
		}

		if !strings.Contains(string(body), "officialExtensions") {
			http.Error(w, "schematic not forwarded: "+string(body), http.StatusBadRequest)

			return
		}

		w.Write([]byte(`{"id":"abc123","schematic":"customization: {}\n"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}

	id, err := c.Submit(context.Background(), map[string]any{
		"customization": map[string]any{
			"systemExtensions": map[string]any{"officialExtensions": []string{"siderolabs/iscsi-tools"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if id != "abc123" {
		t.Errorf("id = %q", id)
	}
}

func TestSubmitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid schematic", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}

	_, err := c.Submit(context.Background(), map[string]any{"customization": map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "invalid schematic") {
		t.Fatalf("expected factory error, got %v", err)
	}
}
