package confirm

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

var p = Plan{
	Title:  "savings-preview check, context prod",
	Reads:  []string{"ask the API server who you are", "read no usage history"},
	Writes: "It writes x.json, in /tmp.",
	After:  "Then it exits.",
}

func TestGatePrintsThePlanAndGoesOnWithoutAsking(t *testing.T) {
	var w bytes.Buffer
	if !Gate(context.Background(), p, strings.NewReader(""), &w, false) {
		t.Fatal("Gate stopped without asking")
	}
	out := w.String()
	for _, want := range []string{
		"savings-preview check, context prod\n",
		"It will:\n  - ask the API server who you are\n  - read no usage history\n",
		"It writes x.json, in /tmp. Nothing is uploaded.\n",
		"Then it exits.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Press Enter") {
		t.Errorf("asked without ask:\n%s", out)
	}
}

func TestGateSaysWhenItWritesNothing(t *testing.T) {
	var w bytes.Buffer
	Gate(context.Background(), Plan{Title: "t", Reads: []string{"r"}}, strings.NewReader(""), &w, false)
	if !strings.Contains(w.String(), "It writes nothing and uploads nothing.") {
		t.Errorf("output:\n%s", w.String())
	}
}

func TestGateAsks(t *testing.T) {
	for in, want := range map[string]bool{"\n": true, "y\n": true, "q\n": false, "no\n": false, "": false, "huh\n\n": true} {
		var w bytes.Buffer
		got := Gate(context.Background(), p, strings.NewReader(in), &w, true)
		if got != want {
			t.Errorf("input %q: Gate = %v, want %v", in, got, want)
		}
		if !strings.Contains(w.String(), "Press Enter to continue, or q to quit.") {
			t.Errorf("input %q: no prompt:\n%s", in, w.String())
		}
		if !want && !strings.Contains(w.String(), "Stopped; nothing was read.") {
			t.Errorf("input %q: no stop line:\n%s", in, w.String())
		}
	}
}

func TestGateQuitsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var w bytes.Buffer
	r := blockReader{}
	if Gate(ctx, p, r, &w, true) {
		t.Fatal("Gate went on after cancel")
	}
}


// blockReader never returns, like a terminal nobody types in.
type blockReader struct{}

func (blockReader) Read([]byte) (int, error) { select {} }
