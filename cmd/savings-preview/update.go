package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// latestReleaseURL redirects to the page of the newest release, whose path
// ends in the tag. Reading the redirect needs no token and does not count
// against the GitHub API's hourly limit.
const latestReleaseURL = "https://github.com/thoras-ai/savings-preview/releases/latest"

// noUpdateCheckEnv turns the update check off when set to anything.
const noUpdateCheckEnv = "SAVINGS_PREVIEW_NO_UPDATE_CHECK"

// viaNpxEnv is set by the npm package's launcher, so the notice can say how
// to upgrade.
const viaNpxEnv = "SAVINGS_PREVIEW_VIA_NPM"

const (
	updateTimeout = 3 * time.Second
	// updateWait bounds how long run and check wait at the end for a lookup
	// that has not finished. They start it first, so it has usually finished.
	updateWait = time.Second
)

// updateCheck is a lookup of the latest release running in the background.
type updateCheck struct {
	current string
	done    chan struct{}
	latest  string
}

// pendingUpdate is the lookup main started for run or check; nil when there
// is none.
var pendingUpdate *updateCheck

// startUpdateCheck looks up the latest release in the background, or returns
// nil when it should not: the build is not a release, the user turned the
// check off, or stderr is not a terminal, so scripts and CI make no extra
// request.
func startUpdateCheck(current string, tty bool, getenv func(string) string) *updateCheck {
	if !tty || getenv(noUpdateCheckEnv) != "" {
		return nil
	}
	if _, ok := parseRelease(current); !ok {
		return nil
	}
	u := &updateCheck{current: current, done: make(chan struct{})}
	go func() {
		defer close(u.done)
		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
		defer cancel()
		u.latest, _ = latestTag(ctx, http.DefaultTransport, latestReleaseURL)
	}()
	return u
}

// report prints a line on w when the lookup found a newer release. It waits
// up to wait for a lookup still running, and prints nothing on any failure.
func (u *updateCheck) report(w io.Writer, wait time.Duration, hint string) {
	if u == nil {
		return
	}
	select {
	case <-u.done:
	default:
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-u.done:
		case <-t.C:
			return
		}
	}
	if newer(u.latest, u.current) {
		fmt.Fprintf(w, "\nsavings-preview %s is available; this is %s. To upgrade: %s\n", u.latest, u.current, hint)
	}
}

// latestTag asks url for its redirect and returns the tag at the end of the
// target, such as v0.2.6.
func latestTag(ctx context.Context, rt http.RoundTripper, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "savings-preview/"+versionString())
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("latest release: %s with no redirect", resp.Status)
	}
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if _, ok := parseRelease(tag); !ok {
		return "", errors.New("latest release: unexpected redirect to " + loc)
	}
	return tag, nil
}

// parseRelease reads a release tag vMAJOR.MINOR.PATCH. Prereleases and dev
// builds do not parse, so they never get or cause a notice.
func parseRelease(tag string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if !strings.HasPrefix(tag, "v") || len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// newer reports whether release tag a is newer than release tag b.
func newer(a, b string) bool {
	va, ok1 := parseRelease(a)
	vb, ok2 := parseRelease(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range va {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}

// upgradeHint says how to upgrade the copy at exe: through npm when the
// launcher started it, Homebrew when it lives in a Cellar, else the install
// script.
func upgradeHint(exe string, getenv func(string) string) string {
	if getenv(viaNpxEnv) != "" {
		return "npx @thoras-ai/savings-preview@latest, or npm update -g @thoras-ai/savings-preview"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.Contains(filepath.ToSlash(exe), "/Cellar/savings-preview/") {
		return "brew upgrade savings-preview"
	}
	return "curl -fsSL https://github.com/thoras-ai/savings-preview/releases/latest/download/install.sh | sh"
}

// reportUpdate prints the notice for the lookup main started, if any.
func reportUpdate(w io.Writer) {
	if pendingUpdate == nil {
		return
	}
	exe, _ := os.Executable()
	pendingUpdate.report(w, updateWait, upgradeHint(exe, os.Getenv))
}
