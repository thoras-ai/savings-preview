package main

import (
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
	out := fs.String("o", "", "file to write (default: next to the input, named NAME.compact.otlp.json with --compact)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: savings-preview export [--compact] [-o FILE] INPUT

Writes a contract file from INPUT, which is a contract file or the readings
directory of a poll (FILE.readings). With --compact the file has one point per
15 minutes holding the mean and the peak of usage, and requests, limits and
replica counts only where they change. The target is taken from 15-minute
peaks, so its totals come within about 1% of the full file's, and it is small
enough for the report page. INPUT is read as a stream, so its size does not
matter. Without --compact, a readings directory is written at full resolution.

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
	if !readings && !*compactFlag {
		return fmt.Errorf("%s is already a contract file; add --compact to write a compact copy", in)
	}
	if *out == "" {
		base := strings.TrimSuffix(in, string(filepath.Separator))
		if readings {
			base = strings.TrimSuffix(base, ".readings")
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
	if readings {
		st, err = compactReadings(in, *out)
	} else {
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
	var st compact.Stats
	err := replaceFile(out, func(w io.Writer) error {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			_, err := polling.ExportReadings(dir, pw)
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
	var b strings.Builder
	fmt.Fprintf(&b, "wrote %s%s from %s%s in %s: %d resources", out, size(out), in, size(in), took.Round(time.Second), st.Resources)
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
