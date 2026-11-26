package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/siderolabs/go-kubernetes/kubernetes/ssa"
	"github.com/siderolabs/go-kubernetes/kubernetes/upgrade"
	talosclusterpkg "github.com/siderolabs/talos/pkg/cluster"
	k8s "github.com/siderolabs/talos/pkg/cluster/kubernetes"
	clusterapi "github.com/siderolabs/talos/pkg/machinery/api/cluster"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"go.yaml.in/yaml/v4"
	"google.golang.org/grpc/codes"

	"github.com/vchrisb/tanvil/internal/config"
)

const (
	talosconfigFile = "talosconfig"
	kubeconfigFile  = "kubeconfig"

	healthTimeout = 20 * time.Minute
)

// TALOS-SYNC: mirrors talosctl gen secrets (cmd/talosctl/cmd/mgmt/gen/secrets.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// GenSecrets writes a new secrets bundle to spec.secretsFile.
func (m *Manager) GenSecrets() error {
	contract, err := m.versionContract()
	if err != nil {
		return err
	}

	bundle, err := secrets.NewBundle(secrets.NewFixedClock(time.Now()), contract)
	if err != nil {
		return fmt.Errorf("generating secrets: %w", err)
	}

	data, err := yaml.Marshal(bundle)
	if err != nil {
		return err
	}

	path := m.Cluster.Path(m.Cluster.Spec.SecretsFile)
	if err := m.writeFile(path, data, 0o600); err != nil {
		return err
	}

	m.printf("Secrets written to %s\n", path)

	return nil
}

// TALOS-SYNC: mirrors talosctl gen config (cmd/talosctl/cmd/mgmt/gen/config.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// NodeConfig renders the machine config of a single node.
func (m *Manager) NodeConfig(ctx context.Context, node config.Node) ([]byte, error) {
	image, err := m.InstallerImage(ctx)
	if err != nil {
		return nil, err
	}

	input, err := m.generateInput(generate.WithInstallImage(image))
	if err != nil {
		return nil, err
	}

	// The generated documents come first so that the cluster-wide, role and node patches, applied
	// in that order, can override them, e.g. a node patch selecting a different install disk.
	patchArgs, err := m.nodePatches(node, image)
	if err != nil {
		return nil, err
	}

	patches := m.Cluster.Spec.ConfigPatches
	userPatches := append([]config.Patch{}, patches.All...)

	machineType := machine.TypeWorker
	if node.IsControlPlane() {
		machineType = machine.TypeControlPlane
		userPatches = append(userPatches, patches.ControlPlane...)
	} else {
		userPatches = append(userPatches, patches.Worker...)
	}

	userPatches = append(userPatches, node.Patches...)

	for _, p := range userPatches {
		patchArgs = append(patchArgs, m.Cluster.PatchArg(p))
	}

	loaded, err := configpatcher.LoadPatches(patchArgs)
	if err != nil {
		return nil, fmt.Errorf("loading config patches: %w", err)
	}

	cfg, err := input.Config(machineType)
	if err != nil {
		return nil, fmt.Errorf("generating %s config: %w", machineType, err)
	}

	out, err := configpatcher.Apply(configpatcher.WithConfig(cfg), loaded)
	if err != nil {
		return nil, fmt.Errorf("applying config patches for %s: %w", node.Name, err)
	}

	patched, err := out.Config()
	if err != nil {
		return nil, err
	}

	if err := m.validateConfig(node, patched); err != nil {
		return nil, err
	}

	return patched.EncodeBytes(encoder.WithComments(encoder.CommentsAll))
}

// nodePatches returns the install, hostname and static network documents of a node.
func (m *Manager) nodePatches(node config.Node, image string) ([]string, error) {
	network := m.Cluster.Spec.Network
	install := m.Cluster.Spec.Install

	docs := []map[string]any{
		{
			// Since the v1.14 config contract the installer lives in its own document.
			"apiVersion": "v1alpha1",
			"kind":       "UnattendedInstallConfig",
			"installer":  map[string]any{"image": image},
			"provisioning": map[string]any{
				"diskSelector": map[string]any{"match": install.DiskSelector},
				"wipe":         install.Wipe,
			},
		},
		{
			"apiVersion": "v1alpha1",
			"kind":       "HostnameConfig",
			"hostname":   node.Name,
			"auto":       "off",
		},
		{
			"apiVersion": "v1alpha1",
			"kind":       "LinkConfig",
			"name":       network.Interface,
			"up":         true,
			"addresses":  []map[string]any{{"address": fmt.Sprintf("%s/%d", node.Address, network.Prefix)}},
			"routes":     []map[string]any{{"gateway": network.Gateway}},
		},
	}

	out := make([]string, 0, len(docs))

	for _, doc := range docs {
		data, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}

		out = append(out, string(data))
	}

	return out, nil
}

