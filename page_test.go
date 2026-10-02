package savingspreview

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportPageMatchesBuildScript(t *testing.T) {
	page, err := ReportPage()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "{{") {
		t.Fatal("page still has a template marker")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	if out, err := exec.Command("python3", "scripts/build_report.py", dir).CombinedOutput(); err != nil {
		t.Fatalf("build_report.py: %v\n%s", err, out)
	}
	want, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if page != string(want) {
		t.Fatal("ReportPage differs from scripts/build_report.py output; keep the two builders in step")
	}
}
