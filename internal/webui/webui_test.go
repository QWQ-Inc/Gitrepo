package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler("/ui/")
	for path, want := range map[string]string{"/ui/": "<title>Gitrepo</title>", "/ui/app.js": "function viewRepos", "/ui/app.css": "--accent"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d, body lacks %q", path, rec.Code, want)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}
}

// The console must not use innerHTML or inline styles/scripts, which the
// CSP forbids and which would open XSS holes with API data.
func TestNoUnsafeDOM(t *testing.T) {
	b, err := files.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "style: '"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
}
