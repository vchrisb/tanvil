package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// podListTimeout bounds a single pod list request.
const podListTimeout = 60 * time.Second

// UnreadyPods lists every pod that has not succeeded and is not running with all containers ready.
func UnreadyPods(ctx context.Context, kube kubernetes.Interface) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, podListTimeout)
	defer cancel()

	pods, err := kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=" + string(corev1.PodSucceeded),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	var unready []string

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded {
			continue
		}

		ready, total := 0, len(pod.Spec.Containers)

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
		}

		status := podStatus(pod)
		if total > 0 && ready == total && status == string(corev1.PodRunning) {
			continue
		}

		unready = append(unready, fmt.Sprintf("%s/%s (ready=%d/%d, status=%s)", pod.Namespace, pod.Name, ready, total, status))
	}

	return unready, nil
}

// podStatus approximates the STATUS column of kubectl get pods.
func podStatus(pod *corev1.Pod) string {
	if pod.DeletionTimestamp != nil {
		return "Terminating"
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}

		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}

	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}

	return string(pod.Status.Phase)
}

func (m *Manager) checkPods(ctx context.Context) ([]string, error) {
	kube, err := m.Kubernetes(ctx)
	if err != nil {
		return nil, err
	}

	return UnreadyPods(ctx, kube)
}

func (m *Manager) reportPodCheck(unready []string, err error) {
	if err != nil {
		m.printf("Pod readiness check failed: %v\n", err)

		return
	}

	m.printf("Pods are not ready:\n  %s\n", strings.Join(unready, "\n  "))
}

// promptPodCheckFailure asks whether to retry or continue; cancelling returns ErrCancelled.
func (m *Manager) promptPodCheckFailure() (retry bool, err error) {
	for {
		m.printf("Choose [r]etry, [q] cancel, or [c]ontinue: ")

		answer, err := m.readAnswer()
		if err != nil {
			return false, err
		}

		switch answer {
		case "r", "retry":
			return true, nil
		case "q", "quit", "x", "cancel":
			return false, ErrCancelled
		case "c", "continue":
			m.printf("Continuing despite the failed pod check.\n")

			return false, nil
		default:
			m.printf("Invalid input. Please enter retry, cancel, or continue.\n")
		}
	}
}

// EnsurePodsReady blocks a node change until all pods are ready or the user decides otherwise.
func (m *Manager) EnsurePodsReady(ctx context.Context, operation, node string) error {
	if m.Options.SkipPodCheck {
		return nil
	}

	m.printf("Checking pod readiness before %s %s...\n", operation, node)

	for {
		unready, err := m.checkPods(ctx)
		if err == nil && len(unready) == 0 {
			m.printf("Pod readiness check passed.\n")

			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		m.reportPodCheck(unready, err)

		retry, err := m.promptPodCheckFailure()
		if err != nil || !retry {
			return err
		}

		m.printf("Retrying pod check...\n")
	}
}

// WaitForPods waits until all pods are ready on consecutive checks after a node change.
// It reports whether the check passed; false means it was skipped or the user continued anyway.
func (m *Manager) WaitForPods(ctx context.Context, operation, node string) (bool, error) {
	if m.Options.SkipPodCheck {
		return false, nil
	}

	for {
		m.printf("Waiting up to %s for pod readiness after %s %s...\n", m.PodCheckTimeout, operation, node)

		deadline := m.now().Add(m.PodCheckTimeout)
		stable := 0

		var (
			unready []string
			lastErr error
		)

		for m.now().Before(deadline) {
			unready, lastErr = m.checkPods(ctx)
			if ctx.Err() != nil {
				return false, ctx.Err()
			}

			if lastErr == nil && len(unready) == 0 {
				stable++
				if stable >= m.PodStableChecks {
					m.printf("Pod readiness check passed.\n")

					return true, nil
				}

				m.printf("All pods are ready; confirming stability...\n")
			} else {
				stable = 0
			}

			if remaining := deadline.Sub(m.now()); remaining > 0 {
				if err := m.sleep(ctx, min(m.PodCheckInterval, remaining)); err != nil {
					return false, err
				}
			}
		}

		m.printf("Pod readiness check is still failing after %s.\n", m.PodCheckTimeout)

		if lastErr != nil || len(unready) > 0 {
			m.reportPodCheck(unready, lastErr)
		}

		retry, err := m.promptPodCheckFailure()
		if err != nil {
			return false, err
		}

		if !retry {
			return false, nil
		}

		m.printf("Retrying pod check with a new %s timeout...\n", m.PodCheckTimeout)
	}
}
