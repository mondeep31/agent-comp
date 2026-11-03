package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mondeep31/agent-comp/internal/golden"
	"github.com/mondeep31/agent-comp/internal/metrics"
	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestMetrics(t *testing.T) {
	m := metrics.New()
	m.ObserveRequest("POST /v1/sandboxes", "POST", 201, 30*time.Millisecond)
	m.SandboxEvent(sandbox.Event{From: sandbox.StateCreating, To: sandbox.StateRunning, Warm: true, Elapsed: 12 * time.Millisecond})
	m.SandboxEvent(sandbox.Event{From: sandbox.StateCreating, To: sandbox.StateRunning, Fork: true, Elapsed: 90 * time.Millisecond})
	m.SandboxEvent(sandbox.Event{From: sandbox.StateRunning, To: sandbox.StatePaused, Reason: "idle"})
	key := golden.Key{Image: "base", Shape: golden.Shape{VCPU: 1, MemoryMiB: 256, DiskMiB: 2048}}
	m.Watch(
		func() map[sandbox.State]int { return map[sandbox.State]int{sandbox.StateRunning: 3} },
		func() []pool.Stats { return []pool.Stats{{Key: key, Target: 4, Ready: 2, Filling: 1}} },
	)

	out := scrape(t, m)
	for _, want := range []string{
		`agentcomp_api_requests_total{code="201",method="POST",route="POST /v1/sandboxes"} 1`,
		`agentcomp_sandbox_create_duration_seconds_count{source="warm"} 1`,
		`agentcomp_sandbox_create_duration_seconds_count{source="fork"} 1`,
		`agentcomp_sandbox_transitions_total{from="running",to="paused"} 1`,
		`agentcomp_sandboxes{state="running"} 3`,
		`agentcomp_sandboxes{state="paused"} 0`,
		`agentcomp_pool_ready{key="base/1c-256m-2048d"} 2`,
		`agentcomp_pool_target{key="base/1c-256m-2048d"} 4`,
		`go_goroutines`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}
