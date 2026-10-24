package webassets

import (
	"bytes"
	"crypto/sha256"
	"image/jpeg"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release builder enables this after Vite completes, so it verifies the
// actual Go-embedded assets rather than only the frontend source directory.
// Ordinary source-only Go tests may still use the checked-in dist placeholder.
func TestEmbeddedHelpBundle(t *testing.T) {
	if os.Getenv("RCD_VERIFY_WEB_HELP") != "1" {
		t.Skip("release-only embedded web check; build frontend then set RCD_VERIFY_WEB_HELP=1")
	}
	assets := FS()
	metadata, err := os.ReadFile("src/features/help/screenshots.ts")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`id: '((?:manager|platform)-[a-z-]+)'`).FindAllSubmatch(metadata, -1)
	if len(matches) == 0 {
		t.Fatal("empty help screenshot manifest")
	}
	seen := map[string]bool{}
	for _, match := range matches {
		name := string(match[1]) + ".jpg"
		if seen[name] {
			continue
		}
		seen[name] = true
		path := "help/screenshots/" + name
		actual, err := fs.ReadFile(assets, path)
		if err != nil {
			t.Fatalf("not embedded: %s: %v", path, err)
		}
		expected, err := os.ReadFile(filepath.Join("public", "help", "screenshots", name))
		if err != nil || sha256.Sum256(actual) != sha256.Sum256(expected) {
			t.Fatalf("image differs from source: %s: %v", path, err)
		}
		info, err := jpeg.DecodeConfig(bytes.NewReader(actual))
		if err != nil || info.Width != 1440 || info.Height != 960 {
			t.Fatalf("invalid screenshot: %s: %#v %v", path, info, err)
		}
		request := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "image/jpeg") || !bytes.Equal(response.Body.Bytes(), actual) {
			t.Fatalf("embedded HTTP image failed: %s: %d %s", path, response.Code, response.Header().Get("Content-Type"))
		}
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`src="(/assets/[^"\s]+\.js)"`).FindSubmatch(index)
	if len(entry) != 2 {
		t.Fatal("missing Vite entrypoint")
	}
	entryJS, err := fs.ReadFile(assets, strings.TrimPrefix(string(entry[1]), "/"))
	if err != nil {
		t.Fatal(err)
	}
	chunk := regexp.MustCompile(`HelpPage-[A-Za-z0-9_-]+\.js`).Find(entryJS)
	if len(chunk) == 0 {
		t.Fatal("help route chunk absent from the embedded entrypoint")
	}
	helpJS, err := fs.ReadFile(assets, "assets/"+string(chunk))
	if err != nil || !bytes.Contains(helpJS, []byte("help/screenshots/")) {
		t.Fatalf("help page/image references not embedded: %v", err)
	}
	t.Logf("embedded HelpPage route and %d source-matched JPEGs passed HTTP decoding checks", len(seen))
}
