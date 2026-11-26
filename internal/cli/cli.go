// Package cli wires the cluster operations to cobra commands.
package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/vchrisb/tanvil/internal/cluster"
	"github.com/vchrisb/tanvil/internal/config"
)

// Version is set at build time with -ldflags "-X github.com/vchrisb/tanvil/internal/cli.Version=...".
var Version = "dev"

type rootFlags struct {
	configFile string
	opts       cluster.Options
}

// NewRootCommand builds the tanvil command tree.
func NewRootCommand() *cobra.Command {
	flags := &rootFlags{}

	root := &cobra.Command{
		Use:           "tanvil",
		Short:         "Manage a Talos Linux cluster described by tanvil.yaml",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&flags.configFile, "config", "c", "tanvil.yaml", "cluster definition file")
	pf.BoolVar(&flags.opts.DryRun, "dry-run", false, "show what would change without changing the cluster")
	pf.BoolVar(&flags.opts.Redact, "redact", true,
		"replace private keys, tokens and encryption secrets in the output (--redact=false to show them)")
	pf.BoolVar(&flags.opts.Force, "force", false, "overwrite existing output files; with upgrade, also upgrade nodes that are already up to date")

	root.AddCommand(
		genSecretsCmd(flags),
		genConfigCmd(flags),
		talosconfigCmd(flags),
		kubeconfigCmd(flags),
		updateCmd(flags),
		upgradeCmd(flags),
		upgradeK8sCmd(flags),
		diffCmd(flags),
		resetCmd(flags),
		bootstrapCmd(flags),
		healthCmd(flags),
		validateCmd(flags),
	)

	return root
}

// run loads the definition and calls fn with a Manager that is closed afterwards.
func (f *rootFlags) run(cmd *cobra.Command, fn func(context.Context, *cluster.Manager) error) error {
	def, err := config.Load(f.configFile)
	if err != nil {
		return err
	}

	m := cluster.New(def, f.opts)
	m.Out = cmd.OutOrStdout()

	defer m.Close() //nolint:errcheck

	cmd.Printf("Cluster %s (Talos %s, Kubernetes %s) from %s\n\n",
		def.Metadata.Name, def.Spec.Versions.Talos, def.Spec.Versions.Kubernetes, f.configFile)

	return fn(cmd.Context(), m)
}

// selectNodes returns the named node, or all nodes control planes first when no name is given.
func selectNodes(def *config.TalosCluster, args []string) ([]config.Node, error) {
	if len(args) == 0 {
		return def.OrderedNodes(), nil
	}

	node, err := def.Node(args[0])
	if err != nil {
		return nil, err
	}

	return []config.Node{node}, nil
}

func genSecretsCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "gensecrets",
		Short: "Generate the cluster secrets bundle at spec.secretsFile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd, func(_ context.Context, m *cluster.Manager) error {
				return m.GenSecrets()
			})
		},
	}
}

func genConfigCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "genconfig [node]",
		Short: "Write <node>.yaml machine configs for review or export (update renders its own)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				nodes, err := selectNodes(m.Cluster, args)
				if err != nil {
					return err
				}

				if err := m.GenConfig(ctx, nodes); err != nil {
					return err
				}

				cmd.Println("Configuration generation complete!")

				return nil
			})
		},
	}
}

func talosconfigCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "talosconfig",
		Short: "Write a talosconfig for talosctl (tanvil itself does not need it)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd, func(_ context.Context, m *cluster.Manager) error {
				return m.WriteTalosconfig()
			})
		},
	}
}

func kubeconfigCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "kubeconfig",
		Short: "Write the cluster admin kubeconfig to ./kubeconfig (--force to overwrite)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				return m.WriteKubeconfig(ctx)
			})
		},
	}
}

func addPodCheckFlags(cmd *cobra.Command, f *rootFlags) {
	cmd.Flags().BoolVar(&f.opts.SkipPodCheck, "skip-pod-check", false, "skip the pod readiness checks before and after each node change")
	cmd.Flags().BoolVar(&f.opts.AllowNotReady, "allow-not-ready", false,
		"change nodes even when Talos reports unmet readiness conditions")
}

