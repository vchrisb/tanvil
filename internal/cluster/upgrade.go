package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/siderolabs/talos/cmd/talosctl/pkg/talos/action"
	"github.com/siderolabs/talos/cmd/talosctl/pkg/talos/nodedrain"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/reporter"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"

	"github.com/vchrisb/tanvil/internal/config"
)

// rebootTimeout bounds the wait for a node to come back after its upgrade reboot.
const rebootTimeout = 30 * time.Minute

// systemContainerd is the containerd instance talosctl upgrade uses by default.
var systemContainerd = &common.ContainerdInstance{
	Driver:    common.ContainerDriver_CRI,
	Namespace: common.ContainerdNamespace_NS_SYSTEM,
}

// TALOS-SYNC: mirrors talosctl upgrade (cmd/talosctl/cmd/talos/upgrade.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// deleteDrain also copies the drain settings of go-kubernetes nodedrain.Drain; re-check them
// when github.com/siderolabs/go-kubernetes changes.
// Upgrade upgrades the nodes one at a time, control planes first, then runs a health check
// when more than one node was upgraded.
func (m *Manager) Upgrade(ctx context.Context, nodes []config.Node) error {
	image, err := m.InstallerImage(ctx)
	if err != nil {
		return err
	}

	nodes, err = m.pendingUpgrades(ctx, nodes)
	if err != nil {
		return err
	}

	if len(nodes) == 0 {
		m.printf("All selected nodes are up to date.\n")

		return nil
	}

	if err := m.EnsureNodesHealthy(ctx, nodes); err != nil {
		return err
	}

	for i, node := range nodes {
		passed, err := m.upgradeNode(ctx, node, image)
		if err != nil {
			return err
		}

		last := i == len(nodes)-1
		if last || m.Options.DryRun || m.Options.AssumeYes || (m.Options.AutoContinueOnReady && passed) {
			continue
		}

		if err := m.Confirm(fmt.Sprintf("Continue with %s?", nodes[i+1].Name)); err != nil {
			return err
		}
	}

	if m.Options.DryRun || len(nodes) < 2 {
		return nil
	}

	return m.Health(ctx)
}

// upgradeNode returns whether the post-upgrade pod check passed.
func (m *Manager) upgradeNode(ctx context.Context, node config.Node, image string) (bool, error) {
	if err := m.EnsurePodsReady(ctx, "upgrading", node.Name); err != nil {
		return false, err
	}

	if m.Options.DryRun {
		m.printf("Dry run: would upgrade %s (%s) to %s\n", node.Name, node.Address, image)

		return false, nil
	}

	m.printf("  -> Upgrading %s (%s) to %s...\n", node.Name, node.Address, image)

	c, err := m.Talos(ctx)
	if err != nil {
		return false, err
	}

	nodeCtx := client.WithNode(ctx, node.Address)

	if err := m.pullImage(nodeCtx, c, image); err != nil {
		return false, fmt.Errorf("pulling %s on %s: %w", image, node.Name, err)
	}

	if err := m.installImage(nodeCtx, c, image); err != nil {
		return false, fmt.Errorf("upgrading %s: %w", node.Name, err)
	}

	k8sName, err := m.drain(ctx, nodeCtx, c, node)
	if err != nil {
		return false, err
	}

	m.printf("Rebooting %s...\n", node.Name)

	rebootErr := action.NewTracker(
		singleNode{c: c, node: node.Address},
		action.MachineReadyEventFn,
		func(ctx context.Context, c *client.Client) (string, error) {
			resp, err := c.RebootWithResponse(ctx)
			if err != nil {
				return "", err
			}

			if len(resp.GetMessages()) == 0 {
				return "", errors.New("reboot returned no messages")
			}

			return resp.GetMessages()[0].GetActorId(), nil
		},
		action.WithPostCheck(action.BootIDChangedPostCheckFn),
		action.WithTimeout(rebootTimeout),
	).Run(ctx)
	if rebootErr != nil {
		rebootErr = fmt.Errorf("rebooting %s: %w", node.Name, rebootErr)
	}

	// Uncordon even after a failed reboot wait, like talosctl does.
	if err := m.uncordon(ctx, k8sName); err != nil {
		return false, errors.Join(rebootErr, err)
	}

	if rebootErr != nil {
		return false, rebootErr
	}

	return m.WaitForPods(ctx, "upgrading", node.Name)
}

func (m *Manager) pullImage(ctx context.Context, c *client.Client, image string) error {
	stream, err := c.ImageClient.Pull(ctx, &machineapi.ImageServicePullRequest{
		Containerd: systemContainerd,
		ImageRef:   image,
	})
	if err != nil {
		return err
	}

	m.printf("Pulling %s...\n", image)

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		if name := resp.GetName(); name != "" {
			m.printf("Pulled %s\n", name)
		}
	}
}

