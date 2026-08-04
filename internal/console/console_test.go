package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedConsoleAssets(t *testing.T) {
	h := Handler()
	for _, test := range []struct{ path, contentType string }{
		{"/", "text/html"}, {"/assets/console.css", "text/css"}, {"/assets/console.mjs", "text/javascript"}, {"/assets/model.mjs", "text/javascript"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
			if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), test.contentType) || response.Body.Len() == 0 {
				t.Fatalf("asset unavailable: %d %s", response.Code, response.Body.String())
			}
			policy := response.Header().Get("Content-Security-Policy")
			if !strings.Contains(policy, "script-src 'self'") || !strings.Contains(policy, "frame-ancestors 'none'") || strings.Contains(policy, "unsafe-inline") {
				t.Fatalf("unexpected script policy: %s", policy)
			}
			head := httptest.NewRecorder()
			h.ServeHTTP(head, httptest.NewRequest("HEAD", test.path, nil))
			if head.Code != http.StatusOK || head.Body.Len() != 0 {
				t.Fatal("HEAD returned an asset body")
			}
		})
	}
}

func TestConsoleDoesNotExposeOtherFilesOrMutations(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/README.md", "/assets/../console.go", "/assets/missing.js", "/.env", "/v1/workflows"} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("unexpected public path: %s", path)
		}
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest("POST", "/", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("console accepted a mutation")
	}
}