func updateCmd(f *rootFlags) *cobra.Command {
	var maintenance string

	cmd := &cobra.Command{
		Use:   "update [node]",
		Short: "Render the machine configs from the definition and apply them to all nodes or one node",
		Long: `Render the machine configs from the definition and apply them to all nodes or one node.

Before any node is changed, every config is rendered and validated, every node must be running
and ready (see --allow-not-ready), and every node compares its running config with the new one
in a dry run. Nodes without changes are skipped. For the others the diff is shown and confirmed
(see --assume-yes) before the config is applied and the pod check runs.

With --dry-run only the diffs are shown; the exit status is 2 when any node would change.
--maintenance applies directly to a fresh node in maintenance mode.`,
		Example: `  tanvil update --dry-run
  tanvil update cp-01
  tanvil update cp-01 --maintenance 10.0.0.99 --skip-pod-check`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if maintenance != "" && len(args) == 0 {
				return fmt.Errorf("--maintenance requires a node name")
			}

			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				nodes, err := selectNodes(m.Cluster, args)
				if err != nil {
					return err
				}

				if err := m.Update(ctx, nodes, maintenance); err != nil {
					return err
				}

				cmd.Println("Update complete!")

				return nil
			})
		},
	}

	cmd.Flags().BoolVarP(&f.opts.AssumeYes, "assume-yes", "y", false, "apply without asking for each changed node")
	cmd.Flags().StringVar(&maintenance, "maintenance", "",
		"apply insecurely to an unconfigured node in maintenance mode at this address")
	addPodCheckFlags(cmd, f)

	return cmd
}

func upgradeCmd(f *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade [node]",
		Short: "Upgrade Talos on all nodes (one at a time) or one node",
		Long: `Upgrade Talos on all nodes (one at a time, control planes first) or one node.

Nodes that already run spec.versions.talos with the configured schematic are skipped, so an
interrupted rolling upgrade can simply be run again. --force upgrades them anyway.`,
		Example: `  tanvil upgrade --dry-run
  tanvil upgrade worker-01
  tanvil upgrade --auto-continue-on-ready`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.opts.DrainTimeout <= 0 {
				return fmt.Errorf("--drain-timeout must be positive, got %s", f.opts.DrainTimeout)
			}

			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				nodes, err := selectNodes(m.Cluster, args)
				if err != nil {
					return err
				}

				if err := m.Upgrade(ctx, nodes); err != nil {
					return err
				}

				if f.opts.DryRun {
					cmd.Println("Dry run complete; no nodes were upgraded.")
				} else {
					cmd.Println("Upgrade complete!")
				}

				return nil
			})
		},
	}

	cmd.Flags().BoolVarP(&f.opts.AssumeYes, "assume-yes", "y", false, "do not ask before upgrading the next node")
	cmd.Flags().BoolVar(&f.opts.AutoContinueOnReady, "auto-continue-on-ready", false,
		"continue to the next node without asking when the post-upgrade pod check passed")
	cmd.Flags().BoolVar(&f.opts.NoDrain, "no-drain", false, "do not cordon and drain the node before rebooting it")
	cmd.Flags().DurationVar(&f.opts.DrainTimeout, "drain-timeout", 5*time.Minute,
		"maximum time to wait for the pods to be evicted (and, with --delete-if-eviction-fails, deleted)")
	cmd.Flags().BoolVar(&f.opts.DeleteIfEvictionFails, "delete-if-eviction-fails", false,
		"when the drain fails, e.g. because a PodDisruptionBudget blocks an eviction, delete the remaining pods instead")
	addPodCheckFlags(cmd, f)

	return cmd
}

func upgradeK8sCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade-k8s",
		Short: "Upgrade Kubernetes to spec.versions.kubernetes and check cluster health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				if err := m.UpgradeKubernetes(ctx); err != nil {
					return err
				}

				if f.opts.DryRun {
					return nil
				}

				if err := m.Health(ctx); err != nil {
					return err
				}

				cmd.Println("Kubernetes upgrade complete!")

				return nil
			})
		},
	}
}

func bootstrapCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap etcd on the first control plane node and write ./kubeconfig",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				// Refuse before bootstrapping rather than after: the kubeconfig is written last.
				if err := m.CheckKubeconfigWritable(); err != nil {
					return err
				}

				if err := m.Bootstrap(ctx); err != nil {
					return err
				}

				if err := m.WriteKubeconfig(ctx); err != nil {
					return err
				}

				cmd.Println("Bootstrap complete!")

				return nil
			})
		},
	}
}

func healthCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Run the Talos cluster health check against the configured nodes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				return m.Health(ctx)
			})
		},
	}
}

func diffCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "diff [node]",
		Short: "Show where the running cluster differs from the definition",
		Long: `Compare the running cluster with the definition, for all nodes or one node:

  - the Talos version and Image Factory schematic each node actually booted
  - each node's running machine config against a config freshly rendered from the definition
    (an apply-config dry run, so nothing is changed)
  - the cluster's Kubernetes version

Exits with status 1 when anything differs, so it can be used in scripts and CI.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				nodes, err := selectNodes(m.Cluster, args)
				if err != nil {
					return err
				}

				return m.Diff(ctx, nodes)
			})
		},
	}
}

func resetCmd(f *rootFlags) *cobra.Command {
	opts := cluster.ResetOptions{}

	cmd := &cobra.Command{
		Use:   "reset [node]",
		Short: "Reset nodes to their initial state (DESTRUCTIVE)",
		Long: `Reset all nodes or one node to their initial state, one at a time, workers first.

This erases the node: by default the whole system disk is wiped and the node reboots, coming back
in maintenance mode (from boot media or PXE) where 'update <node> --maintenance <ip>' reinstalls
it. Wiping only --system-labels-to-wipe STATE,EPHEMERAL keeps Talos installed, so the node boots
into maintenance mode from disk.

Every node is confirmed before it is reset (see --assume-yes). A graceful reset drains the node
and leaves etcd first; the last control plane node is always reset without --graceful, because
it cannot leave etcd. Nodes already in maintenance mode are skipped.`,
		Example: `  tanvil reset worker-01 --dry-run
  tanvil reset worker-01 --system-labels-to-wipe STATE,EPHEMERAL
  tanvil reset --reboot=false        # tear down the whole cluster and power off`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, ok := cluster.WipeModes[opts.WipeMode]; !ok {
				return fmt.Errorf("--wipe-mode must be all, system-disk or user-disks, got %q", opts.WipeMode)
			}

			return f.run(cmd, func(ctx context.Context, m *cluster.Manager) error {
				nodes, err := selectNodes(m.Cluster, args)
				if err != nil {
					return err
				}

				return m.Reset(ctx, nodes, opts)
			})
		},
	}

	cmd.Flags().BoolVar(&opts.Graceful, "graceful", true, "cordon and drain the node and leave etcd before resetting")
	cmd.Flags().BoolVar(&opts.Reboot, "reboot", true, "reboot after the reset; --reboot=false shuts the node down")
	cmd.Flags().StringVar(&opts.WipeMode, "wipe-mode", "all", "what to wipe: all, system-disk or user-disks")
	cmd.Flags().StringSliceVar(&opts.SystemLabelsToWipe, "system-labels-to-wipe", nil,
		"wipe only these system partitions (e.g. STATE,EPHEMERAL) and keep the installation")
	cmd.Flags().StringSliceVar(&opts.UserDisksToWipe, "user-disks-to-wipe", nil, "also wipe these devices")
	cmd.Flags().BoolVarP(&f.opts.AssumeYes, "assume-yes", "y", false, "reset without asking for each node")

	return cmd
}

func validateCmd(f *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the cluster definition without contacting the cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := config.Load(f.configFile); err != nil {
				return err
			}

			cmd.Printf("%s is valid\n", f.configFile)

			return nil
		},
	}
}
