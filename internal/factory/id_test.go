package factory

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Test vectors from github.com/siderolabs/image-factory pkg/schematic/schematic_test.go (v1.8.0):
// they pin the conversion from the tanvil.yaml map to the factory's canonical form.
func TestLocalIDMatchesFactory(t *testing.T) {
	for name, tc := range map[string]struct{ schematic, id string }{
		"empty":                  {`{}`, "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"},
		"empty customization":    {`customization: {}`, "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"},
		"empty kernel args":      {`customization: {"extraKernelArgs": []}`, "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"},
		"kernel args":            {`{"customization": {"extraKernelArgs": ["noapic", "nolapic"]}}`, "9cba8e32753f91a16c1837ab8abf356af021706ef284aef07380780177d9a06c"},
		"kernel args, empty ext": {`{"customization": {"extraKernelArgs": ["noapic", "nolapic"], "systemExtensions": {}}}`, "9cba8e32753f91a16c1837ab8abf356af021706ef284aef07380780177d9a06c"},
		"meta":                   {`{"customization": {"meta": [{"key": 10, "value": "foo"}], "extraKernelArgs": [], "systemExtensions": {}}}`, "d308a2a5ee2277bed5fbaa104fcbc8d59122abfa737df987a95b4ca763459a7f"},
		"overlay":                {`{"overlay": {"name": "rpi_generic", "image": "siderolabs/sbc-raspberrypi"}}`, "ee21ef4a5ef808a9b7484cc0dda0f25075021691c8c09a276591eedb638ea1f9"},
		"overlay, empty custom":  {`{"overlay": {"name": "rpi_generic", "image": "siderolabs/sbc-raspberrypi"},"customization":{}}`, "ee21ef4a5ef808a9b7484cc0dda0f25075021691c8c09a276591eedb638ea1f9"},
		"secureboot":             {`{"customization":{"secureboot": {"includeWellKnownCertificates": true}}}`, "fa8e05f142a851d3ee568eb0a8e5841eaf6b0ebc8df9a63df16ac5ed2c04f3e6"},
		"4k sector":              {`{"customization":{"diskImage": {"sectorSize": 4096}}}`, "92833e9ddd9bb9e11b2464fa5525429f01866306d52ebd882ee08d7918f6d1ea"},
	} {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			if err := yaml.Unmarshal([]byte(tc.schematic), &m); err != nil {
				t.Fatal(err)
			}

			id, err := LocalID(m)
			if err != nil {
				t.Fatal(err)
			}

			if id != tc.id {
				t.Errorf("id = %s, want %s", id, tc.id)
			}
		})
	}
}

func TestLocalIDRejectsUnknownFields(t *testing.T) {
	_, err := LocalID(map[string]any{"customization": map[string]any{"systemExtension": map[string]any{}}})
	if err == nil || !strings.Contains(err.Error(), "systemExtension") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}
