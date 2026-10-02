// Package report serves the report page next to a contract file and packages
// both for sharing.
package report

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
// the contract file at dataPath is served, read from disk on each request,
// at "/savings-preview.otlp.json".
func Serve(page, dataPath string) (*Server, error) {
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
		f, err := os.Open(dataPath)
		if err != nil {
			http.Error(w, "data file unavailable", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		io.Copy(w, f)
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

// WriteInline writes a copy of the page that carries the contract file read
// from data, so it opens from disk without a server. The data is copied as
// it is read, so it is never held in memory; it should already have passed
// the contract checks.
func WriteInline(w io.Writer, page string, data io.Reader) error {
	marker := "<script>"
	i := strings.LastIndex(page, marker)
	if i < 0 {
		return errors.New("report page has no script element")
	}
	if _, err := io.WriteString(w, page[:i]+`<script type="application/json" id="report-data">`); err != nil {
		return err
	}
	esc := &scriptEscaper{w: w}
	if _, err := io.Copy(esc, data); err != nil {
		return err
	}
	if err := esc.flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "</script>\n"+page[i:])
	return err
}

// scriptEscaper rewrites <, >, &, U+2028 and U+2029 as \u escapes, as
// json.HTMLEscape does, so JSON cannot end the script element early. In JSON
// those characters occur only inside strings, where the escapes mean the same.
type scriptEscaper struct {
	w     io.Writer
	carry []byte // the start of a UTF-8 sequence split across writes
}

func (e *scriptEscaper) Write(p []byte) (int, error) {
	n := len(p)
	if len(e.carry) > 0 {
		p = append(e.carry, p...)
		e.carry = nil
	}
	out := make([]byte, 0, len(p)+64)
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '<', '>', '&':
			out = append(out, `\u00`...)
			out = append(out, "0123456789abcdef"[c>>4], "0123456789abcdef"[c&0xf])
		case 0xe2:
			if len(p)-i < 3 {
				e.carry = append([]byte(nil), p[i:]...)
				i = len(p)
				continue
			}
			if p[i+1] == 0x80 && (p[i+2] == 0xa8 || p[i+2] == 0xa9) {
				out = append(out, `\u202`...)
				out = append(out, "89"[p[i+2]-0xa8])
				i += 2
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	if _, err := e.w.Write(out); err != nil {
		return 0, err
	}
	return n, nil
}

func (e *scriptEscaper) flush() error {
	_, err := e.w.Write(e.carry)
	e.carry = nil
	return err
}

// Entry is one file in the zip: Data, or what Write writes.
type Entry struct {
	Name  string
	Data  []byte
	Write func(io.Writer) error
}

// WriteZip writes the entries to path, replacing any file already there.
// comment, when set, is the archive comment that unzip -z shows.
func WriteZip(path, comment string, entries []Entry) error {
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
		write := e.Write
		if write == nil {
			write = func(w io.Writer) error { _, err := w.Write(e.Data); return err }
		}
		if err := write(w); err != nil {
			tmp.Close()
			return err
		}
	}
	if comment != "" {
		if err := zw.SetComment(comment); err != nil {
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
