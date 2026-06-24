package guestfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type ExtractedFile struct {
	LocalPath  string
	RemotePath string
	Size       int64
}

// ResolvePVCName finds the PVC backing the VM's boot disk.
// If diskName is specified, it looks for that specific volume; otherwise it
// returns the first volume backed by a PVC or DataVolume.
func ResolvePVCName(vmObj *unstructured.Unstructured, diskName string) (string, error) {
	volumes, found, err := unstructured.NestedSlice(vmObj.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		return "", fmt.Errorf("VM has no spec.template.spec.volumes")
	}

	for _, v := range volumes {
		vol, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(vol, "name")
		if diskName != "" && name != diskName {
			continue
		}

		if pvcClaim, found, _ := unstructured.NestedString(vol, "persistentVolumeClaim", "claimName"); found && pvcClaim != "" {
			return pvcClaim, nil
		}
		if dvName, found, _ := unstructured.NestedString(vol, "dataVolume", "name"); found && dvName != "" {
			return dvName, nil
		}
	}

	if diskName != "" {
		return "", fmt.Errorf("volume %q not found or not backed by a PVC/DataVolume", diskName)
	}
	return "", fmt.Errorf("no PVC or DataVolume-backed volume found in VM spec")
}

// GetPVCVolumeMode returns true if the PVC uses block mode.
func GetPVCVolumeMode(ctx context.Context, client kubernetes.Interface, namespace, pvcName string) (bool, error) {
	pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("fetching PVC %s/%s: %w", namespace, pvcName, err)
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return true, nil
	}
	return false, nil
}

const extractionScript = `#!/bin/sh
set -e
mkdir -p /output/minidumps

echo "Inspecting disk $DISK_PATH..."
guestfish --ro --inspector -a "$DISK_PATH" <<'GUESTFISH'
!mkdir -p /output/minidumps
-copy-out /Windows/MEMORY.DMP /output/
-glob copy-out /Windows/Minidump/* /output/minidumps/
GUESTFISH

echo "=== Extracted files ==="
find /output -type f -name '*.DMP' -o -name '*.dmp' 2>/dev/null | while read f; do
  size=$(stat -c '%s' "$f" 2>/dev/null || echo "unknown")
  echo "$size $f"
done
touch /output/.done
echo "Extraction complete. Waiting for file transfer..."
sleep infinity
`

// ExtractCrashDump creates a guestfs pod to extract Windows crash dumps from the VM's disk.
func ExtractCrashDump(ctx context.Context, kubeClient kubernetes.Interface, restConfig *rest.Config, namespace, pvcName, nodeName string, isBlock bool, guestfsImage string) ([]ExtractedFile, error) {
	diskPath := "/disk/disk.img"
	if isBlock {
		diskPath = "/dev/vda"
	}

	pod := buildGuestfsPod(namespace, pvcName, nodeName, isBlock, diskPath, guestfsImage)

	created, err := kubeClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating guestfs pod: %w", err)
	}
	podName := created.Name
	fmt.Fprintf(os.Stderr, "  Created guestfs pod %s on node %s\n", podName, nodeName)

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = kubeClient.CoreV1().Pods(namespace).Delete(cleanupCtx, podName, metav1.DeleteOptions{})
		fmt.Fprintf(os.Stderr, "  Cleaned up guestfs pod %s\n", podName)
	}()

	// Wait for the pod to be running
	fmt.Fprintf(os.Stderr, "  Waiting for guestfs pod to start...\n")
	if err := waitForPodRunning(ctx, kubeClient, namespace, podName); err != nil {
		return nil, fmt.Errorf("waiting for guestfs pod to start: %w", err)
	}

	// Wait for extraction to complete
	fmt.Fprintf(os.Stderr, "  Waiting for crash dump extraction...\n")
	if err := waitForExtractionDone(ctx, kubeClient, restConfig, namespace, podName); err != nil {
		return nil, fmt.Errorf("waiting for extraction: %w", err)
	}

	// List extracted files
	files, err := listExtractedFiles(ctx, kubeClient, restConfig, namespace, podName)
	if err != nil {
		return nil, fmt.Errorf("listing extracted files: %w", err)
	}

	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "  No crash dump files found on the VM's disk.\n")
		return nil, nil
	}

	// Stream each file to local disk
	var extracted []ExtractedFile
	for _, remotePath := range files {
		baseName := filepath.Base(remotePath)
		localPath := baseName
		fmt.Fprintf(os.Stderr, "  Streaming %s to %s...\n", remotePath, localPath)

		size, err := streamFileFromPod(ctx, kubeClient, restConfig, namespace, podName, remotePath, localPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: Failed to stream %s: %v\n", remotePath, err)
			continue
		}
		extracted = append(extracted, ExtractedFile{
			LocalPath:  localPath,
			RemotePath: remotePath,
			Size:       size,
		})
	}

	return extracted, nil
}

