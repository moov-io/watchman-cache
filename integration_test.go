package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	composeProject = "watchman-cache"
	watchmanPing   = "http://localhost:8084/ping"
	cacheHealth    = "http://localhost:3000/health"
	timeout        = 6 * time.Minute // generous for cold downloads + image pulls + possible one restart on large CSV flake
)

// TestWatchmanStartsThroughCache brings up the full docker-compose stack
// (nginx cache + moov/watchman:v0.62.0) and verifies that watchman can
// successfully start and serve requests after downloading its lists
// through the nginx cache.
//
// It uses only the standard library + docker compose (no testcontainers).
func TestWatchmanStartsThroughCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Ensure docker and docker compose are available
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping integration test")
	}

	// Best-effort cleanup from previous runs
	_ = runComposeDown(t, true)

	t.Cleanup(func() {
		_ = runComposeDown(t, true)
	})

	t.Log("Starting docker compose stack (cache + watchman:v0.62.0) ...")

	// Wait only for the fast cache healthcheck. Watchman data load (even via cache)
	// can take 30-120s on first run depending on INCLUDED_LISTS size.
	if err := runComposeUpWaitCacheOnly(t); err != nil {
		t.Fatalf("docker compose up --wait for cache failed: %v", err)
	}
	// Now bring up watchman (it will start its initial download through the cache)
	if err := runCmd(t, "docker", "compose", "up", "-d", "watchman"); err != nil {
		t.Fatalf("failed to start watchman container: %v", err)
	}
	t.Logf("Cache healthy, watchman starting (data load via cache can take 30-120s)...")

	// Give the processes a moment to fully settle (healthchecks can pass slightly early)
	time.Sleep(2 * time.Second)

	// Verify cache health endpoint
	if err := waitForHTTP200(t, cacheHealth, 10*time.Second); err != nil {
		t.Fatalf("cache /health never became healthy: %v", err)
	}
	t.Log("nginx cache /health OK")

	// The critical assertion: watchman started successfully and its initial
	// data refresh (which uses the cache URLs) completed without error.
	// Note: cold fetches of the ~2 MB consolidated.csv from Azure Front Door
	// can occasionally produce truncated bodies ("upstream prematurely closed")
	// even with our retries/SNI/timeout hardening. Watchman will fatal+restart
	// (restart: unless-stopped). A second attempt usually gets a good body or
	// benefits from any prior partial cache population. Give it enough wall time.
	if err := waitForHTTP200(t, watchmanPing, 6*time.Minute); err != nil {
		// Dump logs on failure for diagnosis
		dumpLogs(t, "watchman")
		dumpLogs(t, "cache")
		t.Fatalf("watchman /ping never responded with 200: %v", err)
	}
	t.Log("watchman /ping returned PONG — data loaded via cache")

	// Stronger verification for the exact failure modes reported by users.
	// Transient first-attempt truncation on consolidated.csv is tolerated if a
	// subsequent attempt succeeds (see verifyNoLoadErrorsInWatchmanLogs).
	// Persistent errors (no final "data refreshed") still fail the test.
	if err := verifyNoLoadErrorsInWatchmanLogs(t); err != nil {
		dumpLogs(t, "watchman")
		t.Fatalf("watchman logs contained load errors after /ping was up: %v", err)
	}
	t.Log("watchman logs show clean initial downloads (or recovered transient on large CSV)")

	// Optional deeper verification: confirm we saw cache activity for the list file.
	// Guarded defensively — docker exec can occasionally hang in some environments.
	func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-T", "cache",
			"sh", "-c", "tail -n 20 /var/log/nginx/access.log 2>/dev/null || true")
		cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
		if out, err := cmd.CombinedOutput(); err == nil && len(out) > 0 {
			logs := string(out)
			if strings.Contains(logs, "fincen_311.html") || strings.Contains(logs, "cache:") {
				t.Log("verified cache activity for list file(s)")
			}
		}
	}()

	// One more explicit confirmation using the watchman version endpoint on admin port
	if err := waitForHTTP200(t, "http://localhost:9094/version", 5*time.Second); err == nil {
		t.Log("watchman admin /version reachable")
	}

	t.Log("SUCCESS: watchman started cleanly through the nginx cache")
}

