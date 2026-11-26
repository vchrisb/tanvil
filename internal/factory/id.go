package factory

import (
	"fmt"

	"github.com/siderolabs/image-factory/pkg/schematic"
	"sigs.k8s.io/yaml"
)

// ParseSchematic strictly decodes a schematic the way the Image Factory does, rejecting unknown
// fields.
func ParseSchematic(s map[string]any) (*schematic.Schematic, error) {
	data, err := yaml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encoding schematic: %w", err)
	}

	parsed, err := schematic.Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("invalid schematic: %w", err)
	}

	return parsed, nil
}

// LocalID computes the content-addressed schematic ID offline, with the Image Factory's own code.
func LocalID(s map[string]any) (string, error) {
	parsed, err := ParseSchematic(s)
	if err != nil {
		return "", err
	}

	return parsed.ID()
}
