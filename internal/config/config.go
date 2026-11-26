// Package config loads and validates the tanvil.yaml cluster definition.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"

	"github.com/blang/semver/v4"
	"sigs.k8s.io/yaml"

	"github.com/vchrisb/tanvil/internal/factory"
)

const (
	// APIVersion is the only supported apiVersion of the cluster definition.
	APIVersion = "tanvil.dev/v1alpha1"
	// Kind is the only supported kind of the cluster definition.
	Kind = "TalosCluster"

	// RoleControlPlane marks a control plane node.
	RoleControlPlane = "controlplane"
	// RoleWorker marks a worker node.
	RoleWorker = "worker"
)

// MinTalosVersion is the oldest Talos release this tool supports.
var MinTalosVersion = semver.MustParse("1.14.0")

// TalosCluster is the root object of tanvil.yaml.
type TalosCluster struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       Spec       `json:"spec"`

	// dir is the directory of the loaded file; relative paths resolve against it.
	dir string
}

// ObjectMeta holds the cluster identity.
type ObjectMeta struct {
	Name string `json:"name"`
}

// Spec describes the desired cluster.
type Spec struct {
	// Endpoint is the Kubernetes API host name; the URL is https://<endpoint>:6443.
	Endpoint      string        `json:"endpoint"`
	SecretsFile   string        `json:"secretsFile"`
	Versions      Versions      `json:"versions"`
	Image         Image         `json:"image"`
	Install       Install       `json:"install"`
	ConfigPatches ConfigPatches `json:"configPatches,omitempty"`
	Network       Network       `json:"network"`
	Nodes         []Node        `json:"nodes"`
}

// Versions pins Talos and Kubernetes.
type Versions struct {
	// Talos is the Talos release (e.g. v1.14.0); it is also the machine config contract.
	Talos string `json:"talos"`
	// Kubernetes is the Kubernetes release without a leading "v" (e.g. 1.35.0).
	Kubernetes string `json:"kubernetes"`
}

// DefaultFactory is the public Talos Image Factory host.
const DefaultFactory = "factory.talos.dev"

// Image selects the Image Factory installer image.
type Image struct {
	// Factory is the Image Factory host serving the installer image, e.g. a self-hosted factory
	// or a mirror; it defaults to factory.talos.dev.
	Factory string `json:"factory,omitempty"`
	// Platform is the installer flavor, e.g. metal or vmware.
	Platform string `json:"platform"`
	// Schematic is the Image Factory schematic. Its ID is computed locally, as the factory does.
	Schematic map[string]any `json:"schematic"`
}

// FactoryHost returns spec.image.factory or the default factory.
func (i Image) FactoryHost() string {
	if i.Factory != "" {
		return i.Factory
	}

	return DefaultFactory
}

// Install controls the unattended installation to disk (UnattendedInstallConfig).
type Install struct {
	// DiskSelector is a CEL expression selecting the install disk, e.g. disk.dev_path == "/dev/sda".
	DiskSelector string `json:"diskSelector"`
	// Wipe wipes the selected disk before installing.
	Wipe bool `json:"wipe,omitempty"`
}

// ConfigPatches lists the patches applied to every node and to every node of one role.
type ConfigPatches struct {
	All          []Patch `json:"all,omitempty"`
	ControlPlane []Patch `json:"controlPlane,omitempty"`
	Worker       []Patch `json:"worker,omitempty"`
}

// Patch is a machine config patch, either a path to a patch file (relative to the definition) or
// an inline strategic merge patch document. JSON6902 operation lists are not accepted inline:
// Talos v1.14 machine configs are multi-document, which JSON6902 patches cannot be applied to.
type Patch struct {
	File   string
	Inline json.RawMessage
}

// UnmarshalJSON decodes a string as a file path and anything else as an inline patch.
func (p *Patch) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &p.File)
	}

	p.Inline = append(json.RawMessage(nil), data...)

	return nil
}

// MarshalJSON is the inverse of UnmarshalJSON.
func (p Patch) MarshalJSON() ([]byte, error) {
	if p.Inline != nil {
		return p.Inline, nil
	}

	return json.Marshal(p.File)
}

