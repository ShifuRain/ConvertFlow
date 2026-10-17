package ui_test

import (
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ui"
)

// requireBuilt skips when the UI has not been built (a fresh checkout has an empty dist). Set
// CONVERTFLOW_REQUIRE_UI=1 (as `make test` does) to turn that skip into a failure.
func requireBuilt(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("dist/index.html"); err != nil {
		if os.Getenv("CONVERTFLOW_REQUIRE_UI") != "" {
			t.Fatal("dist/index.html missing: run `make ui` first")
		}
		t.Skip("UI not built (run `make ui`)")
	}
}

func TestIndexServed(t *testing.T) {
	requireBuilt(t)
	w := httptest.NewRecorder()
	ui.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "main.js") {
		t.Fatalf("index not served (run `make ui` first): %d", w.Code)
	}
	for _, p := range []string{"/main.js", "/docs.js", "/style.css", "/docs.html"} {
		w := httptest.NewRecorder()
		ui.Handler().ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s not served: %d", p, w.Code)
		}
	}
}

func TestNoInlineScriptsOrExternalReferences(t *testing.T) {
	requireBuilt(t)
	root := os.DirFS("dist")
	ext := regexp.MustCompile(`https?://`)
	allowed := "http://www.w3.org/"
	inlineScript := regexp.MustCompile(`<script(\s[^>]*)?>[^<]`)
	inlineHandler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return err
		}
		b, _ := fs.ReadFile(root, p)
		s := strings.ReplaceAll(string(b), allowed, "")
		if ext.MatchString(s) {
			t.Errorf("%s references an external URL (air-gap violation)", p)
		}
		if strings.HasSuffix(p, ".html") {
			if inlineScript.MatchString(s) || strings.Contains(s, " style=") || strings.Contains(s, "<style") || inlineHandler.MatchString(s) {
				t.Errorf("%s contains inline script/style/handler (violates CSP)", p)
			}
		}
		if strings.HasSuffix(p, ".css") && (strings.Contains(s, "@import") || strings.Contains(s, "url(")) {
			t.Errorf("%s loads extra resources (@import/url())", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Server-provided strings (filenames, errors, encoder names) must only reach the DOM through
// textContent. Guard both the TypeScript source and the built bundles.
func TestNoUnsafeDOMSinks(t *testing.T) {
	bad := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\s*\(|new\s+Function\s*\(|createContextualFragment|srcdoc`)
	var files []string
	for _, glob := range []string{"../../ui/src/*.ts", "dist/*.js"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	src, _ := filepath.Glob("../../ui/src/*.ts")
	if len(src) == 0 {
		t.Fatal("no UI sources found to scan")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := bad.FindString(string(b)); loc != "" {
			t.Errorf("%s uses unsafe DOM sink %q", f, loc)
		}
	}
}

func TestDotfilesAreNotServed(t *testing.T) {
	for _, p := range []string{"/.gitkeep", "/.", "/a/.hidden", "/%2egitkeep"} {
		w := httptest.NewRecorder()
		ui.Handler().ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code == 200 {
			t.Errorf("%s served with 200", p)
		}
	}
}
