// Package metrics defines acd's Prometheus metrics. They are served on their
// own loopback listener, never through the tunnel.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mondeep31/agent-comp/internal/pool"
	"github.com/mondeep31/agent-comp/internal/sandbox"
)

const namespace = "agentcomp"

type Metrics struct {
	reg         *prometheus.Registry
	requests    *prometheus.CounterVec
	requestDur  *prometheus.HistogramVec
	transitions *prometheus.CounterVec
	createDur   *prometheus.HistogramVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m := &Metrics{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "api_requests_total",
			Help: "API requests by route pattern, method and status code.",
		}, []string{"route", "method", "code"}),
		requestDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "api_request_duration_seconds",
			Help:    "API request latency by route pattern (WebSocket and MCP streams included).",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 10, 30, 120},
		}, []string{"route", "method"}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "sandbox_transitions_total",
			Help: "Sandbox state transitions.",
		}, []string{"from", "to"}),
		createDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "sandbox_create_duration_seconds",
			Help:    "Time from create request to a configured sandbox, by source (warm pool, cold restore, fork).",
			Buckets: []float64{.005, .01, .015, .02, .03, .05, .075, .1, .15, .25, .5, 1, 5},
		}, []string{"source"}),
	}
	reg.MustRegister(m.requests, m.requestDur, m.transitions, m.createDur)
	return m
}

// Handler serves the metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

func (m *Metrics) ObserveRequest(route, method string, code int, d time.Duration) {
	m.requests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	m.requestDur.WithLabelValues(route, method).Observe(d.Seconds())
}

// SandboxEvent records a lifecycle event from the sandbox manager.
func (m *Metrics) SandboxEvent(e sandbox.Event) {
	m.transitions.WithLabelValues(string(e.From), string(e.To)).Inc()
	if e.From == sandbox.StateCreating && e.To == sandbox.StateRunning {
		source := "cold"
		switch {
		case e.Fork:
			source = "fork"
		case e.Warm:
			source = "warm"
		}
		m.createDur.WithLabelValues(source).Observe(e.Elapsed.Seconds())
	}
}

// Watch exports sandbox counts and pool levels, read at scrape time.
func (m *Metrics) Watch(sandboxes func() map[sandbox.State]int, pools func() []pool.Stats) {
	m.reg.MustRegister(&stateCollector{
		sandboxes: sandboxes,
		pools:     pools,
		count:     prometheus.NewDesc(namespace+"_sandboxes", "Sandboxes by state.", []string{"state"}, nil),
		ready:     prometheus.NewDesc(namespace+"_pool_ready", "Paused, prewarmed VMs ready to hand out.", []string{"key"}, nil),
		fill:      prometheus.NewDesc(namespace+"_pool_filling", "VMs being restored into the pool.", []string{"key"}, nil),
		want:      prometheus.NewDesc(namespace+"_pool_target", "Configured warm pool size.", []string{"key"}, nil),
	})
}

type stateCollector struct {
	sandboxes                func() map[sandbox.State]int
	pools                    func() []pool.Stats
	count, ready, fill, want *prometheus.Desc
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.count
	ch <- c.ready
	ch <- c.fill
	ch <- c.want
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.sandboxes()
	for _, s := range []sandbox.State{sandbox.StateCreating, sandbox.StateRunning, sandbox.StatePaused, sandbox.StateStopped} {
		ch <- prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, float64(counts[s]), string(s))
	}
	for _, p := range c.pools() {
		key := p.Key.String()
		ch <- prometheus.MustNewConstMetric(c.ready, prometheus.GaugeValue, float64(p.Ready), key)
		ch <- prometheus.MustNewConstMetric(c.fill, prometheus.GaugeValue, float64(p.Filling), key)
		ch <- prometheus.MustNewConstMetric(c.want, prometheus.GaugeValue, float64(p.Target), key)
	}
}