// GenConfig writes <name>.yaml for the given nodes into the working directory. Every config is
// rendered and validated first, so nothing is written when any node's config is invalid.
func (m *Manager) GenConfig(ctx context.Context, nodes []config.Node) error {
	rendered := make([][]byte, len(nodes))

	var errs []error

	for i, node := range nodes {
		m.printf("  -> Generating config for %s...\n", node.Name)

		data, err := m.NodeConfig(ctx, node)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		rendered[i] = data
	}

	if len(errs) > 0 {
		return fmt.Errorf("no configs written: %w", errors.Join(errs...))
	}

	for i, node := range nodes {
		if err := m.writeFile(node.Name+".yaml", rendered[i], 0o600); err != nil {
			return err
		}
	}

	return nil
}

// TALOS-SYNC: mirrors talosctl gen config --output-types talosconfig (cmd/talosctl/cmd/mgmt/gen/config.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// WriteTalosconfig writes a talosconfig for talosctl with all control plane endpoints.
func (m *Manager) WriteTalosconfig() error {
	input, err := m.generateInput()
	if err != nil {
		return err
	}

	cfg, err := input.Talosconfig()
	if err != nil {
		return err
	}

	data, err := cfg.Bytes()
	if err != nil {
		return err
	}

	if err := m.writeFile(talosconfigFile, data, 0o600); err != nil {
		return err
	}

	m.printf("Wrote %s with endpoints %v\n", talosconfigFile, config.Addresses(m.Cluster.ControlPlaneNodes()))

	return nil
}

// CheckKubeconfigWritable fails when ./kubeconfig exists and --force is not set, so callers can
// check before changing the cluster.
func (m *Manager) CheckKubeconfigWritable() error {
	return m.checkWritable(kubeconfigFile)
}

// TALOS-SYNC: mirrors talosctl kubeconfig (cmd/talosctl/cmd/talos/kubeconfig.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// WriteKubeconfig fetches the admin kubeconfig over the Talos API into ./kubeconfig.
func (m *Manager) WriteKubeconfig(ctx context.Context) error {
	if err := m.CheckKubeconfigWritable(); err != nil {
		return err
	}

	c, err := m.Talos(ctx)
	if err != nil {
		return err
	}

	data, err := c.Kubeconfig(ctx)
	if err != nil {
		return fmt.Errorf("fetching kubeconfig: %w", err)
	}

	if err := m.writeFile(kubeconfigFile, data, 0o600); err != nil {
		return err
	}

	m.printf("Wrote %s\n", kubeconfigFile)

	return nil
}

