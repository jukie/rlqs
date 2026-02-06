package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ActiveStreams tracks the number of currently active gRPC streams.
	ActiveStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "rlqs_active_streams",
		Help: "Number of currently active rate limit quota streams",
	})

	// ActiveBuckets tracks the number of active buckets per domain.
	ActiveBuckets = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rlqs_active_buckets",
		Help: "Number of active rate limit buckets by domain",
	}, []string{"domain"})

	// UsageReportsTotal counts the total number of usage reports processed.
	UsageReportsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rlqs_usage_reports_total",
		Help: "Total number of usage reports processed by domain",
	}, []string{"domain"})

	// AssignmentsTotal counts the total number of quota assignments issued.
	AssignmentsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rlqs_assignments_total",
		Help: "Total number of quota assignments issued by domain",
	}, []string{"domain"})

	// EngineDuration tracks the time spent processing usage reports in the quota engine.
	EngineDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "rlqs_engine_duration_seconds",
		Help:    "Duration of quota engine processing in seconds",
		Buckets: prometheus.DefBuckets,
	})
)
