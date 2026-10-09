package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestManagementControlPanelRefusesFramingByOtherSites(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	if err := os.WriteFile(filepath.Join(staticDir, "management.html"), []byte("<html>management app</html>"), 0o600); err != nil {
		t.Fatalf("failed to write management asset: %v", err)
	}
	server := newTestServer(t)

	assertFrameHeaders := func(t *testing.T, rr *httptest.ResponseRecorder) {
		t.Helper()
		if got := rr.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'self'" {
			t.Fatalf("Content-Security-Policy = %q, want frame-ancestors 'self'", got)
		}
		if got := rr.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
			t.Fatalf("X-Frame-Options = %q, want SAMEORIGIN", got)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	assertFrameHeaders(t, rr)

	req = httptest.NewRequest(http.MethodGet, "/management.html", nil)
	req.Header.Set("If-Modified-Since", rr.Header().Get("Last-Modified"))
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want %d", rr.Code, http.StatusNotModified)
	}
	assertFrameHeaders(t, rr)
}
