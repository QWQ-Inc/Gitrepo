package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler("/ui/")
	for path, want := range map[string]string{
		"/ui/": "<title>Gitrepo</title>", "/ui/app.js": "function viewRepos", "/ui/app.css": "Geist Variable",
		"/ui/stackenigma.css": "--primary", "/ui/theme.js": "data-theme", "/ui/NOTICE.txt": "Apache License",
	} {
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

func TestFonts(t *testing.T) {
	h := Handler("/ui/")
	for _, f := range []string{"geist-latin-wght-normal.woff2", "geist-mono-latin-wght-normal.woff2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/fonts/"+f, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "font/woff2" || rec.Body.Len() < 10000 {
			t.Errorf("%s: %d %q %d bytes", f, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
	}
}

// Everything the page loads is served from the console itself: the CSP
// allows nothing else (no font or script CDNs).
func TestNoExternalResources(t *testing.T) {
	for _, name := range []string{"static/index.html", "static/app.css", "static/stackenigma.css", "static/app.js", "static/theme.js"} {
		b, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, bad := range []string{"fonts.googleapis", "fonts.gstatic", "<script>", "@import"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
		if strings.HasSuffix(name, ".html") && strings.Contains(s, " style=") {
			t.Errorf("%s uses an inline style attribute (blocked by the CSP)", name)
		}
	}
}

// The console must not use innerHTML or inline styles/scripts, which the
// CSP forbids and which would open XSS holes with API data.
func TestNoUnsafeDOM(t *testing.T) {
	for _, name := range []string{"static/app.js", "static/theme.js"} {
		b, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "style: '"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", name, bad)
			}
		}
	}
}