func (p Patch) validate() string {
	if p.Inline == nil {
		if p.File == "" {
			return "must not be empty"
		}

		return ""
	}

	switch p.Inline[0] {
	case '{':
		return ""
	case '[':
		return "JSON6902 patches cannot be applied to multi-document Talos v1.14 configs; use a strategic merge patch"
	default:
		return "must be a file path or an inline patch document"
	}
}

// Network is the static network setup shared by all nodes.
type Network struct {
	Interface string `json:"interface"`
	Prefix    int    `json:"prefix"`
	Gateway   string `json:"gateway"`
}

// Node is a single cluster member.
type Node struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Address string `json:"address"`
	// Patches apply to this node only, after spec.configPatches.
	Patches []Patch `json:"patches,omitempty"`
}

// IsControlPlane reports whether the node runs the control plane.
func (n Node) IsControlPlane() bool { return n.Role == RoleControlPlane }

var (
	talosVersionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	k8sVersionRe   = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// RFC 1123 label, which is what Talos accepts as hostname and Kubernetes as node name.
	nodeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// A registry host: DNS name or IP, optional port.
	factoryHostRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:\d+)?$`)
)

// Load reads, strictly decodes and validates the cluster definition at path.
func Load(path string) (*TalosCluster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cluster, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cluster.dir = filepath.Dir(path)

	return cluster, nil
}

// Parse strictly decodes and validates a cluster definition.
func Parse(data []byte) (*TalosCluster, error) {
	var cluster TalosCluster

	if err := yaml.UnmarshalStrict(data, &cluster); err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}

	if err := cluster.Validate(); err != nil {
		return nil, err
	}

	return &cluster, nil
}

// Validate checks the definition and returns every problem found.
//
//nolint:gocyclo
func (c *TalosCluster) Validate() error {
	var errs []error

	fail := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if c.APIVersion != APIVersion {
		fail("apiVersion", "must be %q, got %q", APIVersion, c.APIVersion)
	}

	if c.Kind != Kind {
		fail("kind", "must be %q, got %q", Kind, c.Kind)
	}

	if c.Metadata.Name == "" {
		fail("metadata.name", "is required")
	}

	s := c.Spec

	if s.Endpoint == "" {
		fail("spec.endpoint", "is required")
	}

	if s.SecretsFile == "" {
		fail("spec.secretsFile", "is required")
	}

	if !talosVersionRe.MatchString(s.Versions.Talos) {
		fail("spec.versions.talos", "must look like v1.14.0, got %q", s.Versions.Talos)
	} else if v := semver.MustParse(s.Versions.Talos[1:]); v.LT(MinTalosVersion) {
		fail("spec.versions.talos", "%s is not supported, the minimum is v%s", s.Versions.Talos, MinTalosVersion)
	}

	if !k8sVersionRe.MatchString(s.Versions.Kubernetes) {
		fail("spec.versions.kubernetes", "must look like 1.35.0 (no leading v), got %q", s.Versions.Kubernetes)
	}

	if s.Image.Platform == "" {
		fail("spec.image.platform", "is required")
	}

	if _, ok := s.Image.Schematic["customization"]; !ok {
		fail("spec.image.schematic.customization", "is required")
	} else if _, err := factory.ParseSchematic(s.Image.Schematic); err != nil {
		fail("spec.image.schematic", "%v", err)
	}

	if f := s.Image.Factory; f != "" && !factoryHostRe.MatchString(f) {
		fail("spec.image.factory", "must be a host name with an optional port, without scheme or path, got %q", f)
	}

	if s.Install.DiskSelector == "" {
		fail("spec.install.diskSelector", `is required, e.g. disk.dev_path == "/dev/sda"`)
	}

	validatePatches := func(field string, patches []Patch) {
		for i, p := range patches {
			if msg := p.validate(); msg != "" {
				fail(fmt.Sprintf("%s[%d]", field, i), "%s", msg)
			}
		}
	}

	validatePatches("spec.configPatches.all", s.ConfigPatches.All)
	validatePatches("spec.configPatches.controlPlane", s.ConfigPatches.ControlPlane)
	validatePatches("spec.configPatches.worker", s.ConfigPatches.Worker)

	if s.Network.Interface == "" {
		fail("spec.network.interface", "is required")
	}

	gateway, err := netip.ParseAddr(s.Network.Gateway)
	if err != nil {
		fail("spec.network.gateway", "must be an IP address, got %q", s.Network.Gateway)
	}

	maxPrefix := 32
	if gateway.Is6() {
		maxPrefix = 128
	}

	if s.Network.Prefix < 1 || s.Network.Prefix > maxPrefix {
		fail("spec.network.prefix", "must be between 1 and %d, got %d", maxPrefix, s.Network.Prefix)
	}

	names := map[string]bool{}
	addresses := map[string]bool{}
	controlPlanes := 0

	for i, n := range s.Nodes {
		field := fmt.Sprintf("spec.nodes[%d]", i)

		if !nodeNameRe.MatchString(n.Name) {
			fail(field+".name", "must be a lowercase RFC 1123 label, got %q", n.Name)
		} else if names[n.Name] {
			fail(field+".name", "duplicate node name %q", n.Name)
		}

		names[n.Name] = true

		switch n.Role {
		case RoleControlPlane:
			controlPlanes++
		case RoleWorker:
		default:
			fail(field+".role", "must be %q or %q, got %q", RoleControlPlane, RoleWorker, n.Role)
		}

		addr, err := netip.ParseAddr(n.Address)
		if err != nil {
			fail(field+".address", "must be an IP address, got %q", n.Address)
		} else if gateway.IsValid() && addr.Is4() != gateway.Is4() {
			fail(field+".address", "IP family does not match spec.network.gateway")
		} else if addresses[addr.String()] {
			fail(field+".address", "duplicate address %s", addr)
		}

		addresses[addr.String()] = true

		validatePatches(field+".patches", n.Patches)
	}

	if controlPlanes == 0 {
		fail("spec.nodes", "at least one node with role %q is required", RoleControlPlane)
	}

	return errors.Join(errs...)
}

// Path resolves a path from the definition relative to the definition's directory.
func (c *TalosCluster) Path(p string) string {
	if filepath.IsAbs(p) || c.dir == "" {
		return p
	}

	return filepath.Join(c.dir, p)
}

// PatchArg renders a patch the way talosctl --config-patch takes it: @<path> or the patch itself.
func (c *TalosCluster) PatchArg(p Patch) string {
	if p.Inline != nil {
		return string(p.Inline)
	}

	return "@" + c.Path(p.File)
}

// ControlPlaneNodes returns the control plane nodes in definition order.
func (c *TalosCluster) ControlPlaneNodes() []Node { return c.byRole(RoleControlPlane) }

// WorkerNodes returns the worker nodes in definition order.
func (c *TalosCluster) WorkerNodes() []Node { return c.byRole(RoleWorker) }

// OrderedNodes returns control plane nodes first, then workers: the order rolling operations use.
func (c *TalosCluster) OrderedNodes() []Node {
	return append(c.ControlPlaneNodes(), c.WorkerNodes()...)
}

// Node looks a node up by name.
func (c *TalosCluster) Node(name string) (Node, error) {
	for _, n := range c.Spec.Nodes {
		if n.Name == name {
			return n, nil
		}
	}

	return Node{}, fmt.Errorf("no node named %q in the cluster definition", name)
}

// Addresses returns the addresses of the given nodes.
func Addresses(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Address)
	}

	return out
}

// ClusterEndpointURL is the Kubernetes API URL embedded in machine configs.
func (c *TalosCluster) ClusterEndpointURL() string {
	return fmt.Sprintf("https://%s:6443", c.Spec.Endpoint)
}

// InstallerImage is the Image Factory installer reference for a schematic ID.
func (c *TalosCluster) InstallerImage(schematicID string) string {
	return fmt.Sprintf("%s/%s-installer/%s:%s", c.Spec.Image.FactoryHost(), c.Spec.Image.Platform, schematicID, c.Spec.Versions.Talos)
}

func (c *TalosCluster) byRole(role string) []Node {
	var out []Node

	for _, n := range c.Spec.Nodes {
		if n.Role == role {
			out = append(out, n)
		}
	}

	return out
}