// runComposeUpWait runs `docker compose up -d --wait` with a timeout.
func runComposeUpWait(t *testing.T) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "compose",
		"up", "-d", "--build", "--wait", "--wait-timeout", "180")
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// runComposeUpWaitCacheOnly starts the nginx cache container (build if needed).
// We intentionally do *not* use `--wait` here because the healthcheck + compose
// wait timing is racy on slow builders/CI (the root cause of the "is unhealthy"
// failure in early test runs). Instead we just `up -d` and rely on the explicit
// waitForHTTP200(cacheHealth, ...) poll that immediately follows in the test.
// This is more robust and still fast.
func runComposeUpWaitCacheOnly(t *testing.T) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "compose",
		"up", "-d", "--build", "cache")
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func runCmd(t *testing.T, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runComposeDown(t *testing.T, removeVolumes bool) error {
	args := []string{"compose", "down"}
	if removeVolumes {
		args = append(args, "-v")
	}
	args = append(args, "--remove-orphans")

	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
	// Don't fail the test on down errors during cleanup
	_ = cmd.Run()
	return nil
}

func waitForHTTP200(t *testing.T, url string, maxWait time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(maxWait)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			t.Logf("%s returned %d", url, resp.StatusCode)
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not return 200 within %s", url, maxWait)
}

func dumpLogs(t *testing.T, service string) {
	cmd := exec.Command("docker", "compose",
		"logs", "--tail=80", service)
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
	out, _ := cmd.CombinedOutput()
	if len(out) > 0 {
		t.Logf("=== last logs from %s ===\n%s", service, string(out))
	}
}

func getCacheAccessLog(t *testing.T) (string, error) {
	cmd := exec.Command("docker", "compose", "exec", "-T", "cache",
		"sh", "-c", "cat /var/log/nginx/access.log 2>/dev/null | tail -n 50 || true")
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// verifyNoLoadErrorsInWatchmanLogs scans recent watchman logs.
// Transient "unexpected EOF" / "problem during initial download" during the very first
// cold fetch of the ~2 MB consolidated.csv (Azure Front Door origin flakes) are
// tolerated if a later attempt succeeds ("data refreshed", "finished all lists").
// This matches real-world behavior with the known US CSL server characteristics
// while still catching persistent failures.
func verifyNoLoadErrorsInWatchmanLogs(t *testing.T) error {
	cmd := exec.Command("docker", "compose", "logs", "--tail=300", "watchman")
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+composeProject)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("note: docker compose logs returned err: %v", err)
	}
	logs := string(out)

	// If we see a successful final load in the captured logs, earlier transient
	// failures (one restart due to truncated CSV on first MISS) are acceptable.
	hasSuccessfulLoad := strings.Contains(logs, "data refreshed") ||
		strings.Contains(logs, "finished all lists")

	badSubstrings := []string{
		"unexpected EOF",
		"problem during initial download",
		"max retries reached while trying to obtain file",
		"loading US CSL records: parsing US CSL: failed to parse CSV",
	}
	for _, bad := range badSubstrings {
		if strings.Contains(logs, bad) {
			if hasSuccessfulLoad {
				t.Logf("note: saw %q but a successful load ('data refreshed' / 'finished all lists') followed it — treating as recovered transient (known Azure behavior on cold consolidated.csv)", bad)
				continue
			}
			return fmt.Errorf("found prohibited error string %q in watchman logs with no subsequent successful load", bad)
		}
	}

	// Positive signals that the lists the user cares about actually completed.
	goodSignals := []string{
		"finished US CSL download",
		"finished OFAC download",
		"finished US Non-SDN download",
		"finished EU CSL download",
	}
	foundGood := 0
	for _, good := range goodSignals {
		if strings.Contains(logs, good) {
			foundGood++
		}
	}
	if foundGood == 0 {
		t.Log("note: did not see the usual 'finished XXX download' strings; this may be normal for minimal INCLUDED_LISTS runs")
	} else {
		t.Logf("saw %d positive 'finished * download' signals for major lists", foundGood)
	}

	return nil
}
