package collectors

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// K8s reports node readiness and pod phase counts from the API server.
type K8s struct {
	apiURL string
	token  string
	client *http.Client
}

func NewK8s(apiURL, token string) *K8s {
	return &K8s{apiURL: apiURL, token: token, client: &http.Client{
		Timeout: 5 * time.Second,
		// in-cluster CA handling is deploy-time config; skip-verify inside the cluster
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
}

func (k *K8s) Name() string { return "k8s" }

// watchCacheList is appended to every cluster-wide LIST this collector issues.
//
// DO NOT REMOVE THIS. Without a resourceVersion the kube-apiserver treats a
// LIST as "most recent" and satisfies it with a quorum read straight out of
// etcd; at one sample per interval, on every agent, that is a standing load on
// the cluster's consensus store for numbers nobody reads twice. With
// resourceVersion=0 the apiserver answers from its in-memory watch cache
// instead. The documented cost is that the answer may lag the true state by a
// short moment — which for a node/pod counter sampled every few seconds is
// exactly the right trade, and is why this must not be "fixed" back into a
// quorum read.
const watchCacheList = "?resourceVersion=0"

func (k *K8s) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (k *K8s) Collect(ctx context.Context) ([]Sample, error) {
	var nodes struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.get(ctx, "/api/v1/nodes"+watchCacheList, &nodes); err != nil {
		return nil, err
	}
	ready := 0.0
	for _, n := range nodes.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
			}
		}
	}

	var pods struct {
		Items []struct {
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					RestartCount float64 `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.get(ctx, "/api/v1/pods"+watchCacheList, &pods); err != nil {
		return nil, err
	}
	running, failed, restarts := 0.0, 0.0, 0.0
	for _, p := range pods.Items {
		switch p.Status.Phase {
		case "Running":
			running++
		case "Failed":
			failed++
		}
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
	}

	return []Sample{
		{Key: "k8s.nodes_ready", Value: ready},
		{Key: "k8s.nodes_total", Value: float64(len(nodes.Items))},
		{Key: "k8s.pods_running", Value: running},
		{Key: "k8s.pods_failed", Value: failed},
		{Key: "k8s.restarts", Value: restarts},
	}, nil
}
