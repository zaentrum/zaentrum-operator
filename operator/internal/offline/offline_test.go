package offline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zaentrum/zaentrum-operator/operator/internal/updates"
)

// reachOut makes TestARequestOutFailsTheRun, run again by itself, ask for the
// URL it holds.
const reachOut = "ZAENTRUM_TEST_OFFLINE_REACH_OUT"

func TestMain(m *testing.M) { os.Exit(Run(m)) }

// A client without a transport of its own, as a reconciler without a
// ReleasesURL has, reads the stand-in: the channels as published on 2026-11-16.
func TestThePublishedChannelsAreTheStandIn(t *testing.T) {
	rel, err := updates.Client{}.Fetch(context.Background(), "")
	require.NoError(t, err)
	stable, err := rel.Resolve("stable")
	require.NoError(t, err)
	assert.Equal(t, "v0.4.0", stable)
	edge, err := rel.Resolve("edge")
	require.NoError(t, err)
	assert.Equal(t, updates.Latest, edge)
}

// The tests' own servers are on this machine, and reached as before.
func TestThisMachineIsReached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "here")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "here", string(body))

	for _, host := range []string{"127.0.0.1", "127.0.0.53", "::1", "localhost", "LocalHost"} {
		assert.True(t, onThisMachine(host), host)
	}
	for _, host := range []string{"raw.githubusercontent.com", "example.org", "10.0.0.1", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "::", "fe80::1", ""} {
		assert.False(t, onThisMachine(host), host)
	}
}

// A test that asks for an address outside this machine fails the run, though
// it made nothing of the error it got: this binary again, running that test
// alone, exits 1 and names the request and the test.
func TestARequestOutFailsTheRun(t *testing.T) {
	if url := os.Getenv(reachOut); url != "" {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
		}
		return
	}
	const url = "https://example.invalid/releases.json"
	cmd := exec.Command(os.Args[0], "-test.run=^TestARequestOutFailsTheRun$")
	cmd.Env = append(os.Environ(), reachOut+"="+url)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "the run did not fail (%v):\n%s", err, out)
	assert.Equal(t, 1, exit.ExitCode(), "%s", out)
	assert.Contains(t, string(out), "PASS\n", "the test itself passes")
	assert.Contains(t, string(out), "offline: refused GET "+url+" by TestARequestOutFailsTheRun\n")
}
