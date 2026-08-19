package collectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestK8sCollectsNodeAndPodHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer satoken" {
			t.Errorf("missing bearer token")
		}
		switch r.URL.Path {
		case "/api/v1/nodes":
			w.Write([]byte(`{"items":[
				{"status":{"conditions":[{"type":"Ready","status":"True"}]}},
				{"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`))
		case "/api/v1/pods":
			w.Write([]byte(`{"items":[
				{"status":{"phase":"Running","containerStatuses":[{"restartCount":2}]}},
				{"status":{"phase":"Failed","containerStatuses":[{"restartCount":5}]}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	k := NewK8s(srv.URL, "satoken")
	got, err := k.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	want := map[string]float64{
		"k8s.nodes_ready": 1, "k8s.nodes_total": 2,
		"k8s.pods_running": 1, "k8s.pods_failed": 1, "k8s.restarts": 7,
	}
	for k2, v := range want {
		if byKey[k2] != v {
			t.Fatalf("%s: got %v want %v (all: %v)", k2, byKey[k2], v, byKey)
		}
	}
}

// Both cluster-wide LISTs must ask for resourceVersion=0 so the apiserver
// answers from its watch cache instead of forcing a quorum read out of etcd on
// every sample. Sampling every few seconds, a slightly stale count is fine; an
// etcd quorum read per interval is not.
func TestK8sListsRequestWatchCacheReads(t *testing.T) {
	type seen struct {
		path            string
		resourceVersion string
		present         bool
	}
	var got []seen

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rv, ok := q["resourceVersion"]
		s := seen{path: r.URL.Path, present: ok}
		if ok {
			s.resourceVersion = rv[0]
		}
		got = append(got, s)
		w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	k := NewK8s(srv.URL, "satoken")
	if _, err := k.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{"/api/v1/nodes", "/api/v1/pods"}
	if len(got) != len(want) {
		t.Fatalf("expected %d LIST calls, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i].path != w {
			t.Fatalf("call %d: path = %q, want %q", i, got[i].path, w)
		}
		if !got[i].present {
			t.Errorf("call %d (%s): no resourceVersion query param — this forces an etcd quorum read every sample", i, w)
			continue
		}
		if got[i].resourceVersion != "0" {
			t.Errorf("call %d (%s): resourceVersion = %q, want %q", i, w, got[i].resourceVersion, "0")
		}
	}
}
