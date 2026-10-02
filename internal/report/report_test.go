package report

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
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
	data := filepath.Join(t.TempDir(), DataName)
	if err := os.WriteFile(data, []byte(`{"resourceMetrics":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Serve(page, data)
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
	req, _ := http.NewRequest("GET", s.URL+DataName, nil)
	req.Host = "attacker.example:" + strings.TrimSuffix(strings.SplitN(s.URL, ":", 3)[2], "/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host: %d, want 403", resp.StatusCode)
	}
}

// inline runs WriteInline, reading the data a byte at a time when oneByte is
// set, so multi-byte characters arrive split.
func inline(t *testing.T, data string, oneByte bool) string {
	t.Helper()
	var r io.Reader = strings.NewReader(data)
	if oneByte {
		r = iotest.OneByteReader(r)
	}
	var out strings.Builder
	if err := WriteInline(&out, page, r); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestInlineEscapesData(t *testing.T) {
	data := "{\n \"name\": \"</script><b>&\u2028\u2029\u00e9\u20ac\"\n}"
	for _, oneByte := range []bool{true, false} {
		out := inline(t, data, oneByte)
		if strings.Count(out, "</script>") != 2 || strings.Contains(out, "\u2028") {
			t.Fatalf("data can close the script element or the line:\n%s", out)
		}
		start := strings.Index(out, `id="report-data">`) + len(`id="report-data">`)
		end := strings.Index(out[start:], "</script>")
		var got map[string]string
		if err := json.Unmarshal([]byte(out[start:start+end]), &got); err != nil {
			t.Fatal(err)
		}
		if got["name"] != "</script><b>&\u2028\u2029\u00e9\u20ac" {
			t.Fatalf("round trip: %q", got["name"])
		}
		if !strings.Contains(out, "</script>\n<script>var x") {
			t.Fatal("data element must come before the page script")
		}
	}
}

func TestWriteZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.zip")
	want := map[string]string{"a.json": "{}", "report.html": "<html></html>"}
	entries := []Entry{{Name: "a.json", Data: []byte("{}")},
		{Name: "report.html", Write: func(w io.Writer) error { _, err := io.WriteString(w, "<html></html>"); return err }}}
	if err := WriteZip(path, "where the rest is", entries); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("zip mode: %v %v", fi.Mode(), err)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if zr.Comment != "where the rest is" {
		t.Errorf("comment %q", zr.Comment)
	}
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
