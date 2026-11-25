package main

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// arrayMetrics count and time the controller's Storage Center REST calls.
type arrayMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newArrayMetrics(reg prometheus.Registerer) *arrayMetrics {
	m := &arrayMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sc_csi_array_requests_total",
			Help: "Storage Center REST calls by method, object path and HTTP status (0 = transport error)."},
			[]string{"method", "object", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sc_csi_array_request_duration_seconds", Help: "Storage Center REST call latency.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30}}, []string{"method", "object"}),
	}
	reg.MustRegister(m.requests, m.duration)
	return m
}

// observe is an sc.Observer.
func (m *arrayMetrics) observe(method, object string, status int, d time.Duration) {
	m.requests.WithLabelValues(method, object, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, object).Observe(d.Seconds())
}
