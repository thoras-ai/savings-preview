package report

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

const page = "<html><body><main></main>\n<script>var x = 1;</script>\n</body></html>"

func get(t *testing.T, url string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestServe(t *testing.T) {
	s, err := Serve(page, []byte(`{"resourceMetrics":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if !strings.HasPrefix(s.URL, "http://127.0.0.1:") {
		t.Fatalf("URL %s is not on loopback", s.URL)
	}
	if code, ct, body := get(t, s.URL); code != 200 || !strings.HasPrefix(ct, "text/html") || body != page {
		t.Fatalf("page: %d %s %q", code, ct, body)
	}
	if code, ct, body := get(t, s.URL+DataName); code != 200 || ct != "application/json" || body != `{"resourceMetrics":[]}` {
		t.Fatalf("data: %d %s %q", code, ct, body)
	}
	if code, _, _ := get(t, s.URL+"etc/passwd"); code != 404 {
		t.Fatalf("other path: %d, want 404", code)
	}
}

func TestInlineEscapesData(t *testing.T) {
	data := []byte("{\n \"name\": \"</script><b>&\"\n}")
	out, err := Inline(page, data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "</script>") != 2 {
		t.Fatalf("data can close the script element:\n%s", out)
	}
	start := strings.Index(out, `id="report-data">`) + len(`id="report-data">`)
	end := strings.Index(out[start:], "</script>")
	var got map[string]string
	if err := json.Unmarshal([]byte(out[start:start+end]), &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "</script><b>&" {
		t.Fatalf("round trip: %q", got["name"])
	}
	if !strings.Contains(out, "</script>\n<script>var x") {
		t.Fatal("data element must come before the page script")
	}
	if _, err := Inline(page, []byte("not json")); err == nil {
		t.Fatal("want an error for invalid JSON")
	}
}

func TestWriteZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.zip")
	want := map[string]string{"a.json": "{}", "report.html": "<html></html>"}
	if err := WriteZip(path, []Entry{{"a.json", []byte("{}")}, {"report.html", []byte("<html></html>")}}); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != len(want) {
		t.Fatalf("%d files, want %d", len(zr.File), len(want))
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if want[f.Name] != string(b) {
			t.Fatalf("%s = %q", f.Name, b)
		}
	}
}
