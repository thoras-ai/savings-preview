// Package confirm prints what a command is about to do and waits for Enter
// before it does it, the way run does after its plan.
package confirm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Interactive reports whether a command may wait for Enter: only when stdin,
// stdout and stderr are all terminals, so scripts, pipes, CI and a redirected
// log never wait.
func Interactive(files ...*os.File) bool {
	for _, f := range files {
		if !term.IsTerminal(int(f.Fd())) {
			return false
		}
	}
	return true
}

// Ask waits for Enter, q or Ctrl-C, and reports whether to go on. The end of
// the input quits.
func Ask(ctx context.Context, in io.Reader, w io.Writer) bool {
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(w, "\nPress Enter to continue, or q to quit. ")
		line := make(chan string, 1)
		go func() {
			s, err := r.ReadString('\n')
			if err != nil && s == "" {
				s = "q"
			}
			line <- s
		}()
		select {
		case <-ctx.Done():
			fmt.Fprintln(w)
			return false
		case s := <-line:
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "", "y", "yes":
				return true
			case "q", "quit", "n", "no":
				return false
			}
		}
	}
}

// Plan is what a command is about to do.
type Plan struct {
	// Title names the command and its target, such as "savings-preview
	// prometheus, context prod-us".
	Title string
	// Intro is the line before Reads. Empty means the line for a command
	// that reads clusters: it reads only and changes nothing in them.
	Intro string
	// Reads lists the requests it makes, one line each.
	Reads []string
	// Writes says what it writes and where, or is empty when it writes
	// nothing.
	Writes string
	// After says what happens at the end, if anything worth saying.
	After string
}

// Gate prints p on w and, when ask is set, waits for Enter. It reports
// whether the command goes on; false means the user quit and nothing was
// done.
func Gate(ctx context.Context, p Plan, in io.Reader, w io.Writer, ask bool) bool {
	fmt.Fprintln(w, p.Title)
	fmt.Fprintln(w)
	intro := p.Intro
	if intro == "" {
		intro = "This command only reads; it creates, changes and deletes nothing in your clusters. It will:"
	}
	fmt.Fprintln(w, intro)
	for _, r := range p.Reads {
		fmt.Fprintln(w, "  - "+r)
	}
	if p.Writes != "" {
		fmt.Fprintln(w, p.Writes+" Nothing is uploaded.")
	} else {
		fmt.Fprintln(w, "It writes nothing and uploads nothing.")
	}
	if p.After != "" {
		fmt.Fprintln(w, p.After)
	}
	if !ask {
		fmt.Fprintln(w)
		return true
	}
	if !Ask(ctx, in, w) {
		fmt.Fprintln(w, "Stopped; nothing was read.")
		return false
	}
	fmt.Fprintln(w)
	return true
}

// Stdio is Gate on the process's own terminal: it waits only when stdin,
// stdout and stderr are terminals and yes is not set, and prints on stderr.
func Stdio(ctx context.Context, p Plan, yes bool) bool {
	return Gate(ctx, p, os.Stdin, os.Stderr, !yes && Interactive(os.Stdin, os.Stdout, os.Stderr))
}
