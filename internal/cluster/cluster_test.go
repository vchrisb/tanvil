package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vchrisb/tanvil/internal/config"
	"github.com/vchrisb/tanvil/internal/factory"
)

// testSchematicID is the Image Factory ID of the schematic in clusterYAML (customization: {}).
const testSchematicID = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"

const clusterYAML = `
apiVersion: tanvil.dev/v1alpha1
kind: TalosCluster
metadata:
  name: test
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
    wipe: true
  configPatches:
    all: [patches/common.yaml]
    worker: [patches/worker.yaml]
  network:
    interface: ens3
    prefix: 24
    gateway: 10.0.0.1
  nodes:
    - {name: cp-01, role: controlplane, address: 10.0.0.11}
    - name: worker-01
      role: worker
      address: 10.0.0.21
      patches:
        - apiVersion: v1alpha1
          kind: UnattendedInstallConfig
          provisioning:
            diskSelector:
              match: disk.dev_path == "/dev/nvme0n1"
        - machine: {sysctls: {vm.nr_hugepages: "64"}}
`

func newTestManager(t *testing.T, opts Options) (*Manager, *bytes.Buffer) {
	t.Helper()

	dir := t.TempDir()
	write := func(name, content string) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("tanvil.yaml", clusterYAML)
	write("patches/common.yaml", "machine:\n  time:\n    servers: [time.example.com]\n")
	write("patches/worker.yaml", "apiVersion: v1alpha1\nkind: KubeNodeConfig\nlabels:\n  role: worker\n")

	def, err := config.Load(filepath.Join(dir, "tanvil.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// The schematic ID is computed locally; the factory is only contacted with SubmitToFactory.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !opts.SubmitToFactory {
			t.Error("image factory contacted without --submit-to-factory")
		}

		w.Write([]byte(`{"id":"` + testSchematicID + `","schematic":"customization: {}\n"}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	out := &bytes.Buffer{}
	m := New(def, opts)
	m.Factory = &factory.Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	m.Out = out
	m.In = bufio.NewReader(strings.NewReader(""))
	m.liveHealth = func(context.Context, config.Node) (NodeHealth, error) {
		return NodeHealth{Stage: "running", Ready: true}, nil
	}

	return m, out
}

func TestSchematicIDSubmitToFactory(t *testing.T) {
	m, out := newTestManager(t, Options{SubmitToFactory: true})

	id, err := m.SchematicID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if id != testSchematicID || !strings.Contains(out.String(), "submitted to") {
		t.Errorf("id = %s, output:\n%s", id, out)
	}
}

func TestSchematicIDFactoryMismatch(t *testing.T) {
	m, _ := newTestManager(t, Options{SubmitToFactory: true})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"other","schematic":"customization: {}\n"}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	m.Factory = &factory.Client{BaseURL: srv.URL, HTTPClient: srv.Client()}

	if _, err := m.SchematicID(context.Background()); err == nil || !strings.Contains(err.Error(), "update tanvil") {
		t.Fatalf("expected ID mismatch error, got %v", err)
	}
}

func TestGenSecretsRefusesOverwrite(t *testing.T) {
	m, _ := newTestManager(t, Options{})

	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	if err := m.GenSecrets(); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected overwrite refusal, got %v", err)
	}

	m.Options.Force = true
	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeConfig(t *testing.T) {
	m, _ := newTestManager(t, Options{})
	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	for _, tc := range []struct {
		node        string
		contains    []string
		notContains []string
	}{
		{
			node: "cp-01",
			contains: []string{
				"type: controlplane",
				"kind: UnattendedInstallConfig",
				"image: factory.talos.dev/metal-installer/" + testSchematicID + ":v1.14.7",
				`match: disk.dev_path == "/dev/sda"`,
				"wipe: true",
				"endpoint: https://k8s.example.com:6443",
				"time.example.com",
				"kind: HostnameConfig",
				"hostname: cp-01",
				"kind: LinkConfig",
				"address: 10.0.0.11/24",
				"gateway: 10.0.0.1",
			},
			notContains: []string{"role: worker", "nvme0n1", "nr_hugepages"},
		},
		{
			node: "worker-01",
			contains: []string{
				"type: worker", "hostname: worker-01", "address: 10.0.0.21/24", "role: worker", "time.example.com",
				// node patches override the generated install document and patch the v1alpha1 document
				`match: disk.dev_path == "/dev/nvme0n1"`,
				"image: factory.talos.dev/metal-installer/" + testSchematicID + ":v1.14.7",
				"vm.nr_hugepages: \"64\"",
			},
			notContains: []string{`match: disk.dev_path == "/dev/sda"`},
		},
	} {
		t.Run(tc.node, func(t *testing.T) {
			node, err := m.Cluster.Node(tc.node)
			if err != nil {
				t.Fatal(err)
			}

			data, err := m.NodeConfig(ctx, node)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := configloader.NewFromBytes(data); err != nil {
				t.Fatalf("generated config does not load: %v", err)
			}

			for _, s := range tc.contains {
				if !bytes.Contains(data, []byte(s)) {
					t.Errorf("config does not contain %q", s)
				}
			}

			for _, s := range tc.notContains {
				if bytes.Contains(data, []byte(s)) {
					t.Errorf("config unexpectedly contains %q", s)
				}
			}
		})
	}
}

func TestTalosconfigUsesAllControlPlanes(t *testing.T) {
	m, _ := newTestManager(t, Options{})
	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())

	if err := m.WriteTalosconfig(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile("talosconfig")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(data, []byte("10.0.0.11")) || bytes.Contains(data, []byte("10.0.0.21")) {
		t.Errorf("talosconfig endpoints are wrong:\n%s", data)
	}
}

func pod(ns, name string, phase corev1.PodPhase, ready ...bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.PodStatus{Phase: phase},
	}

	for i, r := range ready {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: string(rune('a' + i))})
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Ready: r})
	}

	return p
}

func TestUnreadyPods(t *testing.T) {
	crash := pod("kube-system", "crash", corev1.PodRunning, false)
	crash.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}

	terminating := pod("default", "terminating", corev1.PodRunning, true)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	kube := fake.NewClientset(
		pod("default", "ok", corev1.PodRunning, true, true),
		pod("default", "done", corev1.PodSucceeded, false),
		pod("default", "partial", corev1.PodRunning, true, false),
		pod("default", "pending", corev1.PodPending),
		crash,
		terminating,
	)

	got, err := UnreadyPods(context.Background(), kube)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"default/partial (ready=1/2, status=Running)":            true,
		"default/pending (ready=0/0, status=Pending)":            true,
		"kube-system/crash (ready=0/1, status=CrashLoopBackOff)": true,
		"default/terminating (ready=1/1, status=Terminating)":    true,
	}

	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}

	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entry %q in %v", g, got)
		}
	}
}

func TestEnsurePodsReady(t *testing.T) {
	for name, tc := range map[string]struct {
		input   string
		pods    []*corev1.Pod
		wantErr error
		wantOut string
	}{
		"ready":    {pods: []*corev1.Pod{pod("a", "p", corev1.PodRunning, true)}, wantOut: "check passed"},
		"continue": {input: "c\n", pods: []*corev1.Pod{pod("a", "p", corev1.PodPending)}, wantOut: "Continuing despite"},
		"cancel":   {input: "q\n", pods: []*corev1.Pod{pod("a", "p", corev1.PodPending)}, wantErr: ErrCancelled},
		"eof":      {pods: []*corev1.Pod{pod("a", "p", corev1.PodPending)}, wantErr: ErrCancelled},
		"retry":    {input: "x?\nr\nc\n", pods: []*corev1.Pod{pod("a", "p", corev1.PodPending)}, wantOut: "Retrying pod check"},
	} {
		t.Run(name, func(t *testing.T) {
			m, out := newTestManager(t, Options{})
			m.In = bufio.NewReader(strings.NewReader(tc.input))

			kube := fake.NewClientset()
			for _, p := range tc.pods {
				kube.Tracker().Add(p) //nolint:errcheck
			}

			m.kube = kube

			err := m.EnsurePodsReady(context.Background(), "updating", "cp-01")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output %q does not contain %q", out.String(), tc.wantOut)
			}
		})
	}
}

func TestSkipPodCheckDoesNotNeedCluster(t *testing.T) {
	m, _ := newTestManager(t, Options{SkipPodCheck: true})

	if err := m.EnsurePodsReady(context.Background(), "updating", "cp-01"); err != nil {
		t.Fatal(err)
	}

	if passed, err := m.WaitForPods(context.Background(), "updating", "cp-01"); err != nil || passed {
		t.Fatalf("WaitForPods = %v, %v", passed, err)
	}
}

// fakeClock makes WaitForPods run instantly.
func fakeClock(m *Manager) {
	now := time.Unix(0, 0)
	m.now = func() time.Time { return now }
	m.sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)

		return nil
	}
}

func TestWaitForPodsNeedsStableChecks(t *testing.T) {
	m, out := newTestManager(t, Options{})
	fakeClock(m)

	m.kube = fake.NewClientset(pod("a", "p", corev1.PodRunning, true))

	passed, err := m.WaitForPods(context.Background(), "upgrading", "cp-01")
	if err != nil || !passed {
		t.Fatalf("WaitForPods = %v, %v", passed, err)
	}

	if !strings.Contains(out.String(), "confirming stability") {
		t.Errorf("expected a stability confirmation, got %q", out.String())
	}
}

func TestWaitForPodsTimeoutThenContinue(t *testing.T) {
	m, out := newTestManager(t, Options{})
	fakeClock(m)
	m.In = bufio.NewReader(strings.NewReader("r\nc\n"))
	m.kube = fake.NewClientset(pod("a", "p", corev1.PodPending))

	passed, err := m.WaitForPods(context.Background(), "upgrading", "cp-01")
	if err != nil || passed {
		t.Fatalf("WaitForPods = %v, %v", passed, err)
	}

	if strings.Count(out.String(), "still failing after 5m0s") != 2 {
		t.Errorf("expected two timeouts (retry, then continue), got %q", out.String())
	}
}

func TestConfirm(t *testing.T) {
	m, _ := newTestManager(t, Options{})

	m.In = bufio.NewReader(strings.NewReader("maybe\nyes\n"))
	if err := m.Confirm("Continue?"); err != nil {
		t.Fatal(err)
	}

	m.In = bufio.NewReader(strings.NewReader("n\n"))
	if err := m.Confirm("Continue?"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradeDryRunDoesNotContactNodes(t *testing.T) {
	m, out := newTestManager(t, Options{DryRun: true, SkipPodCheck: true})
	m.liveVersion = fakeVersions(map[string]NodeVersion{})

	if err := m.Upgrade(context.Background(), m.Cluster.OrderedNodes()); err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{
		"Dry run: would upgrade cp-01 (10.0.0.11) to factory.talos.dev/metal-installer/" + testSchematicID + ":v1.14.7",
		"Dry run: would upgrade worker-01 (10.0.0.21)",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output %q does not contain %q", out.String(), s)
		}
	}

	if m.talos != nil {
		t.Error("dry run created a Talos client")
	}
}

// updateFixture returns a manager whose nodes report diff for worker-01 only, and the list of
// nodes a config was applied to.
func updateFixture(t *testing.T, opts Options, answers string) (*Manager, *bytes.Buffer, *[]string) {
	t.Helper()

	opts.SkipPodCheck = true
	m, out := newTestManager(t, opts)

	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir()) // no <node>.yaml files needed

	m.In = bufio.NewReader(strings.NewReader(answers))
	m.liveConfigDiff = func(_ context.Context, node config.Node, data []byte) (string, error) {
		if !strings.Contains(string(data), "hostname: "+node.Name) {
			t.Errorf("%s was compared with a config for another node", node.Name)
		}

		if node.Name == "worker-01" {
			return "-old\n+new", nil
		}

		return "", nil
	}

	applied := &[]string{}
	m.applyConfig = func(_ context.Context, node config.Node, _ string, _ []byte) error {
		*applied = append(*applied, node.Name)

		return nil
	}

	return m, out, applied
}

func TestUpdateAppliesOnlyChangedNodes(t *testing.T) {
	m, out, applied := updateFixture(t, Options{AssumeYes: true}, "")

	if err := m.Update(context.Background(), m.Cluster.OrderedNodes(), ""); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(*applied, ","); got != "worker-01" {
		t.Errorf("applied to %q, want worker-01 only", got)
	}

	for _, s := range []string{"==> cp-01 (10.0.0.11): no changes", "==> worker-01 (10.0.0.21):\n    -old\n    +new"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output %q does not contain %q", out.String(), s)
		}
	}
}

func TestUpdateDryRunReportsChanges(t *testing.T) {
	m, out, applied := updateFixture(t, Options{DryRun: true}, "")

	err := m.Update(context.Background(), m.Cluster.OrderedNodes(), "")
	if !errors.Is(err, ErrChanges) || !strings.Contains(err.Error(), "1 node(s)") {
		t.Fatalf("expected ErrChanges for 1 node, got %v", err)
	}

	if len(*applied) != 0 {
		t.Errorf("dry run applied to %v", *applied)
	}

	if !strings.Contains(out.String(), "+new") {
		t.Errorf("diff not shown: %q", out.String())
	}
}

func TestUpdateAsksBeforeApplying(t *testing.T) {
	m, _, applied := updateFixture(t, Options{}, "n\n")

	if err := m.Update(context.Background(), m.Cluster.OrderedNodes(), ""); !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got %v", err)
	}

	if len(*applied) != 0 {
		t.Errorf("applied to %v after answering no", *applied)
	}
}

func TestUpdateChecksNodeHealthFirst(t *testing.T) {
	m, _, applied := updateFixture(t, Options{AssumeYes: true}, "")
	compared := false
	diff := m.liveConfigDiff
	m.liveConfigDiff = func(ctx context.Context, node config.Node, data []byte) (string, error) {
		compared = true

		return diff(ctx, node, data)
	}
	m.liveHealth = func(_ context.Context, node config.Node) (NodeHealth, error) {
		if node.Name == "worker-01" {
			return NodeHealth{Stage: "running", Unmet: []string{"network: no addresses"}}, nil
		}

		return NodeHealth{Stage: "running", Ready: true}, nil
	}

	err := m.Update(context.Background(), m.Cluster.OrderedNodes(), "")
	if err == nil || !strings.Contains(err.Error(), "worker-01 (10.0.0.21) is not ready: network: no addresses") {
		t.Fatalf("expected a pre-flight error for worker-01, got %v", err)
	}

	if compared || len(*applied) != 0 {
		t.Errorf("nodes were contacted after a failed pre-flight check: compared=%v applied=%v", compared, *applied)
	}

	m.Options.AllowNotReady = true

	if err := m.Update(context.Background(), m.Cluster.OrderedNodes(), ""); err != nil {
		t.Fatalf("--allow-not-ready: %v", err)
	}
}

func TestHealthProblem(t *testing.T) {
	for _, tc := range []struct {
		health        NodeHealth
		allowNotReady bool
		want          string
	}{
		{NodeHealth{Stage: "running", Ready: true}, false, ""},
		{NodeHealth{Stage: "booting", Ready: true}, false, ""},
		{NodeHealth{Stage: "running", Unmet: []string{"a: b", "c: d"}}, false, "is not ready: a: b; c: d"},
		{NodeHealth{Stage: "running"}, true, ""},
		{NodeHealth{Stage: "maintenance", Ready: true}, true, `is in stage "maintenance", expected running or booting`},
		{NodeHealth{Stage: "upgrading", Ready: true}, false, `is in stage "upgrading"`},
	} {
		if got := healthProblem(tc.health, tc.allowNotReady); !strings.HasPrefix(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("healthProblem(%+v, %v) = %q, want %q", tc.health, tc.allowNotReady, got, tc.want)
		}
	}
}

func TestUpdateAppliesNothingWhenAConfigIsInvalid(t *testing.T) {
	m, _ := newTestManager(t, Options{SkipPodCheck: true})
	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(m.Cluster.Path("patches/worker.yaml"), []byte("machine:\n  network:\n    hostname: other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m.applyConfig = func(_ context.Context, node config.Node, _ string, _ []byte) error {
		t.Errorf("config applied to %s although worker-01 is invalid", node.Name)

		return nil
	}

	err := m.Update(context.Background(), m.Cluster.OrderedNodes(), "")
	if err == nil || !strings.Contains(err.Error(), "worker-01: invalid machine config") {
		t.Fatalf("expected a validation error for worker-01, got %v", err)
	}
}

// fakeVersions returns the given running versions; nodes not listed run v1.14.6 without schematic.
func fakeVersions(versions map[string]NodeVersion) func(context.Context, config.Node) (NodeVersion, error) {
	return func(_ context.Context, node config.Node) (NodeVersion, error) {
		if v, ok := versions[node.Name]; ok {
			return v, nil
		}

		return NodeVersion{Talos: "v1.14.6"}, nil
	}
}

func TestGenConfigWritesNothingWhenAConfigIsInvalid(t *testing.T) {
	m, _ := newTestManager(t, Options{})
	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	// The legacy hostname field conflicts with the generated HostnameConfig document.
	if err := os.WriteFile(m.Cluster.Path("patches/worker.yaml"), []byte("machine:\n  network:\n    hostname: other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())

	err := m.GenConfig(context.Background(), m.Cluster.OrderedNodes())
	if err == nil || !strings.Contains(err.Error(), "worker-01: invalid machine config") {
		t.Fatalf("expected a validation error for worker-01, got %v", err)
	}

	if _, err := os.Stat("cp-01.yaml"); !os.IsNotExist(err) {
		t.Errorf("cp-01.yaml was written although worker-01 is invalid: %v", err)
	}
}

func TestParseConfigDiff(t *testing.T) {
	for in, want := range map[string]string{
		"Dry run summary:\nNo reboot required.\n\nConfig diff:\n\nNo changes.": "",
		"Config diff:\n\n--- a\n+++ b\n@@ -1 +1 @@\n-x\n+y\n":                  "--- a\n+++ b\n@@ -1 +1 @@\n-x\n+y",
	} {
		if got := parseConfigDiff(in); got != want {
			t.Errorf("parseConfigDiff(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiff(t *testing.T) {
	m, out := newTestManager(t, Options{})
	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	m.liveVersion = fakeVersions(map[string]NodeVersion{
		"cp-01": {Talos: "v1.14.7", Schematic: testSchematicID},
	})
	m.liveConfigDiff = func(_ context.Context, node config.Node, data []byte) (string, error) {
		if !strings.Contains(string(data), "hostname: "+node.Name) {
			t.Errorf("%s was compared with a config for another node", node.Name)
		}

		if node.Name == "worker-01" {
			return "-old\n+new", nil
		}

		return "", nil
	}
	m.liveKubernetes = func(context.Context) (string, error) { return "1.35.0", nil }

	err := m.Diff(context.Background(), m.Cluster.OrderedNodes())
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("expected ErrDrift, got %v", err)
	}

	for _, s := range []string{
		"==> cp-01 (10.0.0.11): in sync",
		"==> worker-01 (10.0.0.21):\n    talos: running v1.14.6, want v1.14.7",
		"schematic: running none (not an Image Factory image), want " + testSchematicID,
		"    config:\n      -old\n      +new",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output %q does not contain %q", out.String(), s)
		}
	}

	m.liveVersion = fakeVersions(map[string]NodeVersion{
		"cp-01":     {Talos: "v1.14.7", Schematic: testSchematicID},
		"worker-01": {Talos: "v1.14.7", Schematic: testSchematicID},
	})
	m.liveConfigDiff = func(context.Context, config.Node, []byte) (string, error) { return "", nil }
	m.liveKubernetes = func(context.Context) (string, error) { return m.Cluster.Spec.Versions.Kubernetes, nil }

	if err := m.Diff(context.Background(), m.Cluster.OrderedNodes()); err != nil {
		t.Fatalf("expected no drift, got %v", err)
	}
}

func TestUpgradeSkipsUpToDateNodes(t *testing.T) {
	m, out := newTestManager(t, Options{DryRun: true, SkipPodCheck: true})
	m.liveVersion = fakeVersions(map[string]NodeVersion{
		"cp-01": {Talos: "v1.14.7", Schematic: testSchematicID},
	})

	if err := m.Upgrade(context.Background(), m.Cluster.OrderedNodes()); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "cp-01 already runs v1.14.7 with schematic "+testSchematicID+", skipping") {
		t.Errorf("cp-01 was not skipped: %q", out.String())
	}

	if strings.Contains(out.String(), "would upgrade cp-01") || !strings.Contains(out.String(), "would upgrade worker-01") {
		t.Errorf("wrong nodes upgraded: %q", out.String())
	}

	out.Reset()
	m.Options.Force = true

	if err := m.Upgrade(context.Background(), m.Cluster.OrderedNodes()); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "would upgrade cp-01") {
		t.Errorf("--force did not upgrade cp-01: %q", out.String())
	}
}

func TestWriteKubeconfigRefusesOverwrite(t *testing.T) {
	m, _ := newTestManager(t, Options{})
	t.Chdir(t.TempDir())

	if err := os.WriteFile("kubeconfig", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fails before contacting the cluster: there are no secrets to build a client from.
	if err := m.WriteKubeconfig(context.Background()); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected overwrite refusal, got %v", err)
	}

	if m.talos != nil {
		t.Error("a Talos client was created although the kubeconfig cannot be written")
	}
}

func TestUpgradeChecksNodeHealthFirst(t *testing.T) {
	m, out := newTestManager(t, Options{DryRun: true, SkipPodCheck: true})
	m.liveVersion = fakeVersions(map[string]NodeVersion{})
	m.liveHealth = func(context.Context, config.Node) (NodeHealth, error) {
		return NodeHealth{Stage: "booting", Unmet: []string{"time: not synchronized"}}, nil
	}

	err := m.Upgrade(context.Background(), m.Cluster.OrderedNodes())
	if err == nil || !strings.Contains(err.Error(), "cp-01 (10.0.0.11) is not ready: time: not synchronized") {
		t.Fatalf("expected a pre-flight error, got %v", err)
	}

	if strings.Contains(out.String(), "would upgrade") {
		t.Errorf("upgrade continued after a failed pre-flight check: %q", out.String())
	}
}

func TestOutputRedactsSecrets(t *testing.T) {
	m, out := newTestManager(t, Options{Redact: true})
	if err := m.GenSecrets(); err != nil {
		t.Fatal(err)
	}

	node, _ := m.Cluster.Node("cp-01")

	data, err := m.NodeConfig(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	m.printf("%s", data)

	bundle, err := m.secretsBundle()
	if err != nil {
		t.Fatal(err)
	}

	for name, secret := range map[string]string{
		"bootstrap token":   bundle.Secrets.BootstrapToken,
		"secretbox secret":  bundle.Secrets.SecretboxEncryptionSecret,
		"trustd token":      bundle.TrustdInfo.Token,
		"cluster secret":    bundle.Cluster.Secret,
		"etcd key":          string(bundle.Certs.Etcd.Key),
		"kubernetes CA key": string(bundle.Certs.K8s.Key),
		"OS CA key":         string(bundle.Certs.OS.Key),
	} {
		if secret == "" {
			continue
		}

		// The config embeds keys base64 encoded, tokens verbatim.
		for _, form := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret))} {
			if strings.Contains(string(data), form) && strings.Contains(out.String(), form) {
				t.Errorf("%s is printed unredacted", name)
			}
		}
	}

	if strings.Contains(out.String(), "PRIVATE KEY") {
		t.Errorf("a PEM private key is printed:\n%s", out.String())
	}

	for _, key := range [][]byte{bundle.Certs.Etcd.Key, bundle.Certs.K8s.Key, bundle.Certs.OS.Key, bundle.Certs.K8sServiceAccount.Key} {
		for _, line := range strings.Split(string(key), "\n") {
			if len(line) >= minSecretLength && !strings.HasPrefix(line, "-----") && strings.Contains(out.String(), line) {
				t.Errorf("private key line %q is printed", line)
			}
		}
	}

	if !strings.Contains(out.String(), "key: "+redacted) || !strings.Contains(out.String(), "token: "+redacted) {
		t.Errorf("expected redacted key and token fields in the output")
	}

	if !strings.Contains(out.String(), "crt: LS0t") {
		t.Errorf("certificates should not be redacted")
	}

	out.Reset()
	m.Options.Redact = false
	m.printf("%s", data)

	if strings.Contains(out.String(), redacted) {
		t.Error("--redact=false still redacts")
	}
}

func TestDeleteDrainBypassesEviction(t *testing.T) {
	m, out := newTestManager(t, Options{DrainTimeout: 10 * time.Second})
	p := pod("default", "web", corev1.PodRunning, true)
	p.Spec.NodeName = "worker-01"
	kube := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-01"}, Spec: corev1.NodeSpec{Unschedulable: true}},
		p,
	)

	if err := m.deleteDrain(context.Background(), kube, "worker-01"); err != nil {
		t.Fatal(err)
	}

	pods, err := kube.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(pods.Items) != 0 {
		t.Errorf("pods left after deleteDrain: %d", len(pods.Items))
	}

	for _, a := range kube.Actions() {
		if a.GetSubresource() == "eviction" {
			t.Errorf("deleteDrain used the eviction API: %v", a)
		}
	}

	if !strings.Contains(out.String(), "worker-01: deleted pod default/web") {
		t.Errorf("deletion not reported: %q", out.String())
	}
}

// resetFixture returns a manager with three control planes and a worker, and the list of reset
// requests sent, as "node graceful=<bool>".
func resetFixture(t *testing.T, opts Options, answers string) (*Manager, *bytes.Buffer, *[]string) {
	t.Helper()

	m, out := newTestManager(t, opts)
	m.Cluster.Spec.Nodes = append(m.Cluster.Spec.Nodes,
		config.Node{Name: "cp-02", Role: config.RoleControlPlane, Address: "10.0.0.12"},
	)
	m.In = bufio.NewReader(strings.NewReader(answers))

	sent := &[]string{}
	m.resetNode = func(_ context.Context, node config.Node, req *machineapi.ResetRequest) error {
		*sent = append(*sent, fmt.Sprintf("%s graceful=%v", node.Name, req.GetGraceful()))

		return nil
	}

	return m, out, sent
}

func TestResetWorkersFirstAndLastControlPlaneNotGraceful(t *testing.T) {
	m, out, sent := resetFixture(t, Options{AssumeYes: true}, "")

	err := m.Reset(context.Background(), m.Cluster.OrderedNodes(), ResetOptions{Graceful: true, Reboot: true, WipeMode: "all"})
	if err != nil {
		t.Fatal(err)
	}

	want := "worker-01 graceful=true,cp-01 graceful=true,cp-02 graceful=false"
	if got := strings.Join(*sent, ","); got != want {
		t.Errorf("reset order %q, want %q", got, want)
	}

	if !strings.Contains(out.String(), "cp-02 is the last control plane node") {
		t.Errorf("missing last control plane note: %q", out.String())
	}
}

func TestResetSingleControlPlaneStaysGraceful(t *testing.T) {
	m, _, sent := resetFixture(t, Options{AssumeYes: true}, "")
	node, _ := m.Cluster.Node("cp-01")

	if err := m.Reset(context.Background(), []config.Node{node}, ResetOptions{Graceful: true, WipeMode: "all"}); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(*sent, ","); got != "cp-01 graceful=true" {
		t.Errorf("sent %q; cp-02 still runs etcd, so cp-01 can leave gracefully", got)
	}
}

func TestResetAsksAndSkipsMaintenance(t *testing.T) {
	m, out, sent := resetFixture(t, Options{}, "n\n")
	m.liveHealth = func(_ context.Context, node config.Node) (NodeHealth, error) {
		if node.Name == "worker-01" {
			return NodeHealth{Stage: "maintenance"}, nil
		}

		return NodeHealth{Stage: "running", Ready: true}, nil
	}

	err := m.Reset(context.Background(), m.Cluster.OrderedNodes(), ResetOptions{Graceful: true, WipeMode: "all"})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got %v", err)
	}

	if len(*sent) != 0 {
		t.Errorf("reset sent after answering no: %v", *sent)
	}

	for _, s := range []string{"worker-01 (10.0.0.21) is already in maintenance mode, skipping", "RESET cp-01 (10.0.0.11): reset wipes all, then shuts down"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output %q does not contain %q", out.String(), s)
		}
	}
}

func TestResetDryRunAndRequest(t *testing.T) {
	m, out, sent := resetFixture(t, Options{DryRun: true}, "")
	opts := ResetOptions{Graceful: true, Reboot: true, WipeMode: "system-disk", SystemLabelsToWipe: []string{"STATE", "EPHEMERAL"}}

	if err := m.Reset(context.Background(), m.Cluster.OrderedNodes(), opts); err != nil {
		t.Fatal(err)
	}

	if len(*sent) != 0 {
		t.Errorf("dry run sent %v", *sent)
	}

	if !strings.Contains(out.String(), "Dry run: would worker-01 (10.0.0.21): reset wipes the system partitions STATE, EPHEMERAL, then reboots") {
		t.Errorf("unexpected dry-run output %q", out.String())
	}

	req := opts.request(false)
	if req.GetMode() != machineapi.ResetRequest_SYSTEM_DISK || !req.GetReboot() || req.GetGraceful() ||
		len(req.GetSystemPartitionsToWipe()) != 2 || req.GetSystemPartitionsToWipe()[0].GetLabel() != "STATE" {
		t.Errorf("unexpected request %v", req)
	}
}
