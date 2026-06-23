package gather

import "time"

type Config struct {
	Namespace     string
	VMName        string
	CrashTime     time.Time
	Window        time.Duration
	PrometheusURL string
	DebugImage    string
	Kubeconfig    string
}

