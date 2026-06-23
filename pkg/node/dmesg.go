package node

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

func GetDmesg(ctx context.Context, client kubernetes.Interface, nodeName string, since, until time.Time, debugImage string) ([]byte, error) {
	sinceStr := since.Format("2006-01-02T15:04:05-0700")
	untilStr := until.Format("2006-01-02T15:04:05-0700")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "vmi-crash-gather-debug-",
			Namespace:    "default",
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			HostPID:       true,
			Containers: []corev1.Container{{
				Name:  "debug",
				Image: debugImage,
				Command: []string{
					"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
					"dmesg", "--time-format", "iso",
					"--since", sinceStr,
					"--until", untilStr,
				},
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.To(true),
				},
			}},
		},
	}

	created, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating debug pod: %w", err)
	}
	podName := created.Name

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.CoreV1().Pods("default").Delete(cleanupCtx, podName, metav1.DeleteOptions{})
	}()

	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := client.CoreV1().Pods("default").Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return true, nil
		default:
			return false, nil
		}
	})
	if err != nil {
		return nil, fmt.Errorf("waiting for debug pod to complete: %w", err)
	}

	req := client.CoreV1().Pods("default").GetLogs(podName, &corev1.PodLogOptions{
		Container: "debug",
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("streaming debug pod logs: %w", err)
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("reading debug pod logs: %w", err)
	}

	return data, nil
}
