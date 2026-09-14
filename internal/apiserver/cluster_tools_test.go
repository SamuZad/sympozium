package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClusterCellnToolsAreListedSortedAndReadOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sympoziumv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&sympoziumv1alpha1.ClusterCellnTool{ObjectMeta: metav1.ObjectMeta{Name: "celln-trial-workspace-write"}, Spec: sympoziumv1alpha1.CellnToolSpec{Revision: "v1"}},
		&sympoziumv1alpha1.ClusterCellnTool{ObjectMeta: metav1.ObjectMeta{Name: "celln-trial-https-fetch"}, Spec: sympoziumv1alpha1.CellnToolSpec{Revision: "v1"}},
		&sympoziumv1alpha1.CellnTool{ObjectMeta: metav1.ObjectMeta{Name: "namespaced", Namespace: "default"}, Spec: sympoziumv1alpha1.CellnToolSpec{Revision: "v1"}},
	).Build()
	srv := NewServer(cl, nil, nil, logr.Discard())
	res := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/cluster-celln-tools", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var tools []sympoziumv1alpha1.ClusterCellnTool
	if err := json.Unmarshal(res.Body.Bytes(), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "celln-trial-https-fetch" || tools[1].Name != "celln-trial-workspace-write" {
		t.Fatalf("unexpected listing: %+v", tools)
	}
	res = httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/v1/cluster-celln-tools", nil))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("cluster tools accepted a write: %d", res.Code)
	}
}
