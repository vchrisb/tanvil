// Package cluster implements the cluster operations on top of the Talos and Kubernetes Go clients.
//
// The operations reimplement talosctl commands with the Go bindings. Every function that mirrors a
// talosctl command is marked with a TALOS-SYNC comment naming the talosctl source file it follows.
// When upgrading github.com/siderolabs/talos, run `grep -rn TALOS-SYNC internal/` and diff each
// named file between the old and new release tag, e.g.
//
//	git -C talos diff v1.14.7 v1.15.0 -- cmd/talosctl/cmd/talos/upgrade.go
//
// and port behaviour changes (new defaults, flags, API calls, wait/retry logic) so tanvil
// keeps doing what talosctl does.
package cluster

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/siderolabs/talos/cmd/talosctl/pkg/talos/action"
	"github.com/siderolabs/talos/cmd/talosctl/pkg/talos/kubeclient"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	machineryconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"k8s.io/client-go/kubernetes"

	"github.com/vchrisb/tanvil/internal/config"
	"github.com/vchrisb/tanvil/internal/factory"
)

// Options tune how operations behave.
type Options struct {
	// DryRun shows what would change without changing the cluster.
	DryRun bool
	// Force overwrites existing output files.
	Force bool
	// AssumeYes skips the confirmation between nodes of a rolling upgrade.
	AssumeYes bool
	// SkipPodCheck disables the pod readiness gate around node changes.
	SkipPodCheck bool
	// AutoContinueOnReady skips the confirmation between nodes when the pod check passed.
	AutoContinueOnReady bool
	// NoDrain skips cordoning and draining a node before its upgrade reboot.
	NoDrain bool
	// AllowNotReady lets update and upgrade change nodes with unmet readiness conditions.
	AllowNotReady bool
	// Redact replaces private keys, tokens and encryption secrets in the output.
	Redact bool
	// SubmitToFactory registers the schematic with the Image Factory; by default its ID is only
	// computed locally, without network access.
	SubmitToFactory bool
	// DrainTimeout bounds the drain before an upgrade reboot, including the delete fallback.
	DrainTimeout time.Duration
	// DeleteIfEvictionFails retries a failed drain by deleting the pods, bypassing PodDisruptionBudgets.
	DeleteIfEvictionFails bool
}

// Manager runs cluster operations for one cluster definition.
type Manager struct {
	Cluster *config.TalosCluster
	Options Options
	Factory *factory.Client

	Out io.Writer
	In  *bufio.Reader

	// Pod check timing; overridable in tests.
	PodCheckTimeout  time.Duration
	PodCheckInterval time.Duration
	PodStableChecks  int
	sleep            func(context.Context, time.Duration) error
	now              func() time.Time

	// Live cluster state for diff and upgrade; overridable in tests.
	liveVersion    func(context.Context, config.Node) (NodeVersion, error)
	liveConfigDiff func(context.Context, config.Node, []byte) (string, error)
	liveKubernetes func(context.Context) (string, error)
	applyConfig    func(context.Context, config.Node, string, []byte) error
	liveHealth     func(context.Context, config.Node) (NodeHealth, error)
	resetNode      func(context.Context, config.Node, *machineapi.ResetRequest) error

	mu          sync.Mutex
	secrets     *secrets.Bundle
	redact      func(string) string
	schematicID string
	talos       *client.Client
	kube        kubernetes.Interface
}

// New returns a Manager writing to stdout and reading answers from stdin.
func New(cluster *config.TalosCluster, opts Options) *Manager {
	m := &Manager{
		Cluster:          cluster,
		Options:          opts,
		Factory:          factory.New(cluster.Spec.Image.FactoryHost()),
		Out:              os.Stdout,
		In:               bufio.NewReader(os.Stdin),
		PodCheckTimeout:  300 * time.Second,
		PodCheckInterval: 15 * time.Second,
		PodStableChecks:  2,
		sleep:            sleepContext,
		now:              time.Now,
	}

	m.liveVersion = m.nodeVersion
	m.liveConfigDiff = m.nodeConfigDiff
	m.liveKubernetes = m.clusterKubernetesVersion
	m.applyConfig = m.nodeApplyConfig
	m.liveHealth = m.nodeHealth
	m.resetNode = m.nodeReset

	return m
}

// Close releases the API connections.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.talos != nil {
		return m.talos.Close()
	}

	return nil
}

// printf writes to Out with the cluster secrets redacted (see Options.Redact).
func (m *Manager) printf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)

	if redact := m.redactor(); redact != nil {
		s = redact(s)
	}

	io.WriteString(m.Out, s) //nolint:errcheck
}

func (m *Manager) versionContract() (*machineryconfig.VersionContract, error) {
	return machineryconfig.ParseContractFromVersion(m.Cluster.Spec.Versions.Talos)
}

func (m *Manager) secretsBundle() (*secrets.Bundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.secrets != nil {
		return m.secrets, nil
	}

	path := m.Cluster.Path(m.Cluster.Spec.SecretsFile)

	bundle, err := secrets.LoadBundle(path)
	if err != nil {
		return nil, fmt.Errorf("loading secrets %s (create it with 'tanvil gensecrets'): %w", path, err)
	}

	m.secrets = bundle

	return bundle, nil
}

