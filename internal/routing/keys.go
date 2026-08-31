package routing

// ctxKey is the type for context keys in this package.
type ctxKey string

const (
	KeyEndpoint      ctxKey = "gw.endpoint"
	KeyRetryAttempt  ctxKey = "gw.retry_attempt"
	KeyFallback      ctxKey = "gw.fallback"
	KeyBodyBytes     ctxKey = "gw.body_bytes"
	KeyMetaHolder    ctxKey = "gw.meta_holder"
	KeyPool          ctxKey = "gw.pool"
	KeyRoutedModel   ctxKey = "gw.routed_model"
	KeyRoutingReason ctxKey = "gw.routing_reason"
)

// Meta is a mutable holder for routing metadata. An outer middleware installs
// it via WithMeta before invoking the handler chain; the inner pool handler
// writes the selected endpoint/attempt/fallback into it.
type Meta struct {
	Endpoint      string
	Attempt       int
	Fallback      bool
	Pool          string
	RoutedModel   string
	RoutingReason string
}
