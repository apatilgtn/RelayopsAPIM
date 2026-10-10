package admin

import (
	"github.com/relayops/apim/web"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

// These routes are shared by authenticated and public product surfaces.
func TestEmbeddedDesignAssetsArePublic(t *testing.T) {
	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(nil, nil, nil, "private-test-token", "ui-test", static).Handler()
	paths := []string{"/design-system.css", "/ui.js", "/fonts/InterVariable.woff2"}
	icons, err := fs.Glob(static, "icons/*.svg")
	if err != nil {
		t.Fatal(err)
	}
	if len(icons) < 30 {
		t.Fatal("shared icon bundle is incomplete")
	}
	for _, icon := range icons {
		paths = append(paths, "/"+icon)
	}
	for _, path := range paths {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("public embedded asset %s: status=%d bytes=%d", path, w.Code, w.Body.Len())
		}
		if strings.HasSuffix(path, ".svg") && !strings.Contains(w.Body.String(), "<svg") {
			t.Errorf("invalid SVG %s", path)
		}
	}
	for _, path := range []string{"/", "/portal", "/login", "/signup"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "/design-system.css") || !strings.Contains(w.Body.String(), "/ui.js") {
			t.Errorf("shared foundation missing at %s", path)
		}
	}
}
