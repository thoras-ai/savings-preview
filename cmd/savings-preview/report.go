package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thoras-ai/savings-preview/internal/compact"
	confirmpkg "github.com/thoras-ai/savings-preview/internal/confirm"
	"github.com/thoras-ai/savings-preview/internal/fleet"
	"github.com/thoras-ai/savings-preview/internal/report"
)

func runReport(args []string) error {
	return reportWith(args, os.Stdin, os.Stderr, interactive(os.Stdin, os.Stdout, os.Stderr))
}

// reportWith runs report with progress on w. When ask is set it waits on in
// for Enter after the plan.
func reportWith(args []string, in io.Reader, w io.Writer, ask bool) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(w)
	dir := fs.String("o", ".", "directory to write the compact file, the zip and each cluster's compact file to")
	noOpen := fs.Bool("no-open", false, "write the files and the zip, then exit without serving the report or opening a browser")
	fs.BoolVar(noOpen, "collect-only", false, "same as --no-open")
	yes := fs.Bool("yes", false, "start without waiting for Enter after printing the plan; for scripts")
	fs.BoolVar(yes, "y", false, "same as --yes")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: savings-preview report [-o DIR] [--no-open] [--yes] FILE...

Builds the report from contract files, without reading a cluster. Each FILE
is a contract file (.otlp.json) at full resolution or compact, of one cluster
or several, a report.html that carries its data, a zip that run wrote, or a
zip that holds one .otlp.json file. A file that is not yet compact is compacted as a stream, as export --compact
does, so its size does not matter; the full-resolution file is not rewritten.

One cluster gives a report of that cluster. Several clusters, from one file
or several, are joined into one file of several clusters, as run does with
--context given more than once; two files that hold the same cluster are an
error. The joined file is checked against the contract, and DIR gets the
files run writes: savings-preview.compact.otlp.json, savings-preview.zip with
summary.json and report.html, and for several clusters each cluster's compact
file in savings-preview.clusters. summary.json has no full_data, since report
writes no full-resolution file.

report prints its plan and waits for Enter when stdin, stdout and stderr are
terminals; --yes starts without waiting. Then it prints the totals and opens
the report on a local address, as run does. With --no-open it exits after
writing the files, for scripts.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("report takes at least one FILE")
	}
	inputs := fs.Args()
	var reads []string
	for _, p := range inputs {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory; for a poll's readings run savings-preview export --compact on it first", p)
		}
		reads = append(reads, fmt.Sprintf("read %s (%s)", p, fileSize(fi.Size())))
	}
	out := filepath.Join(*dir, report.DataName)
	abs, err := filepath.Abs(*dir)
	if err != nil {
		abs = *dir
	}
	after := "Then it opens the report in your browser."
	if *noOpen {
		after = "Then it exits. It does not open a browser or serve the report."
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	plan := confirmpkg.Plan{
		Title: fmt.Sprintf("savings-preview report, %d %s", len(inputs), plural(len(inputs), "file", "files")),
		Intro: "This command reads only local files; it connects to no cluster. It will:",
		Reads: reads,
		Writes: fmt.Sprintf("It writes %s and %s, and for several clusters each cluster's compact file in %s, in %s.",
			filepath.Base(compactName(out)), filepath.Base(zipName(out)), filepath.Base(clustersDir(out)), abs),
		After: after,
	}
	if !confirmpkg.Gate(ctx, plan, in, w, ask && !*yes) {
		return nil
	}
	b, err := buildFromFiles(inputs, out, w)
	if err != nil {
		return err
	}
	if *noOpen {
		return nil
	}
	stop()
	return serve(b.page, b.compact, true)
}

