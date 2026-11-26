package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	k8s "github.com/siderolabs/talos/pkg/cluster/kubernetes"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"

	"github.com/vchrisb/tanvil/internal/config"
)

// ErrDrift is returned by Diff when the cluster does not match the definition.
var ErrDrift = errors.New("cluster differs from the definition")

// metalMode is talosctl validate --mode metal: nodes are installed to disk by the installer.
type metalMode struct{}

func (metalMode) String() string        { return "metal" }
func (metalMode) RequiresInstall() bool { return true }
func (metalMode) InContainer() bool     { return false }

// TALOS-SYNC: mirrors talosctl validate --mode metal (cmd/talosctl/cmd/mgmt/validate.go). When
// bumping github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// validateConfig runs the static checks talosctl validate runs. Checks that need the node itself,
// such as whether the install disk exists, only happen when the config is applied.
func (m *Manager) validateConfig(node config.Node, cfg machineryconfig.Provider) error {
	warnings, err := cfg.ValidateAsClient(metalMode{}, validation.WithLocal())
	for _, w := range warnings {
		m.printf("Warning: %s: %s\n", node.Name, w)
	}

	if err != nil {
		return fmt.Errorf("%s: invalid machine config: %w", node.Name, err)
	}

	return nil
}

// NodeVersion is what a node actually booted, as opposed to what its machine config declares.
type NodeVersion struct {
	// Talos is the running Talos version, e.g. v1.14.7.
	Talos string
	// Schematic is the Image Factory schematic ID of the running image; empty when the node was
	// not installed from an Image Factory image.
	Schematic string
}

// nodeVersion reads the running Talos version and schematic from the node's runtime resources.
func (m *Manager) nodeVersion(ctx context.Context, node config.Node) (NodeVersion, error) {
	c, err := m.Talos(ctx)
	if err != nil {
		return NodeVersion{}, err
	}

	ctx = client.WithNode(ctx, node.Address)

	version, err := safe.StateGetByID[*runtime.Version](ctx, c.COSI, "version")
	if err != nil {
		return NodeVersion{}, fmt.Errorf("reading the Talos version of %s: %w", node.Name, err)
	}

	nv := NodeVersion{Talos: version.TypedSpec().Version}

	schematic, err := safe.StateGetByID[*runtime.ImageFactorySchematic](ctx, c.COSI, runtime.ImageFactorySchematicID)

	switch {
	case err == nil:
		nv.Schematic = schematic.TypedSpec().SchematicID
	case !state.IsNotFoundError(err):
		return NodeVersion{}, fmt.Errorf("reading the schematic of %s: %w", node.Name, err)
	}

	return nv, nil
}

// nodeConfigDiff returns the server-side diff between the node's running machine config and data
// (an apply-config dry run), or "" when there is no difference.
func (m *Manager) nodeConfigDiff(ctx context.Context, node config.Node, data []byte) (string, error) {
	c, err := m.Talos(ctx)
	if err != nil {
		return "", err
	}

	resp, err := c.ApplyConfiguration(client.WithNode(ctx, node.Address), &machineapi.ApplyConfigurationRequest{
		Data:   data,
		Mode:   machineapi.ApplyConfigurationRequest_AUTO,
		DryRun: true,
	})
	if err != nil {
		return "", fmt.Errorf("comparing the config of %s: %w", node.Name, err)
	}

	var details strings.Builder

	for _, msg := range resp.GetMessages() {
		details.WriteString(msg.GetModeDetails())
	}

	return parseConfigDiff(details.String()), nil
}

// parseConfigDiff extracts the diff from apply-config dry-run details ("... Config diff:\n\n<diff>").
func parseConfigDiff(details string) string {
	const marker = "Config diff:"

	if i := strings.Index(details, marker); i >= 0 {
		details = details[i+len(marker):]
	}

	details = strings.TrimSpace(details)
	if details == "No changes." {
		return ""
	}

	return details
}