// TALOS-SYNC: mirrors talosctl apply-config (cmd/talosctl/cmd/talos/apply-config.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// Update renders each node's machine config from the definition, exactly like genconfig, and
// applies it where it differs from the running config. Before the first node is touched, all
// configs are rendered and validated, every node passes the health pre-flight check, and every
// node compares its running config with the new one in an apply-config dry run (which also runs
// the node's own validation). Nodes without changes are skipped; for the others the diff is
// shown and, unless --assume-yes is set, confirmed. With --dry-run nothing is applied and
// ErrChanges is returned when any node would change.
//
// maintenanceAddress, when set, applies to a single unconfigured node in maintenance mode at
// that address; there is no running config to compare with, so it is applied directly.
func (m *Manager) Update(ctx context.Context, nodes []config.Node, maintenanceAddress string) error {
	if maintenanceAddress != "" && len(nodes) != 1 {
		return errors.New("a maintenance mode address requires exactly one node")
	}

	rendered := make([][]byte, len(nodes))

	var errs []error

	for i, node := range nodes {
		data, err := m.NodeConfig(ctx, node)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		rendered[i] = data
	}

	if len(errs) > 0 {
		return fmt.Errorf("no node updated: %w", errors.Join(errs...))
	}

	if maintenanceAddress != "" {
		return m.updateNode(ctx, nodes[0], maintenanceAddress, rendered[0])
	}

	if err := m.EnsureNodesHealthy(ctx, nodes); err != nil {
		return err
	}

	diffs := make([]string, len(nodes))

	for i, node := range nodes {
		diff, err := m.liveConfigDiff(ctx, node, rendered[i])
		if err != nil {
			errs = append(errs, err)

			continue
		}

		diffs[i] = diff
	}

	if len(errs) > 0 {
		return fmt.Errorf("no node updated: %w", errors.Join(errs...))
	}

	changed := 0

	for i, node := range nodes {
		if diffs[i] == "" {
			m.printf("==> %s (%s): no changes\n", node.Name, node.Address)

			continue
		}

		changed++

		m.printf("==> %s (%s):\n%s\n", node.Name, node.Address, indent(diffs[i], "    "))

		if m.Options.DryRun {
			continue
		}

		if !m.Options.AssumeYes {
			if err := m.Confirm(fmt.Sprintf("Apply this change to %s?", node.Name)); err != nil {
				return err
			}
		}

		if err := m.updateNode(ctx, node, "", rendered[i]); err != nil {
			return err
		}
	}

	if m.Options.DryRun && changed > 0 {
		return fmt.Errorf("%w on %d node(s)", ErrChanges, changed)
	}

	return nil
}

// ErrChanges is returned by "update --dry-run" when applying would change at least one node.
var ErrChanges = errors.New("changes pending")

func (m *Manager) updateNode(ctx context.Context, node config.Node, maintenanceAddress string, data []byte) error {
	if err := m.EnsurePodsReady(ctx, "updating", node.Name); err != nil {
		return err
	}

	m.printf("  -> Applying config to %s...\n", node.Name)

	if err := m.applyConfig(ctx, node, maintenanceAddress, data); err != nil {
		return err
	}

	if m.Options.DryRun {
		return nil
	}

	_, err := m.WaitForPods(ctx, "updating", node.Name)

	return err
}

// nodeApplyConfig sends data to the node with the apply-config API, in AUTO mode.
func (m *Manager) nodeApplyConfig(ctx context.Context, node config.Node, maintenanceAddress string, data []byte) error {
	var (
		c       *client.Client
		err     error
		nodeCtx = ctx
	)

	if maintenanceAddress != "" {
		c, err = client.New(ctx, client.WithMaintenanceMode(maintenanceAddress, nil), client.WithDefaultGRPCDialOptions())
		if err != nil {
			return fmt.Errorf("connecting to %s in maintenance mode: %w", maintenanceAddress, err)
		}

		defer c.Close() //nolint:errcheck
	} else {
		if c, err = m.Talos(ctx); err != nil {
			return err
		}

		nodeCtx = client.WithNode(ctx, node.Address)
	}

	resp, err := c.ApplyConfiguration(nodeCtx, &machineapi.ApplyConfigurationRequest{
		Data:   data,
		Mode:   machineapi.ApplyConfigurationRequest_AUTO,
		DryRun: m.Options.DryRun,
	})
	if err != nil {
		return fmt.Errorf("applying config to %s: %w", node.Name, err)
	}

	for _, msg := range resp.GetMessages() {
		for _, w := range msg.GetWarnings() {
			m.printf("Warning: %s\n", w)
		}

		if msg.GetModeDetails() != "" {
			m.printf("%s\n", msg.GetModeDetails())
		}
	}

	return nil
}

// TALOS-SYNC: mirrors talosctl bootstrap (cmd/talosctl/cmd/talos/bootstrap.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// Bootstrap starts etcd on the first control plane node.
func (m *Manager) Bootstrap(ctx context.Context) error {
	c, err := m.Talos(ctx)
	if err != nil {
		return err
	}

	node := m.firstControlPlane()
	m.printf("Bootstrapping etcd on %s (%s)...\n", node.Name, node.Address)

	if err := c.Bootstrap(m.firstControlPlaneCtx(ctx), &machineapi.BootstrapRequest{}); err != nil {
		if client.StatusCode(err) == codes.AlreadyExists {
			m.printf("Cluster is already bootstrapped.\n")

			return nil
		}

		return fmt.Errorf("bootstrapping: %w", err)
	}

	return nil
}

