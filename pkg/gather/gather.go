package gather

import (
	"context"
	"fmt"
	"io"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/mansam/guest-crash-collector/pkg/archive"
	"github.com/mansam/guest-crash-collector/pkg/guestfs"
	"github.com/mansam/guest-crash-collector/pkg/kube"
	"github.com/mansam/guest-crash-collector/pkg/node"
)

func Run(ctx context.Context, cfg Config) error {
	clients, err := kube.NewClients(cfg.Kubeconfig)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}

	var artifacts []archive.Artifact

	// Step 1: Get VM YAML
	fmt.Fprintf(os.Stderr, "Fetching VirtualMachine %s/%s...\n", cfg.Namespace, cfg.VMName)
	vm, err := clients.Dynamic.Resource(kube.VMGVR).Namespace(cfg.Namespace).Get(ctx, cfg.VMName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("fetching VirtualMachine %s/%s: %w", cfg.Namespace, cfg.VMName, err)
	}

	vmYAML, err := yaml.Marshal(vm.Object)
	if err != nil {
		return fmt.Errorf("marshaling VM to YAML: %w", err)
	}
	artifacts = append(artifacts, archive.Artifact{
		Filename: fmt.Sprintf("virtualmachine-%s.yaml", cfg.VMName),
		Data:     vmYAML,
	})

	// Step 2: Get VMI to determine the node
	fmt.Fprintf(os.Stderr, "Fetching VirtualMachineInstance %s/%s...\n", cfg.Namespace, cfg.VMName)
	vmiObj, nodeName, err := getVMINodeName(ctx, clients, cfg.Namespace, cfg.VMName)
	if err != nil {
		return fmt.Errorf("VM must be running to collect crash context: %w", err)
	}
	fmt.Fprintf(os.Stderr, "VM is running on node %s\n", nodeName)

	vmiYAML, err := yaml.Marshal(vmiObj.Object)
	if err != nil {
		return fmt.Errorf("marshaling VMI to YAML: %w", err)
	}
	artifacts = append(artifacts, archive.Artifact{
		Filename: fmt.Sprintf("virtualmachineinstance-%s.yaml", cfg.VMName),
		Data:     vmiYAML,
	})

	// Step 3: Get dmesg from the node
	since := cfg.CrashTime.Add(-cfg.Window)
	until := cfg.CrashTime.Add(cfg.Window)
	fmt.Fprintf(os.Stderr, "Collecting dmesg from node %s (%s to %s)...\n", nodeName, since.Format("15:04:05"), until.Format("15:04:05"))

	dmesgData, err := node.GetDmesg(ctx, clients.Kubernetes, nodeName, since, until, cfg.DebugImage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to collect dmesg from node %s: %v\n", nodeName, err)
	} else {
		artifacts = append(artifacts, archive.Artifact{
			Filename: fmt.Sprintf("node-%s-dmesg.log", nodeName),
			Data:     dmesgData,
		})
	}

	// Step 4: Collect virt-launcher pod logs
	podArtifacts, err := collectPodLogs(ctx, clients, cfg.Namespace, cfg.VMName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to collect pod logs: %v\n", err)
	} else {
		artifacts = append(artifacts, podArtifacts...)
	}

	// Step 5: Extract Windows crash dump (if requested)
	if cfg.CollectDump {
		fmt.Fprintf(os.Stderr, "Extracting Windows crash dump...\n")

		pvcName, err := guestfs.ResolvePVCName(vm, cfg.DiskName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: Cannot resolve boot disk PVC: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Boot disk PVC: %s\n", pvcName)

			isBlock, err := guestfs.GetPVCVolumeMode(ctx, clients.Kubernetes, cfg.Namespace, pvcName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: Cannot determine PVC volume mode: %v\n", err)
			} else {
				extracted, err := guestfs.ExtractCrashDump(ctx, clients.Kubernetes, clients.RestConfig, cfg.Namespace, pvcName, nodeName, isBlock, cfg.GuestfsImage)
				if err != nil {
					fmt.Fprintf(os.Stderr, "WARNING: Crash dump extraction failed: %v\n", err)
				} else if len(extracted) > 0 {
					fmt.Fprintf(os.Stderr, "Extracted crash dump files:\n")
					for _, f := range extracted {
						fmt.Fprintf(os.Stderr, "  %s (%d bytes)\n", f.LocalPath, f.Size)
					}
				}
			}
		}
	}

	// Step 6: Create archive
	outputFile := fmt.Sprintf("guest-crash-collector-%s-%s.tar.gz", cfg.VMName, cfg.CrashTime.Format("20060102-150405"))
	fmt.Fprintf(os.Stderr, "Creating archive %s...\n", outputFile)

	if err := archive.Create(artifacts, outputFile); err != nil {
		return fmt.Errorf("creating archive: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Done. Archive: %s\n", outputFile)
	fmt.Fprintf(os.Stderr, "Contents:\n")
	for _, a := range artifacts {
		fmt.Fprintf(os.Stderr, "  %s (%d bytes)\n", a.Filename, len(a.Data))
	}

	return nil
}

func getVMINodeName(ctx context.Context, clients *kube.Clients, namespace, vmName string) (*unstructured.Unstructured, string, error) {
	vmi, err := clients.Dynamic.Resource(kube.VMIGVR).Namespace(namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("fetching VMI: %w", err)
	}

	nodeName, found, err := unstructured.NestedString(vmi.Object, "status", "nodeName")
	if err != nil || !found || nodeName == "" {
		return nil, "", fmt.Errorf("VMI has no status.nodeName")
	}

	return vmi, nodeName, nil
}

func collectPodLogs(ctx context.Context, clients *kube.Clients, namespace, vmName string) ([]archive.Artifact, error) {
	pods, err := clients.Kubernetes.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "vm.kubevirt.io/name=" + vmName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing virt-launcher pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no virt-launcher pods found with label vm.kubevirt.io/name=%s", vmName)
	}

	var artifacts []archive.Artifact
	for _, pod := range pods.Items {
		containers := allContainerNames(&pod)
		for _, containerName := range containers {
			fmt.Fprintf(os.Stderr, "  Collecting logs from %s/%s...\n", pod.Name, containerName)

			data, err := getPodContainerLogs(ctx, clients, namespace, pod.Name, containerName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  WARNING: Failed to get logs for %s/%s: %v\n", pod.Name, containerName, err)
				continue
			}
			artifacts = append(artifacts, archive.Artifact{
				Filename: fmt.Sprintf("%s-%s.log", pod.Name, containerName),
				Data:     data,
			})
		}
	}

	return artifacts, nil
}

func allContainerNames(pod *corev1.Pod) []string {
	var names []string
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

func getPodContainerLogs(ctx context.Context, clients *kube.Clients, namespace, podName, containerName string) ([]byte, error) {
	req := clients.Kubernetes.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	return io.ReadAll(stream)
}
