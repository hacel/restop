package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"restop/internal/restic"
)

const testSnapshotID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testServer(t *testing.T, body string, maxCommands, maxDownloads int) http.Handler {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := New(restic.New(executable, time.Second, maxCommands, maxDownloads), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

func fixtureServer(t *testing.T) http.Handler {
	t.Helper()
	return testServer(t, `case "$1" in
snapshots)
  printf '%s' '[{"time":"2024-01-01T00:00:00Z","hostname":"host<script>","paths":["/data&more"],"tags":["daily"],"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","short_id":"aaaaaaaa","summary":{"total_bytes_processed":2048}}]'
  ;;
ls)
  printf '%s\n' '{"struct_type":"snapshot","id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","short_id":"aaaaaaaa","hostname":"host<script>","time":"2024-01-01T00:00:00Z","summary":{"total_bytes_processed":2048}}'
  if [ "$4" = "/" ]; then
    printf '%s\n' \
      '{"struct_type":"node","name":"/","type":"dir","path":"/"}' \
      '{"struct_type":"node","name":"z-file.txt","type":"file","path":"/z-file.txt","size":7,"mtime":"2024-01-02T03:04:05Z"}' \
      '{"struct_type":"node","name":"a&b","type":"dir","path":"/a&b"}' \
      '{"struct_type":"node","name":"odd \" name.txt","type":"file","path":"/odd \" name.txt","size":7}'
  else
    printf '%s\n' '{"struct_type":"node","name":"a&b","type":"dir","path":"/a&b"}'
  fi
  ;;
dump)
  printf payload
  ;;
*) exit 2 ;;
esac
`, 4, 2)
}

func request(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func TestSnapshotsPageEscapesAndEnhancesLinks(t *testing.T) {
	response := request(t, fixtureServer(t), "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{"ago", "host&lt;script&gt;", "/data&amp;more", "2.0 KiB", "hx-get=", "class=\"row-link\"", "href=\"/snapshots/" + testSnapshotID} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response missing %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "host<script>") {
		t.Fatal("hostile hostname was not escaped")
	}
}

func TestDirectorySortsAndRoundTripsNames(t *testing.T) {
	response := request(t, fixtureServer(t), "/snapshots/"+testSnapshotID+"?path=%2F")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "ago") {
		t.Fatalf("modified timestamp was not humanized: %s", body)
	}
	if strings.Index(body, "a&amp;b") > strings.Index(body, "z-file.txt") {
		t.Fatal("directory was not sorted before files")
	}
	if !strings.Contains(body, "path=%2Fa%26b") {
		t.Fatalf("encoded path was not preserved: %s", body)
	}
	if !strings.Contains(body, `class="row-link file-row-link" href="/snapshots/`+testSnapshotID+`/download?path=%252Fz-file.txt"`) {
		t.Fatalf("file row does not link to its download: %s", body)
	}
	if !strings.Contains(body, `class="row-link" href="/snapshots/`+testSnapshotID+`?path=%252Fa%2526b"`) {
		t.Fatalf("directory row does not link to its contents: %s", body)
	}
	if strings.Contains(body, "<th><span class=\"sr-only\">Actions</span></th>") || strings.Contains(body, "aria-label=\"Download ") {
		t.Fatalf("directory table still contains the download column: %s", body)
	}
	if !strings.Contains(body, "aria-current=\"page\">aaaaaaaa") {
		t.Fatal("snapshot breadcrumb is not accessible or escaped")
	}
	for _, expected := range []string{"Snapshot <code>aaaaaaaa</code>", "host&lt;script&gt;", "Created", "2.0 KiB"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("snapshot heading missing %q: %s", expected, body)
		}
	}
	if strings.Contains(body, ">Root<") {
		t.Fatal("snapshot breadcrumb still uses the root label")
	}
}

func TestValidationAndHealth(t *testing.T) {
	handler := fixtureServer(t)
	for _, target := range []string{
		"/snapshots/short?path=%2F",
		"/snapshots/" + testSnapshotID + "?path=relative",
		"/snapshots/" + testSnapshotID + "?path=%2F..%2Fsecret",
		"/snapshots/" + testSnapshotID + "?path=%2Fdouble%2F%2Fslash",
	} {
		if response := request(t, handler, target); response.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d", target, response.Code)
		}
	}
	response := request(t, handler, "/healthz")
	if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
		t.Fatalf("unexpected health response: %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("security headers were not applied")
	}
}

func TestFileDownloadHeadersAndBody(t *testing.T) {
	response := request(t, fixtureServer(t), "/snapshots/"+testSnapshotID+"/download?path=%2Fodd%2520%2522%2520name.txt")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unexpected content type %q", response.Header().Get("Content-Type"))
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, `filename="odd \" name.txt"`) {
		t.Fatalf("unsafe or missing disposition %q", disposition)
	}
	if response.Body.String() != "payload" {
		t.Fatalf("unexpected payload %q", response.Body.String())
	}
}

func TestRepositoryPathDecodesURLSymbols(t *testing.T) {
	for value, expected := range map[string]string{
		"/hash%23name":    "/hash#name",
		"/percent%25name": "/percent%name",
		"/plus%2Bname":    "/plus+name",
		"/plain+name":     "/plain+name",
	} {
		actual, err := cleanRepositoryPath(value)
		if err != nil {
			t.Errorf("cleanRepositoryPath(%q): %v", value, err)
		} else if actual != expected {
			t.Errorf("cleanRepositoryPath(%q) = %q, want %q", value, actual, expected)
		}
	}
}

func TestDirectoryDownloadIsTar(t *testing.T) {
	response := request(t, fixtureServer(t), "/snapshots/"+testSnapshotID+"/download?path=%2Fa%26b")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("unexpected content type %q", response.Header().Get("Content-Type"))
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.Contains(disposition, "a&b-aaaaaaaa.tar") {
		t.Fatalf("unexpected disposition %q", disposition)
	}
}

func TestBusyMapsToServiceUnavailable(t *testing.T) {
	handler := testServer(t, "exec sleep 5\n", 1, 1)
	started := make(chan struct{})
	finished := make(chan *httptest.ResponseRecorder)
	go func() {
		close(started)
		finished <- request(t, handler, "/")
	}()
	<-started
	time.Sleep(30 * time.Millisecond)
	response := request(t, handler, "/")
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("unexpected busy response: %d", response.Code)
	}
	<-finished
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	response := request(t, fixtureServer(t), "/assets/htmx.min.js")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "htmx") {
		t.Fatalf("HTMX asset unavailable: %d", response.Code)
	}
}
