package prometheus

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/mansam/guest-crash-collector/pkg/kube"
)

type bearerTokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

func QueryNodeAtTime(ctx context.Context, dynClient dynamic.Interface, restConfig *rest.Config, namespace, vmName, prometheusURL string, crashTime time.Time) (string, error) {
	url := prometheusURL
	if url == "" {
		var err error
		url, err = discoverPrometheusURL(ctx, dynClient)
		if err != nil {
			return "", fmt.Errorf("auto-discovering Prometheus: %w", err)
		}
	}

	token, err := getBearerToken(restConfig)
	if err != nil {
		return "", fmt.Errorf("getting bearer token: %w", err)
	}

	transport := &bearerTokenTransport{
		token: token,
		base: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	client, err := promapi.NewClient(promapi.Config{
		Address:      url,
		RoundTripper: transport,
	})
	if err != nil {
		return "", fmt.Errorf("creating Prometheus client: %w", err)
	}

	api := promv1.NewAPI(client)
	query := fmt.Sprintf(`kubevirt_vmi_info{name="%s", namespace="%s"}`, vmName, namespace)

	result, warnings, err := api.Query(ctx, query, crashTime)
	if err != nil {
		return "", fmt.Errorf("querying Prometheus: %w", err)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "WARNING: Prometheus query warning: %s\n", w)
	}

	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return "", fmt.Errorf("no results for kubevirt_vmi_info query at %s", crashTime.Format(time.RFC3339))
	}

	node := string(vector[0].Metric["node"])
	if node == "" {
		return "", fmt.Errorf("kubevirt_vmi_info result has no 'node' label")
	}

	return node, nil
}

func discoverPrometheusURL(ctx context.Context, dynClient dynamic.Interface) (string, error) {
	route, err := dynClient.Resource(kube.RouteGVR).Namespace("openshift-monitoring").Get(ctx, "thanos-querier", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("fetching thanos-querier route: %w", err)
	}

	host, found, err := unstructured.NestedString(route.Object, "spec", "host")
	if err != nil || !found || host == "" {
		return "", fmt.Errorf("thanos-querier route has no spec.host")
	}

	return "https://" + host, nil
}

func getBearerToken(config *rest.Config) (string, error) {
	if config.BearerToken != "" {
		return config.BearerToken, nil
	}
	if config.BearerTokenFile != "" {
		data, err := os.ReadFile(config.BearerTokenFile)
		if err != nil {
			return "", fmt.Errorf("reading bearer token file: %w", err)
		}
		return string(data), nil
	}
	return "", fmt.Errorf("no bearer token available in kubeconfig (try 'oc login' or set a token)")
}