func buildGuestfsPod(namespace, pvcName, nodeName string, isBlock bool, diskPath, guestfsImage string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "guest-crash-collector-guestfs-",
			Namespace:    namespace,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "disk",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
							ReadOnly:  true,
						},
					},
				},
				{
					Name:         "output",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
				{
					Name:         "guestfs-tmp",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
			},
			Containers: []corev1.Container{{
				Name:  "guestfs",
				Image: guestfsImage,
				Command: []string{"sh", "-c", extractionScript},
				Env: []corev1.EnvVar{
					{Name: "DISK_PATH", Value: diskPath},
					{Name: "LIBGUESTFS_BACKEND", Value: "direct"},
					{Name: "LIBGUESTFS_PATH", Value: "/usr/local/lib/guestfs/appliance"},
				},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						"devices.kubevirt.io/kvm": resource.MustParse("1"),
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "output", MountPath: "/output"},
					{Name: "guestfs-tmp", MountPath: "/tmp/guestfs"},
				},
			}},
		},
	}

	container := &pod.Spec.Containers[0]
	if isBlock {
		container.VolumeDevices = []corev1.VolumeDevice{
			{Name: "disk", DevicePath: "/dev/vda"},
		}
	} else {
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: "disk", MountPath: "/disk", ReadOnly: true},
		)
	}

	return pod
}

func waitForPodRunning(ctx context.Context, client kubernetes.Interface, namespace, podName string) error {
	return wait.PollUntilContextTimeout(ctx, 3*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch p.Status.Phase {
		case corev1.PodRunning:
			return true, nil
		case corev1.PodFailed:
			return false, fmt.Errorf("guestfs pod failed")
		default:
			return false, nil
		}
	})
}

func waitForExtractionDone(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName string) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		var stdout bytes.Buffer
		err := execInPod(ctx, client, restConfig, namespace, podName,
			[]string{"test", "-f", "/output/.done"},
			nil, &stdout)
		return err == nil, nil
	})
}

func listExtractedFiles(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName string) ([]string, error) {
	var stdout bytes.Buffer
	err := execInPod(ctx, client, restConfig, namespace, podName,
		[]string{"find", "/output", "-type", "f", "-name", "*.DMP", "-o", "-name", "*.dmp"},
		nil, &stdout)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != "/output/.done" {
			files = append(files, line)
		}
	}
	return files, nil
}

func streamFileFromPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, remotePath, localPath string) (int64, error) {
	f, err := os.Create(localPath)
	if err != nil {
		return 0, fmt.Errorf("creating local file: %w", err)
	}
	defer f.Close()

	err = execInPod(ctx, client, restConfig, namespace, podName,
		[]string{"cat", remotePath},
		nil, f)
	if err != nil {
		os.Remove(localPath)
		return 0, err
	}

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func execInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName string, command []string, stdin io.Reader, stdout io.Writer) error {
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: "guestfs",
		Command:   command,
		Stdin:     stdin != nil,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	var stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("exec %v: %w (stderr: %s)", command, err, stderr.String())
	}
	return nil
}