// TALOS-SYNC: mirrors talosctl health (cmd/talosctl/cmd/talos/health.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// Health runs the server-side cluster health check against the configured nodes.
func (m *Manager) Health(ctx context.Context) error {
	c, err := m.Talos(ctx)
	if err != nil {
		return err
	}

	m.printf("Checking cluster health...\n")

	stream, err := c.ClusterHealthCheck(m.firstControlPlaneCtx(ctx), healthTimeout, &clusterapi.ClusterInfo{
		ControlPlaneNodes: config.Addresses(m.Cluster.ControlPlaneNodes()),
		WorkerNodes:       config.Addresses(m.Cluster.WorkerNodes()),
	})
	if err != nil {
		return fmt.Errorf("starting health check: %w", err)
	}

	if err := stream.CloseSend(); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || client.StatusCode(err) == codes.Canceled {
				return nil
			}

			return fmt.Errorf("health check: %w", err)
		}

		m.printf("%s\n", msg.GetMessage())
	}
}

// TALOS-SYNC: mirrors talosctl upgrade-k8s (cmd/talosctl/cmd/talos/upgrade-k8s.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// UpgradeKubernetes upgrades the control plane components and kubelets to spec.versions.kubernetes.
func (m *Manager) UpgradeKubernetes(ctx context.Context) error {
	c, err := m.Talos(ctx)
	if err != nil {
		return err
	}

	m.printf("Upgrading Kubernetes to %s...\n", m.Cluster.Spec.Versions.Kubernetes)

	ctx = m.firstControlPlaneCtx(ctx)
	state := k8sUpgradeState(c)
	opts := m.k8sUpgradeOptions()

	from, err := k8s.DetectLowestVersion(ctx, state, opts)
	if err != nil {
		return fmt.Errorf("detecting the current Kubernetes version: %w", err)
	}

	m.printf("Current Kubernetes version: %s\n", from)

	opts.Path, err = upgrade.NewPath(from, m.Cluster.Spec.Versions.Kubernetes)
	if err != nil {
		return fmt.Errorf("planning upgrade path: %w", err)
	}

	return k8s.Upgrade(ctx, state, opts)
}

// lineWriter appends the newline the Kubernetes upgrade log lines omit.
type lineWriter struct{ w io.Writer }

func (l lineWriter) Write(p []byte) (int, error) {
	if _, err := l.w.Write(p); err != nil {
		return 0, err
	}

	if len(p) == 0 || p[len(p)-1] != '\n' {
		if _, err := l.w.Write([]byte{'\n'}); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// k8sUpgradeState is the cluster access talosctl upgrade-k8s uses.
func k8sUpgradeState(c *client.Client) k8s.UpgradeProvider {
	provider := &talosclusterpkg.ConfigClientProvider{DefaultClient: c}

	return &struct {
		talosclusterpkg.ClientProvider
		talosclusterpkg.K8sProvider
	}{
		ClientProvider: provider,
		K8sProvider:    &talosclusterpkg.KubernetesClient{ClientProvider: provider},
	}
}

// k8sUpgradeOptions are talosctl upgrade-k8s's defaults.
func (m *Manager) k8sUpgradeOptions() k8s.UpgradeOptions {
	return k8s.UpgradeOptions{
		DryRun:                 m.Options.DryRun,
		LogOutput:              lineWriter{m.Out},
		PrePullImages:          true,
		UpgradeKubelet:         true,
		EncoderOpt:             encoder.WithComments(encoder.CommentsAll),
		KubeletImage:           constants.KubeletImage,
		APIServerImage:         constants.KubernetesAPIServerImage,
		ControllerManagerImage: constants.KubernetesControllerManagerImage,
		SchedulerImage:         constants.KubernetesSchedulerImage,
		ProxyImage:             constants.KubeProxyImage,
		ReconcileTimeout:       5 * time.Minute,
		InventoryPolicy:        ssa.InventoryPolicyAdoptIfNoInventory,
	}
}
