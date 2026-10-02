// Package report serves the report page next to a contract file and packages
// both for sharing.
package report

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DataName is the file name the page fetches by default.
const DataName = "savings-preview.otlp.json"

// Server serves the page and one contract file on a loopback address.
type Server struct {
	URL string
	srv *http.Server
	ln  net.Listener
}

// Serve listens on 127.0.0.1 on a random free port. The page is at "/" and
// the data at "/savings-preview.otlp.json".
func Serve(page string, data []byte) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, page)
	})
	mux.HandleFunc("GET /"+DataName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	})
	host := ln.Addr().String()
	// Answering only to our own address keeps a page that rebinds its DNS
	// name to 127.0.0.1 from reading the data.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	s := &Server{URL: "http://" + host + "/", ln: ln,
		srv: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}}
	go s.srv.Serve(ln)
	return s, nil
}

// Close stops the server.
func (s *Server) Close(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Inline returns a copy of the page that carries the contract file, so it
// opens from disk without a server.
func Inline(page string, data []byte) (string, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return "", fmt.Errorf("contract file is not valid JSON: %w", err)
	}
	// HTMLEscape rewrites <, > and & as \u escapes, so the data cannot end
	// the script element early.
	var esc bytes.Buffer
	json.HTMLEscape(&esc, compact.Bytes())
	marker := "<script>"
	i := strings.LastIndex(page, marker)
	if i < 0 {
		return "", errors.New("report page has no script element")
	}
	tag := `<script type="application/json" id="report-data">` + esc.String() + "</script>\n"
	return page[:i] + tag + page[i:], nil
}

// Entry is one file in the zip.
type Entry struct {
	Name string
	Data []byte
}

// WriteZip writes the entries to path, replacing any file already there.
func WriteZip(path string, entries []Entry) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".savings-preview-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	zw := zip.NewWriter(tmp)
	now := time.Now()
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: now})
		if err != nil {
			tmp.Close()
			return err
		}
		if _, err := w.Write(e.Data); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Open asks the desktop to open url in the default browser.
func Open(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
