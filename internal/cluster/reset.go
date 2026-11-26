package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/siderolabs/talos/cmd/talosctl/pkg/talos/action"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"

	"github.com/vchrisb/tanvil/internal/config"
)

// WipeModes maps the --wipe-mode values to the reset API, as in talosctl reset.
var WipeModes = map[string]machineapi.ResetRequest_WipeMode{
	"all":         machineapi.ResetRequest_ALL,
	"system-disk": machineapi.ResetRequest_SYSTEM_DISK,
	"user-disks":  machineapi.ResetRequest_USER_DISKS,
}

// ResetOptions are the talosctl reset flags tanvil supports.
type ResetOptions struct {
	// Graceful cordons and drains the node and leaves etcd first. It is turned off automatically
	// for the last control plane node, which cannot leave etcd.
	Graceful bool
	// Reboot reboots the node after the reset; otherwise it shuts down.
	Reboot bool
	// WipeMode is a key of WipeModes.
	WipeMode string
	// SystemLabelsToWipe wipes only these system partitions (e.g. STATE, EPHEMERAL), keeping the
	// installation, so the node boots into maintenance mode.
	SystemLabelsToWipe []string
	// UserDisksToWipe additionally wipes these devices.
	UserDisksToWipe []string
}

func (o ResetOptions) request(graceful bool) *machineapi.ResetRequest {
	req := &machineapi.ResetRequest{
		Graceful:        graceful,
		Reboot:          o.Reboot,
		Mode:            WipeModes[o.WipeMode],
		UserDisksToWipe: o.UserDisksToWipe,
	}

	for _, label := range o.SystemLabelsToWipe {
		req.SystemPartitionsToWipe = append(req.SystemPartitionsToWipe, &machineapi.ResetPartitionSpec{Label: label, Wipe: true})
	}

	return req
}

func (o ResetOptions) describe() string {
	what := "wipes " + o.WipeMode
	if len(o.SystemLabelsToWipe) > 0 {
		what = "wipes the system partitions " + strings.Join(o.SystemLabelsToWipe, ", ")
	}

	if len(o.UserDisksToWipe) > 0 {
		what += " and the disks " + strings.Join(o.UserDisksToWipe, ", ")
	}

	if o.Reboot {
		return what + ", then reboots"
	}

	return what + ", then shuts down"
}

// TALOS-SYNC: mirrors talosctl reset (cmd/talosctl/cmd/talos/reset.go). When bumping
// github.com/siderolabs/talos, diff that file between the old and new tag and port any changes.
// Reset resets the nodes one at a time, workers first, after asking for each (unless
// --assume-yes). Nodes already in maintenance mode are skipped. A graceful reset of the last
// control plane node would fail to leave etcd, so that node is reset without --graceful.
func (m *Manager) Reset(ctx context.Context, nodes []config.Node, opts ResetOptions) error {
	if _, ok := WipeModes[opts.WipeMode]; !ok {
		return fmt.Errorf("unknown wipe mode %q", opts.WipeMode)
	}

	// Workers first, so control planes keep serving while the workers go away.
	ordered := make([]config.Node, 0, len(nodes))
	for _, n := range nodes {
		if !n.IsControlPlane() {
			ordered = append(ordered, n)
		}
	}

	for _, n := range nodes {
		if n.IsControlPlane() {
			ordered = append(ordered, n)
		}
	}

	// Control planes that stay etcd members; a node leaves the set once it is reset.
	remaining := map[string]bool{}
	for _, n := range m.Cluster.ControlPlaneNodes() {
		remaining[n.Name] = true
	}

	for _, node := range ordered {
		health, err := m.liveHealth(ctx, node)
		if err != nil {
			return fmt.Errorf("%s (%s): reading the machine status (if the node was already reset, it no longer "+
				"answers at this address; reset the remaining nodes by name): %w", node.Name, node.Address, err)
		}

		if health.Stage == runtime.MachineStageMaintenance.String() {
			m.printf("==> %s (%s) is already in maintenance mode, skipping\n", node.Name, node.Address)

			delete(remaining, node.Name)

			continue
		}

		graceful := opts.Graceful
		if graceful && node.IsControlPlane() && len(remaining) == 1 && remaining[node.Name] {
			m.printf("%s is the last control plane node; it cannot leave etcd, so it is reset without --graceful\n", node.Name)

			graceful = false
		}

		summary := fmt.Sprintf("%s (%s): reset %s", node.Name, node.Address, opts.describe())
		if graceful {
			summary += " (graceful: drain and leave etcd first)"
		}

		if m.Options.DryRun {
			m.printf("Dry run: would %s\n", summary)

			delete(remaining, node.Name)

			continue
		}

		if !m.Options.AssumeYes {
			if err := m.Confirm(fmt.Sprintf("RESET %s? This erases the node", summary)); err != nil {
				return err
			}
		}

		m.printf("  -> Resetting %s...\n", node.Name)

		if err := m.resetNode(ctx, node, opts.request(graceful)); err != nil {
			return fmt.Errorf("resetting %s: %w", node.Name, err)
		}

		delete(remaining, node.Name)

		m.printf("%s is resetting; with --reboot it comes back in maintenance mode, possibly at a DHCP address\n", node.Name)
	}

	return nil
}

// nodeReset sends the reset request and waits until the node stops all its services, which is
// after a graceful reset has drained the node and left etcd. It does not wait for the node to
// come back: after a reset it no longer has its static address.
func (m *Manager) nodeReset(ctx context.Context, node config.Node, req *machineapi.ResetRequest) error {
	c, err := m.Talos(ctx)
	if err != nil {
		return err
	}

	return action.NewTracker(
		singleNode{c: c, node: node.Address},
		action.StopAllServicesEventFn,
		func(ctx context.Context, c *client.Client) (string, error) {
			resp, err := c.ResetGenericWithResponse(ctx, req)
			if err != nil {
				return "", err
			}

			if len(resp.GetMessages()) == 0 {
				return "", errors.New("reset returned no messages")
			}

			return resp.GetMessages()[0].GetActorId(), nil
		},
		action.WithTimeout(rebootTimeout),
	).Run(ctx)
}
