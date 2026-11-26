package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `
apiVersion: tanvil.dev/v1alpha1
kind: TalosCluster
metadata:
  name: homelab
spec:
  endpoint: k8s.example.com
  secretsFile: secrets.yaml
  versions:
    talos: v1.14.7
    kubernetes: 1.35.0
  image:
    platform: metal
    schematic:
      customization: {}
  install:
    diskSelector: disk.dev_path == "/dev/sda"
  network:
    interface: ens3
    prefix: 24
    gateway: 10.0.0.1
  nodes:
    - {name: cp-01, role: controlplane, address: 10.0.0.11}
    - {name: worker-01, role: worker, address: 10.0.0.21}
    - {name: cp-02, role: controlplane, address: 10.0.0.12}
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, n := range c.OrderedNodes() {
		names = append(names, n.Name)
	}

	if got := strings.Join(names, ","); got != "cp-01,cp-02,worker-01" {
		t.Errorf("OrderedNodes = %s, want control planes first", got)
	}

	if got := c.ClusterEndpointURL(); got != "https://k8s.example.com:6443" {
		t.Errorf("ClusterEndpointURL = %s", got)
	}

	if got := c.InstallerImage("abc"); got != "factory.talos.dev/metal-installer/abc:v1.14.7" {
		t.Errorf("InstallerImage = %s", got)
	}

	c.Spec.Image.Factory = "factory.example.com:5000"
	if got := c.InstallerImage("abc"); got != "factory.example.com:5000/metal-installer/abc:v1.14.7" {
		t.Errorf("InstallerImage with factory = %s", got)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(valid, "  endpoint:", "  fqdn: x\n  endpoint:", 1)))
	if err == nil || !strings.Contains(err.Error(), "fqdn") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		from, to, want string
	}{
		"kind":            {"kind: TalosCluster", "kind: Cluster", "kind: must be"},
		"old talos":       {"talos: v1.14.7", "talos: v1.13.4", "minimum is v1.14.0"},
		"k8s v prefix":    {"kubernetes: 1.35.0", "kubernetes: v1.35.0", "no leading v"},
		"no control":      {"role: controlplane", "role: worker", "at least one node"},
		"bad role":        {"role: worker,", "role: compute,", "role: must be"},
		"duplicate name":  {"name: cp-02", "name: cp-01", "duplicate node name"},
		"duplicate addr":  {"address: 10.0.0.12", "address: 10.0.0.11", "duplicate address"},
		"bad address":     {"address: 10.0.0.21", "address: 10.0.0", "must be an IP"},
		"family mismatch": {"address: 10.0.0.21", "address: fd00::21", "IP family"},
		"prefix":          {"prefix: 24", "prefix: 33", "between 1 and 32"},
		"no schematic":    {"customization: {}", "overlay: {}", "customization: is required"},
		"schematic field": {"customization: {}", "customization: {extensions: []}", "spec.image.schematic: invalid schematic"},
		"factory scheme":  {"platform: metal", "platform: metal\n    factory: https://factory.example.com", "spec.image.factory: must be a host name"},
		"name":            {"name: worker-01", "name: Worker_01", "RFC 1123"},
		"empty patch":     {"- {name: cp-02, role: controlplane, address: 10.0.0.12}", "- {name: cp-02, role: controlplane, address: 10.0.0.12, patches: [\"\"]}", "spec.nodes[2].patches[0]: must not be empty"},
		"scalar patch":    {"  network:", "  configPatches:\n    all: [42]\n  network:", "spec.configPatches.all[0]: must be a file path"},
		"json6902":        {"  network:", "  configPatches:\n    worker: [[{op: remove, path: /machine/install}]]\n  network:", "spec.configPatches.worker[0]: JSON6902"},
		"no disk":         {`diskSelector: disk.dev_path == "/dev/sda"`, `diskSelector: ""`, "diskSelector: is required"},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(valid, tc.from) {
				t.Fatalf("fixture does not contain %q", tc.from)
			}

			_, err := Parse([]byte(strings.ReplaceAll(valid, tc.from, tc.to)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadResolvesPathsRelativeToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tanvil.yaml")

	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := c.Path("secrets.yaml"); got != filepath.Join(dir, "secrets.yaml") {
		t.Errorf("Path = %s", got)
	}

	if got := c.Path("/abs/p.yaml"); got != "/abs/p.yaml" {
		t.Errorf("absolute Path = %s", got)
	}
}

func TestExampleIsValid(t *testing.T) {
	if _, err := Load("../../examples/tanvil.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestPatches(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(valid, "  network:", `  configPatches:
    all:
      - patches/common.yaml
      - machine: {time: {servers: [time.example.com]}}
    worker:
      - {apiVersion: v1alpha1, kind: KubeNodeConfig, labels: {role: worker}}
  network:`, 1)))
	if err != nil {
		t.Fatal(err)
	}

	c.dir = "/defs"

	got := []string{}
	for _, p := range append(c.Spec.ConfigPatches.All, c.Spec.ConfigPatches.Worker...) {
		got = append(got, c.PatchArg(p))
	}

	want := []string{
		"@/defs/patches/common.yaml",
		`{"machine":{"time":{"servers":["time.example.com"]}}}`,
		`{"apiVersion":"v1alpha1","kind":"KubeNodeConfig","labels":{"role":"worker"}}`,
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("PatchArg = %q, want %q", got, want)
	}
}