// SchematicID computes the schematic ID once per run, locally and exactly as the Image Factory
// does, so no factory access is needed. With Options.SubmitToFactory the schematic is also
// registered with the factory, which is needed once before the factory can serve a new schematic.
func (m *Manager) SchematicID(ctx context.Context) (string, error) {
	if m.schematicID != "" {
		return m.schematicID, nil
	}

	id, err := factory.LocalID(m.Cluster.Spec.Image.Schematic)
	if err != nil {
		return "", fmt.Errorf("resolving schematic: %w", err)
	}

	if m.Options.SubmitToFactory {
		remote, err := m.Factory.Submit(ctx, m.Cluster.Spec.Image.Schematic)
		if err != nil {
			return "", fmt.Errorf("submitting schematic: %w", err)
		}

		if remote != id {
			return "", fmt.Errorf("image factory assigned schematic ID %s, but tanvil computed %s: "+
				"the factory knows schematic fields tanvil does not, update tanvil", remote, id)
		}

		m.printf("Schematic %s submitted to %s\n", id, m.Cluster.Spec.Image.FactoryHost())
	}

	m.printf("Schematic ID: %s\n", id)
	m.schematicID = id

	return id, nil
}

// InstallerImage returns the installer image for the configured schematic and Talos version.
func (m *Manager) InstallerImage(ctx context.Context) (string, error) {
	id, err := m.SchematicID(ctx)
	if err != nil {
		return "", err
	}

	return m.Cluster.InstallerImage(id), nil
}

// generateInput builds the generator input shared by machine configs and talosconfig.
func (m *Manager) generateInput(extra ...generate.Option) (*generate.Input, error) {
	bundle, err := m.secretsBundle()
	if err != nil {
		return nil, err
	}

	contract, err := m.versionContract()
	if err != nil {
		return nil, err
	}

	opts := append([]generate.Option{
		generate.WithVersionContract(contract),
		generate.WithSecretsBundle(bundle),
		generate.WithEndpointList(config.Addresses(m.Cluster.ControlPlaneNodes())),
	}, extra...)

	return generate.NewInput(
		m.Cluster.Metadata.Name,
		m.Cluster.ClusterEndpointURL(),
		m.Cluster.Spec.Versions.Kubernetes,
		opts...,
	)
}

// Talos returns an authenticated Talos API client for the control plane endpoints. The client
// certificate is minted in memory from the secrets bundle, so no talosconfig file is needed.
func (m *Manager) Talos(ctx context.Context) (*client.Client, error) {
	m.mu.Lock()
	c := m.talos
	m.mu.Unlock()

	if c != nil {
		return c, nil
	}

	input, err := m.generateInput()
	if err != nil {
		return nil, err
	}

	talosconfig, err := input.Talosconfig()
	if err != nil {
		return nil, fmt.Errorf("building Talos client credentials: %w", err)
	}

	c, err = client.New(ctx,
		client.WithConfig(talosconfig),
		client.WithEndpoints(config.Addresses(m.Cluster.ControlPlaneNodes())...),
		client.WithDefaultGRPCDialOptions(),
		client.WithGRPCDialOptions(action.GRPCDialOptions()...),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to the Talos API: %w", err)
	}

	m.mu.Lock()
	m.talos = c
	m.mu.Unlock()

	return c, nil
}

// Kubernetes returns a clientset built from the admin kubeconfig fetched over the Talos API.
func (m *Manager) Kubernetes(ctx context.Context) (kubernetes.Interface, error) {
	m.mu.Lock()
	kube := m.kube
	m.mu.Unlock()

	if kube != nil {
		return kube, nil
	}

	c, err := m.Talos(ctx)
	if err != nil {
		return nil, err
	}

	// No node in the context: the request goes to whichever control plane endpoint is reachable.
	kube, err = kubeclient.FromTalosClient(ctx, c)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.kube = kube
	m.mu.Unlock()

	return kube, nil
}

func (m *Manager) firstControlPlane() config.Node {
	return m.Cluster.ControlPlaneNodes()[0]
}

func (m *Manager) firstControlPlaneCtx(ctx context.Context) context.Context {
	return client.WithNode(ctx, m.firstControlPlane().Address)
}

// writeFile writes data to path, refusing to overwrite unless --force is set.
func (m *Manager) writeFile(path string, data []byte, perm os.FileMode) error {
	if err := m.checkWritable(path); err != nil {
		return err
	}

	return os.WriteFile(path, data, perm)
}

// checkWritable fails when path exists and --force is not set.
func (m *Manager) checkWritable(path string) error {
	if m.Options.Force {
		return nil
	}

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists, use --force to overwrite it", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// Confirm asks a yes/no question; "no" cancels the operation.
func (m *Manager) Confirm(question string) error {
	for {
		m.printf("%s (y/n): ", question)

		answer, err := m.readAnswer()
		if err != nil {
			return err
		}

		switch answer {
		case "y", "yes":
			return nil
		case "n", "no":
			return ErrCancelled
		default:
			m.printf("Invalid input. Please enter 'y' or 'n'.\n")
		}
	}
}

// ErrCancelled is returned when the user declines to continue.
var ErrCancelled = errors.New("operation cancelled by user")

func (m *Manager) readAnswer() (string, error) {
	line, err := m.In.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			m.printf("\n")

			return "", ErrCancelled
		}

		return "", err
	}

	return strings.ToLower(strings.TrimSpace(line)), nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
