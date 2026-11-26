package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"

	"github.com/vchrisb/tanvil/internal/config"
)

// NodeHealth is the node's own view of its state (the MachineStatus resource).
type NodeHealth struct {
	// Stage is the machine stage, e.g. running, booting or maintenance.
	Stage string
	// Ready is true when every condition of the stage is met.
	Ready bool
	// Unmet lists the unmet conditions as "name: reason".
	Unmet []string
}

// nodeHealth reads the MachineStatus resource of a node.
func (m *Manager) nodeHealth(ctx context.Context, node config.Node) (NodeHealth, error) {
	c, err := m.Talos(ctx)
	if err != nil {
		return NodeHealth{}, err
	}

	status, err := safe.StateGetByID[*runtime.MachineStatus](client.WithNode(ctx, node.Address), c.COSI, runtime.MachineStatusID)
	if err != nil {
		return NodeHealth{}, err
	}

	spec := status.TypedSpec()
	health := NodeHealth{Stage: spec.Stage.String(), Ready: spec.Status.Ready}

	for _, c := range spec.Status.UnmetConditions {
		health.Unmet = append(health.Unmet, fmt.Sprintf("%s: %s", c.Name, c.Reason))
	}

	return health, nil
}

// healthProblem describes why a node must not be changed, or returns "" when it may.
func healthProblem(h NodeHealth, allowNotReady bool) string {
	switch h.Stage {
	case runtime.MachineStageRunning.String(), runtime.MachineStageBooting.String():
	default:
		return fmt.Sprintf("is in stage %q, expected running or booting", h.Stage)
	}

	if h.Ready || allowNotReady {
		return ""
	}

	if len(h.Unmet) == 0 {
		return "is not ready"
	}

	return "is not ready: " + strings.Join(h.Unmet, "; ")
}

// EnsureNodesHealthy checks every node before anything is changed: each must be reachable, in
// the running or booting stage and, unless --allow-not-ready is set, ready. All problems are
// reported together.
func (m *Manager) EnsureNodesHealthy(ctx context.Context, nodes []config.Node) error {
	var errs []error

	for _, node := range nodes {
		health, err := m.liveHealth(ctx, node)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s (%s): reading the machine status: %w", node.Name, node.Address, err))

			continue
		}

		if problem := healthProblem(health, m.Options.AllowNotReady); problem != "" {
			errs = append(errs, fmt.Errorf("%s (%s) %s", node.Name, node.Address, problem))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("node pre-flight check failed (--allow-not-ready ignores unmet conditions): %w", errors.Join(errs...))
	}

	return nil
}
