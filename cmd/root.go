package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mansam/guest-crash-collector/pkg/gather"
)

var (
	namespace    string
	vmName       string
	around       string
	window       string
	debugImage   string
	kubeconfig   string
	collectDump  bool
	guestfsImage string
	diskName     string
)

var rootCmd = &cobra.Command{
	Use:   "guest-crash-collector",
	Short: "Gather diagnostic context for KubeVirt VM guest OS crashes",
	Long: `Collects VM YAML, node dmesg logs, VMI YAML, and virt-launcher pod logs
around a crash timestamp and packages them into a tarball for analysis.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		crashTime, err := time.Parse(time.RFC3339, around)
		if err != nil {
			return fmt.Errorf("parsing --around timestamp: %w (expected RFC3339 format like 2024-01-15T12:30:00Z)", err)
		}

		dur, err := time.ParseDuration(window)
		if err != nil {
			return fmt.Errorf("parsing --window duration: %w (expected format like 30m, 1h)", err)
		}

		if kubeconfig == "" {
			kubeconfig = os.Getenv("KUBECONFIG")
			if kubeconfig == "" {
				home, _ := os.UserHomeDir()
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}

		cfg := gather.Config{
			Namespace:    namespace,
			VMName:       vmName,
			CrashTime:    crashTime,
			Window:       dur,
			DebugImage:   debugImage,
			Kubeconfig:   kubeconfig,
			CollectDump:  collectDump,
			GuestfsImage: guestfsImage,
			DiskName:     diskName,
		}

		return gather.Run(context.Background(), cfg)
	},
}

func init() {
	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace of the VM (required)")
	rootCmd.Flags().StringVarP(&vmName, "vm", "v", "", "name of the VM (required)")
	rootCmd.Flags().StringVarP(&around, "around", "a", "", "crash timestamp in RFC3339 format (required)")
	rootCmd.Flags().StringVarP(&window, "window", "w", "30m", "time window around crash (e.g. 30m, 1h)")
	rootCmd.Flags().StringVar(&debugImage, "debug-image", "registry.access.redhat.com/ubi9/ubi-minimal", "container image for the debug pod")
	rootCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (defaults to $KUBECONFIG or ~/.kube/config)")
	rootCmd.Flags().BoolVar(&collectDump, "collect-dump", false, "extract Windows crash dump (MEMORY.DMP) from the VM's disk via libguestfs")
	rootCmd.Flags().StringVar(&guestfsImage, "guestfs-image", "quay.io/kubevirt/libguestfs-tools:latest", "container image for the guestfs pod")
	rootCmd.Flags().StringVar(&diskName, "disk", "", "VM volume name for the boot disk (default: first PVC/DataVolume volume)")

	_ = rootCmd.MarkFlagRequired("namespace")
	_ = rootCmd.MarkFlagRequired("vm")
	_ = rootCmd.MarkFlagRequired("around")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
