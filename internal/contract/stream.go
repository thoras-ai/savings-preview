package contract

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

// Writer encodes a contract file one resource at a time, so a caller never
// holds every resource in memory. Resources come out in the order given.
type Writer struct {
	w      *bufio.Writer
	n      int
	closed bool
}

// NewWriter starts a file on w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 1<<20)}
}

// Resource appends one resource.
func (sw *Writer) Resource(r Resource) error {
	if sw.closed {
		return errors.New("contract writer is closed")
	}
	rm, err := r.encode()
	if err != nil {
		return err
	}
	b, err := json.Marshal(rm)
	if err != nil {
		return err
	}
	sep := ",\n"
	if sw.n == 0 {
		sep = "{\"resourceMetrics\":[\n"
	}
	sw.n++
	if _, err := sw.w.WriteString(sep); err != nil {
		return err
	}
	_, err = sw.w.Write(b)
	return err
}

// Close ends the file and flushes it.
func (sw *Writer) Close() error {
	if sw.closed {
		return nil
	}
	sw.closed = true
	end := "\n]}\n"
	if sw.n == 0 {
		end = "{\"resourceMetrics\":[]}\n"
	}
	if _, err := sw.w.WriteString(end); err != nil {
		return err
	}
	return sw.w.Flush()
}