// buildFromFiles writes the compact file for out, the zip and, for several
// clusters, each cluster's compact file, from the contract files at inputs,
// and prints the totals. Each input is compacted as a stream; only the
// compact data is read whole.
func buildFromFiles(inputs []string, out string, w io.Writer) (*builtReport, error) {
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(dir, ".savings-preview-report-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	type part struct {
		input, compact string
		read           []string
		failed         []fleet.Failed
	}
	var parts []part
	from := map[string]string{} // cluster -> input
	for i, in := range inputs {
		p := part{input: in, compact: filepath.Join(work, fmt.Sprintf("%d.compact.otlp.json", i))}
		start := time.Now()
		st, err := compactInput(in, p.compact)
		if err != nil {
			return nil, err
		}
		if p.read, p.failed, err = fleet.Clusters(p.compact); err != nil {
			return nil, err
		}
		size := ""
		if fi, err := os.Stat(in); err == nil {
			size = " (" + fileSize(fi.Size()) + ")"
		}
		fmt.Fprintf(w, "read %s%s in %s: %s\n", in, size, time.Since(start).Round(time.Second), compactDetails(st))
		names := append([]string{}, p.read...)
		for _, f := range p.failed {
			names = append(names, f.Cluster)
		}
		for _, c := range names {
			if prev, ok := from[c]; ok {
				if prev == in {
					return nil, fmt.Errorf("%s holds cluster %q twice", in, c)
				}
				return nil, fmt.Errorf("cluster %q is in both %s and %s; give each cluster's data once", c, prev, in)
			}
			from[c] = in
		}
		parts = append(parts, p)
	}

	var read int
	var failed []fleet.Failed
	for _, p := range parts {
		read += len(p.read)
		failed = append(failed, p.failed...)
	}
	if read == 0 {
		return nil, errors.New("none of the files holds data for a cluster")
	}
	compacted := compactName(out)
	var data []bundleData
	single := read == 1 && len(failed) == 0
	if single {
		for _, p := range parts {
			if len(p.read) == 1 {
				if err := os.Rename(p.compact, compacted); err != nil {
					return nil, err
				}
			}
		}
		data = []bundleData{{Name: filepath.Base(compacted), Path: compacted}}
	} else {
		// Each cluster's compact file, joined as run joins a fleet's.
		cdir := clustersDir(out)
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			return nil, err
		}
		used := map[string]bool{}
		var files []string
		for _, p := range parts {
			for _, c := range p.read {
				path := filepath.Join(cdir, clusterFile(used, c)+".compact.otlp.json")
				if err := replaceFile(path, func(f io.Writer) error { return fleet.Extract(f, p.compact, c) }); err != nil {
					return nil, err
				}
				files = append(files, path)
				data = append(data, bundleData{Name: filepath.Base(cdir) + "/" + filepath.Base(path), Path: path})
			}
		}
		if err := replaceFile(compacted, func(f io.Writer) error { return fleet.Merge(f, files, failed) }); err != nil {
			return nil, err
		}
	}
	if err := checkContract(compacted, w); err != nil {
		return nil, err
	}
	b, err := finishReport(out, nil, data)
	if err != nil {
		return nil, err
	}
	if single {
		printCluster(w, b.summary)
		fmt.Fprintf(w, "wrote %s and %s with a summary, report.html, a copy of the report that opens without this tool, and the compact file\n", compacted, b.zip)
	} else {
		fmt.Fprintln(w)
		printFleet(w, b)
		fmt.Fprintf(w, "wrote %s and %s with a summary, report.html, a copy of the report that opens without this tool, and each cluster's compact file\n", compacted, b.zip)
		fmt.Fprintf(w, "each cluster's compact file is in %s\n", clustersDir(out))
	}
	return b, nil
}

// compactInput writes the compact copy of the contract file that in is or
// carries to out, reading in as a stream.
func compactInput(in, out string) (st compact.Stats, err error) {
	kind, err := inputKind(in)
	if err != nil {
		return st, err
	}
	switch kind {
	case "page":
		st, err = compactStream(out, func(w io.Writer) error {
			f, err := os.Open(in)
			if err != nil {
				return err
			}
			defer f.Close()
			return pageData(bufio.NewReader(f), w)
		})
	case "zip":
		st, err = compactStream(out, func(w io.Writer) error { return zipData(in, w) })
	default:
		return compactFile(in, out)
	}
	if err != nil {
		return st, fmt.Errorf("%s: %w", in, err)
	}
	return st, nil
}

// inputKind says whether the file at path is a contract file ("json"), a
// report page ("page") or a zip ("zip"), from its first bytes.
func inputKind(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, _ := bufio.NewReader(f).Peek(512)
	switch t := bytes.TrimLeft(b, " \t\r\n"); {
	case bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return "zip", nil
	case bytes.HasPrefix(t, []byte("{")):
		return "json", nil
	case bytes.HasPrefix(t, []byte("<")):
		return "page", nil
	}
	return "", fmt.Errorf("%s is not a contract file: it is not JSON, a report.html or a zip that savings-preview run wrote", path)
}

// zipData copies the contract file in a zip to w: the data that report.html
// carries in a zip that run wrote, or else the zip's one .otlp.json file.
func zipData(path string, w io.Writer) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	var page *zip.File
	var data []*zip.File
	for _, f := range zr.File {
		switch {
		case f.Name == "report.html":
			page = f
		case !strings.Contains(f.Name, "/") && strings.HasSuffix(f.Name, ".otlp.json"):
			data = append(data, f)
		}
	}
	f := page
	if f == nil {
		if len(data) != 1 {
			return errors.New("the zip has no report.html and not one .otlp.json file, so it holds no contract file to read")
		}
		f = data[0]
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if f == page {
		return pageData(bufio.NewReader(rc), w)
	}
	_, err = io.Copy(w, rc)
	return err
}

// fileSize prints a file size in MB, or GB from 1 GB.
func fileSize(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	}
	return megabytes(n)
}
