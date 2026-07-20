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
