package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	Namespace     = "aware_gateway"
	PoolLabel     = "pool"
	RouteLabel    = "route"
	EndpointLabel = "endpoint"
	ModelLabel    = "model"
	StatusLabel   = "status"
)

var (
	RequestTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "request_total",
			Help:      "Total number of proxied requests",
		},
		[]string{PoolLabel, RouteLabel, EndpointLabel, ModelLabel, StatusLabel},
	)

	RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "request_duration_seconds",
			Help:      "Proxy request latency in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{PoolLabel, RouteLabel, EndpointLabel},
	)

	LLMTTFT = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "llm_ttft_seconds",
			Help:      "Time-to-first-token for streamed LLM responses, in seconds",
			Buckets:   []float64{.025, .05, .1, .15, .2, .3, .4, .5, .75, 1, 1.5, 2, 3, 5},
		},
		[]string{PoolLabel, ModelLabel},
	)

	RetryTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "retry_total",
			Help:      "Total number of retry attempts",
		},
		[]string{PoolLabel, EndpointLabel},
	)

	FallbackTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "fallback_total",
			Help:      "Total number of fallback pool activations",
		},
		[]string{PoolLabel},
	)

	CircuitBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "circuit_breaker_state",
			Help:      "Circuit breaker state: 0=closed, 1=open, 2=half-open",
		},
		[]string{PoolLabel, EndpointLabel},
	)

	PoolEndpointsUp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "pool_endpoints_up",
			Help:      "Number of healthy endpoints in pool",
		},
		[]string{PoolLabel},
	)

	PoolEndpointsTotal = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "pool_endpoints_total",
			Help:      "Total number of endpoints in pool (up + down)",
		},
		[]string{PoolLabel},
	)

	TokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "tokens_total",
			Help:      "Token usage from LLM responses",
		},
		[]string{PoolLabel, EndpointLabel, ModelLabel, "token_type"},
	)

	ActiveRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "active_requests",
			Help:      "Number of in-flight requests per pool",
		},
		[]string{PoolLabel},
	)

	// RoutingDecisionTotal tracks how often each routing decision is made.
	RoutingDecisionTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "routing_decision_total",
			Help:      "Routing decisions by router plugin and reason",
		},
		[]string{"router", "pool", "model", "reason"},
	)

	// --- GenAI Semantic Convention Metrics (OTel GenAI / OpenInference) ---
	// These follow the OpenTelemetry GenAI semantic conventions for metrics,
	// enabling compatibility with backends that expect gen_ai.* metric names.

	// GenAIRequestTotal counts GenAI operations by system, operation, model.
	GenAIRequestTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "gen_ai_request_total",
			Help:      "Total GenAI requests by system, operation, model, status",
		},
		[]string{"gen_ai_system", "gen_ai_operation", "gen_ai_model", "status"},
	)

	// GenAITokensTotal tracks token usage by system, model, token_type.
	GenAITokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "gen_ai_tokens_total",
			Help:      "GenAI token usage (input + output) by system, model",
		},
		[]string{"gen_ai_system", "gen_ai_model", "token_type"}, // token_type: input|output
	)

	// GenAITTFTSeconds records time-to-first-token for streamed responses.
	GenAITTFTSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "gen_ai_ttft_seconds",
			Help:      "GenAI time-to-first-token for streamed responses, in seconds",
			Buckets:   []float64{.025, .05, .1, .15, .2, .3, .4, .5, .75, 1, 1.5, 2, 3, 5, 10},
		},
		[]string{"gen_ai_system", "gen_ai_model"},
	)

	// GenAIRequestDuration records GenAI request latency by system, model.
	GenAIRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "gen_ai_request_duration_seconds",
			Help:      "GenAI request latency in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"gen_ai_system", "gen_ai_operation", "gen_ai_model"},
	)

	// GenAICostTotal tracks estimated cost in USD by system, model.
	GenAICostTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "gen_ai_cost_total",
			Help:      "Estimated GenAI cost in USD",
		},
		[]string{"gen_ai_system", "gen_ai_model"},
	)
)

func BreakerState(state string) float64 {
	switch state {
	case "closed":
		return 0
	case "open":
		return 1
	case "half-open":
		return 2
	default:
		return -1
	}
}