// clusterKubernetesVersion detects the lowest running Kubernetes version the way talosctl
// upgrade-k8s does.
func (m *Manager) clusterKubernetesVersion(ctx context.Context) (string, error) {
	c, err := m.Talos(ctx)
	if err != nil {
		return "", err
	}

	return k8s.DetectLowestVersion(m.firstControlPlaneCtx(ctx), k8sUpgradeState(c), m.k8sUpgradeOptions())
}

func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// versionDrift lists how a node's running version differs from the wanted one.
func versionDrift(running NodeVersion, talos, schematic string) []string {
	var drift []string

	if !sameVersion(running.Talos, talos) {
		drift = append(drift, fmt.Sprintf("talos: running %s, want %s", running.Talos, talos))
	}

	if running.Schematic != schematic {
		have := running.Schematic
		if have == "" {
			have = "none (not an Image Factory image)"
		}

		drift = append(drift, fmt.Sprintf("schematic: running %s, want %s", have, schematic))
	}

	return drift
}

// Diff compares the running cluster with the definition: per node the booted Talos version and
// schematic and the machine config (rendered fresh from the definition), then the cluster's
// Kubernetes version. It returns ErrDrift when anything differs.
func (m *Manager) Diff(ctx context.Context, nodes []config.Node) error {
	spec := m.Cluster.Spec

	schematic, err := m.SchematicID(ctx)
	if err != nil {
		return err
	}

	drifted := false

	var errs []error

	for _, node := range nodes {
		data, err := m.NodeConfig(ctx, node)
		if err != nil {
			return err
		}

		running, err := m.liveVersion(ctx, node)
		if err != nil {
			errs = append(errs, err)
			m.printf("==> %s (%s): %v\n", node.Name, node.Address, err)

			continue
		}

		lines := versionDrift(running, spec.Versions.Talos, schematic)

		diff, err := m.liveConfigDiff(ctx, node, data)
		if err != nil {
			errs = append(errs, err)
			lines = append(lines, fmt.Sprintf("config: %v", err))
		} else if diff != "" {
			lines = append(lines, "config:\n"+indent(diff, "      "))
		}

		if len(lines) == 0 {
			m.printf("==> %s (%s): in sync\n", node.Name, node.Address)

			continue
		}

		drifted = true

		m.printf("==> %s (%s):\n", node.Name, node.Address)

		for _, l := range lines {
			m.printf("    %s\n", l)
		}
	}

	kube, err := m.liveKubernetes(ctx)

	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("detecting the Kubernetes version: %w", err))
		m.printf("==> kubernetes: %v\n", err)
	case !sameVersion(kube, spec.Versions.Kubernetes):
		drifted = true

		m.printf("==> kubernetes: running %s, want %s\n", strings.TrimPrefix(kube, "v"), spec.Versions.Kubernetes)
	default:
		m.printf("==> kubernetes: in sync (%s)\n", spec.Versions.Kubernetes)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if drifted {
		return ErrDrift
	}

	return nil
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}

	return strings.Join(lines, "\n")
}

// pendingUpgrades drops the nodes that already run the target Talos version and schematic, so a
// rolling upgrade can be resumed after it stopped half way. --force keeps every node.
func (m *Manager) pendingUpgrades(ctx context.Context, nodes []config.Node) ([]config.Node, error) {
	if m.Options.Force {
		return nodes, nil
	}

	schematic, err := m.SchematicID(ctx)
	if err != nil {
		return nil, err
	}

	pending := make([]config.Node, 0, len(nodes))

	for _, node := range nodes {
		running, err := m.liveVersion(ctx, node)
		if err != nil {
			return nil, err
		}

		if len(versionDrift(running, m.Cluster.Spec.Versions.Talos, schematic)) == 0 {
			m.printf("%s already runs %s with schematic %s, skipping (--force upgrades it anyway)\n",
				node.Name, running.Talos, running.Schematic)

			continue
		}

		pending = append(pending, node)
	}

	return pending, nil
}
