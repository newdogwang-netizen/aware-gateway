package routing

import (
	"context"
	"net/http"
)

// WithMeta installs a fresh Meta holder and returns both the updated request
// and the holder. The caller keeps the holder to read final values after the
// handler completes.
func WithMeta(r *http.Request) (*http.Request, *Meta) {
	m := &Meta{}
	return r.WithContext(context.WithValue(r.Context(), KeyMetaHolder, m)), m
}

// MetaFrom returns the Meta holder from the request context, or nil.
func MetaFrom(r *http.Request) *Meta {
	m, _ := r.Context().Value(KeyMetaHolder).(*Meta)
	return m
}

// SetMeta records routing metadata into the context holder.
func SetMeta(r *http.Request, endpoint string, attempt int, fallback bool) {
	if m, ok := r.Context().Value(KeyMetaHolder).(*Meta); ok && m != nil {
		m.Endpoint = endpoint
		m.Attempt = attempt
		m.Fallback = fallback
	}
}

// SetPool records the pool name into the context holder.
func SetPool(r *http.Request, pool string) {
	if m, ok := r.Context().Value(KeyMetaHolder).(*Meta); ok && m != nil {
		m.Pool = pool
	}
}

// SetRoutedModel records the router plugin's model decision.
func SetRoutedModel(r *http.Request, model, reason string) {
	if m, ok := r.Context().Value(KeyMetaHolder).(*Meta); ok && m != nil {
		m.RoutedModel = model
		m.RoutingReason = reason
	}
}

// SetBodyBytes stores the buffered request body in context for reuse.
func SetBodyBytes(r *http.Request, body []byte) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), KeyBodyBytes, body))
}

// GetBodyBytes retrieves the buffered request body from context.
func GetBodyBytes(r *http.Request) []byte {
	b, _ := r.Context().Value(KeyBodyBytes).([]byte)
	return b
}

// GetRoutingMeta retrieves routing metadata from request context.
func GetRoutingMeta(r *http.Request) (endpoint string, attempt int, fallback string) {
	m := MetaFrom(r)
	if m != nil {
		if m.Fallback {
			return m.Endpoint, m.Attempt, "true"
		}
		return m.Endpoint, m.Attempt, ""
	}
	endpoint, _ = r.Context().Value(KeyEndpoint).(string)
	attempt, _ = r.Context().Value(KeyRetryAttempt).(int)
	fallback, _ = r.Context().Value(KeyFallback).(string)
	return
}
