package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/polling"
	"github.com/thoras-ai/savings-preview/internal/compact"
)

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	compactFlag := fs.Bool("compact", false, "write the file at 15-minute steps that keep the peak and the mean of usage")
	out := fs.String("o", "", "file to write (default: next to the input, named NAME.otlp.json, or NAME.compact.otlp.json with --compact)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: savings-preview export [--compact] [-o FILE] INPUT

Writes a contract file from INPUT, which is a contract file, the readings
directory of a poll (FILE.readings), or a report.html that carries its data,
like the one in the zip that run writes. With --compact the file has one point
per 15 minutes holding the mean and the peak of usage, and requests, limits
and replica counts only where they change. The target is taken from 15-minute
peaks, so its totals come within about 1% of the full file's, and it is small
enough for the report page. INPUT is read as a stream, so its size does not
matter. Without --compact, a readings directory is written at full resolution
and a report page's data is written as it is.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("export takes one INPUT")
	}
	in := fs.Arg(0)
	fi, err := os.Stat(in)
	if err != nil {
		return err
	}
	readings := fi.IsDir()
	page := !readings && isPage(in)
	if !readings && !page && !*compactFlag {
		return fmt.Errorf("%s is already a contract file; add --compact to write a compact copy", in)
	}
	if *out == "" {
		base := strings.TrimSuffix(in, string(filepath.Separator))
		switch {
		case readings:
			base = strings.TrimSuffix(base, ".readings")
		case page:
			base = strings.TrimSuffix(base, filepath.Ext(base)) + ".otlp.json"
		}
		if *compactFlag {
			*out = compactName(base)
		} else {
			*out = base
		}
	}
	if same, _ := sameFile(in, *out); same {
		return fmt.Errorf("-o %s would overwrite the input", *out)
	}

	start := time.Now()
	if page && !*compactFlag {
		f, err := os.Open(in)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := replaceFile(*out, func(w io.Writer) error { return pageData(bufio.NewReader(f), w) }); err != nil {
			return fmt.Errorf("%s: %w", in, err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s from the data in %s\n", *out, in)
		return nil
	}
	if !*compactFlag {
		var sum polling.Summary
		err := replaceFile(*out, func(w io.Writer) error {
			var err error
			sum, err = polling.ExportReadings(in, w)
			return err
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s from %d readings\n", *out, sum.Readings)
		return nil
	}
	var st compact.Stats
	switch {
	case readings:
		st, err = compactReadings(in, *out)
	case page:
		st, err = compactStream(*out, func(w io.Writer) error {
			f, err := os.Open(in)
			if err != nil {
				return err
			}
			defer f.Close()
			return pageData(bufio.NewReader(f), w)
		})
	default:
		st, err = compactFile(in, *out)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, compactReport(in, *out, st, time.Since(start)))
	return nil
}

// compactName is the compact file written next to out.
func compactName(out string) string {
	base := strings.TrimSuffix(out, filepath.Ext(out))
	base = strings.TrimSuffix(base, ".otlp")
	return base + ".compact.otlp.json"
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// compactFile writes the compact copy of the contract file in to out.
func compactFile(in, out string) (compact.Stats, error) {
	f, err := os.Open(in)
	if err != nil {
		return compact.Stats{}, err
	}
	defer f.Close()
	var st compact.Stats
	err = replaceFile(out, func(w io.Writer) error {
		var err error
		st, err = compact.Compact(f, w, compact.DefaultStep)
		if err != nil {
			return fmt.Errorf("compact %s: %w", in, err)
		}
		return nil
	})
	return st, err
}

// compactReadings writes the compact file for a poll's readings directory to
// out, streaming the full file through memory without writing it.
func compactReadings(dir, out string) (compact.Stats, error) {
	return compactStream(out, func(w io.Writer) error {
		_, err := polling.ExportReadings(dir, w)
		return err
	})
}

// compactStream writes the compact copy of the contract file that produce
// writes, without holding or storing that file.
func compactStream(out string, produce func(io.Writer) error) (compact.Stats, error) {
	var st compact.Stats
	err := replaceFile(out, func(w io.Writer) error {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := produce(pw)
			pw.CloseWithError(err)
			done <- err
		}()
		var err error
		st, err = compact.Compact(pr, w, compact.DefaultStep)
		if err != nil {
			// Unblocks the export, whose own error is then only a consequence.
			pr.CloseWithError(err)
			<-done
			return err
		}
		// The export may still be writing the end of the file.
		if _, err := io.Copy(io.Discard, pr); err != nil {
			return err
		}
		return <-done
	})
	return st, err
}

func compactReport(in, out string, st compact.Stats, took time.Duration) string {
	size := func(p string) string {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			return ""
		}
		return " (" + megabytes(fi.Size()) + ")"
	}
	return fmt.Sprintf("wrote %s%s from %s%s in %s: %s", out, size(out), in, size(in), took.Round(time.Second), compactDetails(st))
}

// compactDetails says how many resources Compact wrote and how.
func compactDetails(st compact.Stats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d resources", st.Resources)
	if st.Compacted > 0 {
		from := shortDur(time.Duration(st.FinestStep) * time.Second)
		if st.SourceStep != st.FinestStep {
			from += " to " + shortDur(time.Duration(st.SourceStep)*time.Second)
		}
		fmt.Fprintf(&b, ", %d at %s steps from %s data", st.Compacted, shortDur(time.Duration(st.Step)*time.Second), from)
	}
	if st.Unchanged > 0 {
		fmt.Fprintf(&b, ", %d already at %s or coarser and copied as they were", st.Unchanged, shortDur(time.Duration(st.Step)*time.Second))
	}
	return b.String()
}

func megabytes(n int64) string {
	if n < 1e6 {
		return fmt.Sprintf("%.0f KB", float64(n)/1e3)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

const (
	dataStart = `<script type="application/json" id="report-data">`
	dataEnd   = "</script>"
)

// isPage reports whether the file at path is an HTML page rather than JSON.
func isPage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b, _ := bufio.NewReader(f).Peek(512)
	return bytes.HasPrefix(bytes.TrimLeft(b, " \t\r\n"), []byte("<"))
}

// pageData copies the contract file a report page carries to w. Its \u
// escapes are valid JSON as they stand.
func pageData(r *bufio.Reader, w io.Writer) error {
	if err := skipPast(r, dataStart); err != nil {
		return errors.New("not a report page that carries its data")
	}
	bw := bufio.NewWriterSize(w, 1<<16)
	matched := 0
	for {
		c, err := r.ReadByte()
		if err != nil {
			return errors.New("the data in the page is not complete")
		}
		if c == dataEnd[matched] {
			if matched++; matched == len(dataEnd) {
				return bw.Flush()
			}
			continue
		}
		// The end tag starts with its only '<', so a failed match restarts at 0.
		bw.WriteString(dataEnd[:matched])
		matched = 0
		if c == dataEnd[0] {
			matched = 1
			continue
		}
		bw.WriteByte(c)
	}
}

// skipPast reads r up to and including the first occurrence of marker, which
// has no '<' after its first byte.
func skipPast(r *bufio.Reader, marker string) error {
	matched := 0
	for matched < len(marker) {
		c, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch {
		case c == marker[matched]:
			matched++
		case c == marker[0]:
			matched = 1
		default:
			matched = 0
		}
	}
	return nil
}
