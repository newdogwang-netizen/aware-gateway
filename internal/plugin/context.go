package plugin

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/pool"
	"github.com/prometheus/client_golang/prometheus"
)

// Context is passed to Plugin.Init(). It provides access to gateway internals
// that plugins may need: config, logger, pool provider, metrics registry,
// and a shutdown channel for background goroutines.
type Context struct {
	Config   *config.Config
	Logger   *slog.Logger
	Pools    PoolProvider
	Metrics  *MetricsRegistry
	StopChan <-chan struct{} // closed when gateway is shutting down
}

// PoolProvider abstracts pool lookup so plugins can inspect available pools
// and endpoints without depending on the concrete pool manager.
type PoolProvider interface {
	Get(name string) (*pool.Pool, bool)
	All() map[string]*pool.Pool
}

// MetricsRegistry wraps prometheus.Registry and provides helper methods
// for plugins to register metrics without import-cycle concerns.
type MetricsRegistry struct {
	REG *prometheus.Registry
}

// Register registers a prometheus.Collector, ignoring already-registered errors.
func (m *MetricsRegistry) Register(c prometheus.Collector) error {
	if m == nil || m.REG == nil {
		return nil
	}
	return m.REG.Register(c)
}

// MustRegister panics on non-duplicate errors.
func (m *MetricsRegistry) MustRegister(c ...prometheus.Collector) {
	if m == nil || m.REG == nil {
		return
	}
	for _, cc := range c {
		if err := m.REG.Register(cc); err != nil {
			// Ignore already-registered — plugins may be re-initialized on hot reload
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// AuditRecord is the structured event passed to AuditSink.Record().
// Plugins that implement AuditSink receive this after each request completes.
type AuditRecord struct {
	TraceID       string    `json:"trace_id"`
	Timestamp     time.Time `json:"timestamp"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Endpoint      string    `json:"endpoint,omitempty"`
	Status        int       `json:"status"`
	LatencyMs     int64     `json:"latency_ms"`
	Model         string    `json:"model,omitempty"`
	RoutedModel   string    `json:"routed_model,omitempty"` // model after router plugin decision
	Pool          string    `json:"pool,omitempty"`
	PromptTokens  int       `json:"prompt_tokens,omitempty"`
	CompTokens    int       `json:"completion_tokens,omitempty"`
	TotalTokens   int       `json:"total_tokens,omitempty"`
	RetryAttempt  int       `json:"retry_attempt,omitempty"`
	Fallback      string    `json:"fallback,omitempty"`
	UserID        string    `json:"user_id,omitempty"`
	APIKey        string    `json:"api_key,omitempty"`
	Cost          float64   `json:"cost,omitempty"`
	RoutingReason string    `json:"routing_reason,omitempty"` // why this model was chosen
	ContentLength int64     `json:"content_length,omitempty"`
	Streaming     bool      `json:"streaming,omitempty"`
	FinishReason  string    `json:"finish_reason,omitempty"`
	ErrorKind     string    `json:"error_kind,omitempty"`

	// --- Task/Step correlation ---
	// Populated from request headers (X-Trial-Name, X-Step-Name, X-Session-ID).
	// Allows grouping multiple LLM calls into a single task run / step.
	// Harbor agents pass these via LiteLLM extra_headers.
	SessionID  string `json:"session_id,omitempty"`  // X-Session-ID (e.g. "{trial_name}__agent")
	TrialName  string `json:"trial_name,omitempty"`  // X-Trial-Name (e.g. "trial-abc123")
	StepName   string `json:"step_name,omitempty"`   // X-Step-Name (e.g. "fix-bug")
	TaskName   string `json:"task_name,omitempty"`   // X-Task-Name (e.g. "data-anonymization")
}

// RequestBody is a convenience type for reading and restoring request bodies.
// Plugins that need to inspect the body should use this to avoid consuming it.
type RequestBody struct {
	Data []byte
}

// ReadAndRestore reads up to limit bytes from r.Body and restores it
// so downstream handlers can still read the full body.
func ReadAndRestore(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limited := io.LimitReader(r.Body, limit)
	data, err := readAll(limited)
	r.Body.Close()
	if err != nil {
		return data, err
	}
	// Restore for downstream
	r.Body = bodyReader(data)
	return data, nil
}

// readAll is a simple io.ReadAll replacement to avoid extra imports.
func readAll(r io.Reader) ([]byte, error) {
	buf := make([]byte, 0, 512)
	for {
		tmp := make([]byte, 4096)
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				return buf, nil
			}
			return buf, err
		}
	}
}

// bodyReader wraps a byte slice as an io.ReadCloser.
type bodyReaderImpl struct {
	data []byte
	pos  int
}

func bodyReader(data []byte) *bodyReaderImpl {
	return &bodyReaderImpl{data: data}
}

func (b *bodyReaderImpl) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}

func (b *bodyReaderImpl) Close() error { return nil }
