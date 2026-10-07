// Package fleet joins the contract files of several clusters into one file,
// as docs/contract.md describes for contract version 1.4.0.
package fleet

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

// Failed is a cluster that could not be read.
type Failed struct {
	Cluster string
	Reason  string
}

// Merge writes one contract file to w holding every resource of the files at
// paths, in order, and a cluster resource for each failed cluster. Each file
// is read one resource at a time. With more than one file, or any failed
// cluster, every resource is written with contract.FleetScopeVersion; the
// files must each hold different clusters.
func Merge(w io.Writer, paths []string, failed []Failed) error {
	fleet := len(paths) > 1 || len(failed) > 0
	bw := bufio.NewWriterSize(w, 1<<20)
	n := 0
	put := func(b []byte) error {
		sep := ",\n"
		if n == 0 {
			sep = "{\"resourceMetrics\":[\n"
		}
		n++
		if _, err := bw.WriteString(sep); err != nil {
			return err
		}
		_, err := bw.Write(b)
		return err
	}
	for _, p := range paths {
		if err := eachResource(p, func(rm *resource) error {
			for i := range rm.ScopeMetrics {
				if fleet {
					rm.ScopeMetrics[i].Scope.Version = contract.FleetScopeVersion
				}
			}
			b, err := json.Marshal(rm)
			if err != nil {
				return err
			}
			return put(b)
		}); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	if len(failed) > 0 {
		var buf bytesWriter
		cw := contract.NewWriter(&buf)
		cw.Version = contract.FleetScopeVersion
		for _, f := range failed {
			if err := cw.Resource(contract.Resource{Common: contract.Common{Cluster: f.Cluster}, Error: f.Reason}); err != nil {
				return err
			}
		}
		if err := cw.Close(); err != nil {
			return err
		}
		var doc struct{ ResourceMetrics []json.RawMessage }
		if err := json.Unmarshal(buf, &doc); err != nil {
			return err
		}
		for _, rm := range doc.ResourceMetrics {
			if err := put(rm); err != nil {
				return err
			}
		}
	}
	end := "\n]}\n"
	if n == 0 {
		end = "{\"resourceMetrics\":[]}\n"
	}
	if _, err := bw.WriteString(end); err != nil {
		return err
	}
	return bw.Flush()
}

type bytesWriter []byte

func (b *bytesWriter) Write(p []byte) (int, error) { *b = append(*b, p...); return len(p), nil }

// resource keeps a resourceMetrics entry as it was read, apart from the scope
// version.
type resource struct {
	Resource     json.RawMessage `json:"resource"`
	SchemaURL    string          `json:"schemaUrl"`
	ScopeMetrics []scopeMetrics  `json:"scopeMetrics"`
}

type scopeMetrics struct {
	Scope struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"scope"`
	SchemaURL string          `json:"schemaUrl"`
	Metrics   json.RawMessage `json:"metrics"`
}

func eachResource(path string, fn func(*resource) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	if tok, err := dec.Token(); err != nil {
		return err
	} else if tok != json.Delim('{') {
		return errors.New("not a contract file")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		if key != "resourceMetrics" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if tok, err := dec.Token(); err != nil {
			return err
		} else if tok != json.Delim('[') {
			return errors.New("resourceMetrics is not an array")
		}
		for dec.More() {
			var rm resource
			if err := dec.Decode(&rm); err != nil {
				return err
			}
			if err := fn(&rm); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	return nil
}
