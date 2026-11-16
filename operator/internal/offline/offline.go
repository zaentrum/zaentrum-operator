// Package offline keeps a test binary off the network. Each package's TestMain
// runs its tests through Run:
//
//	func TestMain(m *testing.M) { os.Exit(offline.Run(m)) }
//
// Run replaces http.DefaultTransport, the transport of every client that names
// none (the release channels' updates.Client and the digest resolver among
// them), with a guard that
//
//   - answers updates.DefaultReleasesURL with a stand-in channel document,
//     releases.json beside this file. A reconciler built without a ReleasesURL
//     reads the published document, so a reconcile test would otherwise render
//     whatever the stable channel names on the day it runs. The stand-in holds
//     the channels as the published document had them on 2026-11-16, stable
//     v0.4.0 and edge latest, and a release does not change it. A test about
//     the channels serves a document of its own (controller/channels_test.go);
//   - passes a request for this machine (127.0.0.1, ::1, localhost: the tests'
//     httptest servers) on to the transport it replaced;
//   - refuses any other request with an error, and records it. After the
//     tests, a refused request fails the run, listed with the test that asked.
//
// Not covered: a client with a transport of its own (an httptest server's
// Client, the addon Fetcher's guarded transport), name lookups, and the
// programs a test runs.
package offline

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/zaentrum-operator/operator/internal/updates"
)

// releases stands in for the published channel document.
//
//go:embed releases.json
var releases []byte

// Run runs m's tests offline. It returns the code TestMain exits with: m.Run's,
// or 1 when a test asked for anything outside this machine.
func Run(m *testing.M) int {
	g := &guard{
		local:    http.DefaultTransport,
		standIns: map[string][]byte{updates.DefaultReleasesURL: releases},
		count:    map[refusal]int{},
	}
	http.DefaultTransport = g
	code := m.Run()
	refused, total := g.report()
	if total == 0 {
		return code
	}
	fmt.Fprintf(os.Stderr, "offline: %d request(s) for outside this machine refused; the tests read no network\n", total)
	for _, line := range refused {
		fmt.Fprintln(os.Stderr, "offline: refused "+line)
	}
	if code == 0 {
		code = 1
	}
	return code
}

// guard is the transport Run puts in http.DefaultTransport's place.
type guard struct {
	local    http.RoundTripper // the transport it replaced: this machine's requests go there
	standIns map[string][]byte // answered in place of the published documents, by URL

	mu      sync.Mutex
	refused []refusal // each one once, in the order first asked
	count   map[refusal]int
}

// refusal is a request refused, and the test that asked for it.
type refusal struct{ method, url, test string }

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if onThisMachine(req.URL.Hostname()) {
		return g.local.RoundTrip(req)
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if doc, ok := g.standIns[req.URL.String()]; ok && req.Method == http.MethodGet {
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(doc)),
			ContentLength: int64(len(doc)),
			Request:       req,
		}, nil
	}
	r := refusal{method: req.Method, url: req.URL.Redacted(), test: testName()}
	g.mu.Lock()
	if g.count[r]++; g.count[r] == 1 {
		g.refused = append(g.refused, r)
	}
	g.mu.Unlock()
	return nil, fmt.Errorf("offline: %s %s refused: the tests read no network", r.method, r.url)
}

// CloseIdleConnections passes on to the replaced transport: httptest's
// Server.Close asks http.DefaultTransport to close its idle connections.
func (g *guard) CloseIdleConnections() {
	if c, ok := g.local.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// report lists the refused requests, the same request of the same test once,
// and counts them all.
func (g *guard) report() (lines []string, total int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.refused {
		n := g.count[r]
		line := r.method + " " + r.url + " by " + r.test
		if n > 1 {
			line += fmt.Sprintf(" (%d times)", n)
		}
		lines = append(lines, line)
		total += n
	}
	return lines, total
}

// onThisMachine reports whether host is this machine, where the tests' httptest
// servers listen.
func onThisMachine(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// testName names the test whose goroutine asks: the outermost function on its
// stack whose name starts with Test, up to its first dot, so TestX for
// TestX.func1, a subtest's function.
func testName() string {
	pc := make([]uintptr, 256)
	frames := runtime.CallersFrames(pc[:runtime.Callers(3, pc)])
	name := "a goroutine of no test"
	for {
		f, more := frames.Next()
		fn := f.Function[strings.LastIndex(f.Function, "/")+1:] // controller.TestX.func1
		if _, inPkg, ok := strings.Cut(fn, "."); ok && strings.HasPrefix(inPkg, "Test") {
			name, _, _ = strings.Cut(inPkg, ".")
		}
		if !more {
			return name
		}
	}
}
