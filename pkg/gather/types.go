package gather

import "time"

type Config struct {
	Namespace    string
	VMName       string
	CrashTime    time.Time
	Window       time.Duration
	DebugImage   string
	Kubeconfig   string
	CollectDump  bool
	GuestfsImage string
	DiskName     string
}