func (m *Manager) installImage(ctx context.Context, c *client.Client, image string) error {
	stream, err := c.LifecycleClient.Upgrade(ctx, &machineapi.LifecycleServiceUpgradeRequest{
		Containerd: systemContainerd,
		Source:     &machineapi.InstallArtifactsSource{ImageName: image},
	})
	if err != nil {
		return err
	}

	exitCode := int32(-1)

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return err
		}

		progress := resp.GetProgress()

		switch p := progress.GetResponse().(type) {
		case *machineapi.LifecycleServiceInstallProgress_Message:
			m.printf("  %s\n", p.Message)
		case *machineapi.LifecycleServiceInstallProgress_ExitCode:
			exitCode = p.ExitCode
		}
	}

	switch exitCode {
	case 0:
		m.printf("Installed %s\n", image)

		return nil
	case -1:
		return errors.New("upgrade stream ended without an exit code")
	default:
		return fmt.Errorf("installer exited with code %d", exitCode)
	}
}

// drain cordons and drains the node and returns its Kubernetes name; empty when draining is off.
func (m *Manager) drain(ctx, nodeCtx context.Context, c *client.Client, node config.Node) (string, error) {
	if m.Options.NoDrain {
		return "", nil
	}

	k8sName, err := nodedrain.GetKubernetesNodeName(nodeCtx, c)
	if err != nil {
		return "", fmt.Errorf("resolving Kubernetes node name of %s: %w", node.Name, err)
	}

	kube, err := m.Kubernetes(ctx)
	if err != nil {
		return "", err
	}

	m.printf("Draining %s...\n", k8sName)

	err = nodedrain.CordonAndDrain(ctx, kube, k8sName, nodedrain.Options{DrainTimeout: m.drainTimeout()}, m.report)
	if err != nil && m.Options.DeleteIfEvictionFails && ctx.Err() == nil {
		m.printf("Draining %s failed (%v); deleting the remaining pods, bypassing PodDisruptionBudgets...\n", k8sName, err)

		err = m.deleteDrain(ctx, kube, k8sName)
	}

	if err != nil {
		// Leave the node schedulable if the drain gave up before the reboot.
		return "", errors.Join(fmt.Errorf("draining %s: %w", k8sName, err), m.uncordon(ctx, k8sName))
	}

	return k8sName, nil
}

func (m *Manager) uncordon(ctx context.Context, k8sName string) error {
	if k8sName == "" {
		return nil
	}

	kube, err := m.Kubernetes(ctx)
	if err != nil {
		return err
	}

	m.printf("Waiting for %s to become Ready...\n", k8sName)

	if err := nodedrain.WaitForNodeReady(ctx, kube, k8sName, rebootTimeout); err != nil {
		return fmt.Errorf("waiting for %s to become Ready: %w", k8sName, err)
	}

	if err := nodedrain.Uncordon(ctx, kube, k8sName, m.report); err != nil {
		return fmt.Errorf("uncordoning %s: %w", k8sName, err)
	}

	return nil
}

func (m *Manager) report(u reporter.Update) {
	if u.Message != "" {
		m.printf("  %s\n", u.Message)
	}
}

// singleNode adapts the shared client to the tracker's client factory for one node.
type singleNode struct {
	c    *client.Client
	node string
}

func (s singleNode) BuildClient(ctx context.Context, node string) (context.Context, *client.Client, error) {
	return client.WithNode(ctx, node), s.c, nil
}

func (s singleNode) Nodes() []string { return []string{s.node} }

func (m *Manager) drainTimeout() time.Duration {
	if m.Options.DrainTimeout > 0 {
		return m.Options.DrainTimeout
	}

	return nodedrain.DefaultDrainTimeout
}

// deleteDrain drains the already cordoned node by deleting its pods instead of evicting them, so
// PodDisruptionBudgets no longer block it (kubectl drain --disable-eviction). Apart from that it
// uses the settings of talosctl's drain (go-kubernetes nodedrain.Drain), and the same timeout.
func (m *Manager) deleteDrain(ctx context.Context, kube kubernetes.Interface, k8sName string) error {
	timeout := m.drainTimeout()

	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	helper := &drain.Helper{
		Ctx:                 drainCtx,
		Client:              kube,
		Force:               true,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		DisableEviction:     true,
		Timeout:             timeout,
		Out:                 io.Discard,
		ErrOut:              io.Discard,
		OnPodDeletionOrEvictionFinished: func(pod *corev1.Pod, _ bool, err error) {
			if err != nil {
				m.printf("%s: failed to delete pod %s/%s: %v\n", k8sName, pod.Namespace, pod.Name, err)

				return
			}

			m.printf("%s: deleted pod %s/%s\n", k8sName, pod.Namespace, pod.Name)
		},
	}

	if err := drain.RunNodeDrain(helper, k8sName); err != nil {
		return fmt.Errorf("deleting pods on %s: %w", k8sName, err)
	}

	return nil
}
