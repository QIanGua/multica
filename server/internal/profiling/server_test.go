package profiling

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandlerServesCPUAndHeapProfilesOnlyOnPprofPaths(t *testing.T) {
	handler := NewHandler()

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if index.Code != http.StatusOK {
		t.Fatalf("pprof index status = %d, want %d", index.Code, http.StatusOK)
	}
	indexBody, _ := io.ReadAll(index.Body)
	for _, profile := range []string{
		"<a href='profile?debug=1'>profile</a>",
		"<a href='heap?debug=1'>heap</a>",
	} {
		if !strings.Contains(string(indexBody), profile) {
			t.Fatalf("pprof index does not advertise %q: %s", profile, indexBody)
		}
	}

	heap := httptest.NewRecorder()
	handler.ServeHTTP(heap, httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil))
	if heap.Code != http.StatusOK {
		t.Fatalf("heap profile status = %d, want %d", heap.Code, http.StatusOK)
	}
	if got := heap.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("heap profile Content-Type = %q, want application/octet-stream", got)
	}

	notFound := httptest.NewRecorder()
	handler.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/health", nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("non-pprof path status = %d, want %d", notFound.Code, http.StatusNotFound)
	}

	methodNotAllowed := httptest.NewRecorder()
	handler.ServeHTTP(methodNotAllowed, httptest.NewRequest(http.MethodPost, "/debug/pprof/heap", nil))
	if methodNotAllowed.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST heap profile status = %d, want %d", methodNotAllowed.Code, http.StatusMethodNotAllowed)
	}
}

func TestServerIsLoopbackOnlyAndAllowsLongRunningProfiles(t *testing.T) {
	server := NewServer()
	if server.Addr != "127.0.0.1:6060" {
		t.Fatalf("server address = %q, want loopback pprof address", server.Addr)
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want 5s", server.ReadHeaderTimeout)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want zero so long profiles can complete", server.WriteTimeout)
	}
}
