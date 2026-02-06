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

	// RedisCircuitBreakerState reports the current circuit breaker state.
	// 0 = closed (healthy), 1 = open (tripped/falling back to memory).
	RedisCircuitBreakerState = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "rlqs_redis_circuit_breaker_state",
		Help: "Redis circuit breaker state: 0=closed (healthy), 1=open (tripped)",
	})

	// StorageErrorsTotal counts storage operation errors by operation and error type.
	StorageErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rlqs_storage_errors_total",
		Help: "Total number of storage operation errors",
	}, []string{"operation", "error_type"})

	// RedisFallbackOperationsTotal counts operations routed to in-memory fallback.
	RedisFallbackOperationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "rlqs_redis_fallback_operations_total",
		Help: "Total number of operations routed to in-memory fallback due to Redis unavailability",
	})

	// StorageOperationDuration tracks the duration of storage operations.
	StorageOperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rlqs_storage_operation_duration_seconds",
		Help:    "Duration of storage operations in seconds",
		Buckets: []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
	}, []string{"operation"})
)
