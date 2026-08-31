package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
// Plugin-specific config goes into the Plugins map, keyed by plugin name.
type Config struct {
	Server         ServerConfig          `yaml:"server"`
	RateLimit      RateLimitConfig       `yaml:"rate_limit"`
	Retry          RetryConfig           `yaml:"retry"`
	CircuitBreaker CircuitBreakerConfig  `yaml:"circuit_breaker"`
	Audit          AuditConfig           `yaml:"audit"`
	Tracing        TracingConfig         `yaml:"tracing"`
	Pricing        PricingConfig         `yaml:"pricing"`
	Pools          map[string]PoolConfig `yaml:"pools"`
	Routes         []RouteConfig         `yaml:"routes"`
	ModelMap       map[string]string     `yaml:"model_map"`
	Plugins        map[string]any        `yaml:"plugins"`
}

// PricingConfig holds per-model token pricing for cost calculation.
// Prices are in USD per 1 million (1M) tokens.
type PricingConfig struct {
	Enabled bool                  `yaml:"enabled"`
	Models  map[string]ModelPrice `yaml:"models"`
	Default *ModelPrice           `yaml:"default"`
}

// ModelPrice is the per-token price in USD per 1M tokens.
type ModelPrice struct {
	Prompt     float64 `yaml:"prompt"`     // $/M prompt tokens
	Completion float64 `yaml:"completion"` // $/M completion tokens
}

type ServerConfig struct {
	Listen           string        `yaml:"listen"`
	Timeout          time.Duration `yaml:"timeout"`
	GracefulShutdown time.Duration `yaml:"graceful_shutdown"`
}

type RateLimitConfig struct {
	GlobalRPS float64 `yaml:"global_rps"`
	PerKeyRPS float64 `yaml:"per_key_rps"`
	KeyHeader string  `yaml:"key_header"`
}

type RetryConfig struct {
	MaxRetries        int   `yaml:"max_retries"`
	RetryableStatuses []int `yaml:"retryable_statuses"`
}

type CircuitBreakerConfig struct {
	Threshold    int           `yaml:"threshold"`
	Window       time.Duration `yaml:"window"`
	OpenDuration time.Duration `yaml:"open_duration"`
}

type AuditConfig struct {
	Enabled         bool   `yaml:"enabled"`
	Store           string `yaml:"store"` // log | sqlite | both
	SQLitePath      string `yaml:"sqlite_path"`
	LogTruncateBody int    `yaml:"log_truncate_body"`
	LogTokens       bool   `yaml:"log_tokens"`
}

type TracingConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Exporter    string  `yaml:"exporter"` // stdout | otlp | none
	Endpoint    string  `yaml:"endpoint"`
	Insecure    bool    `yaml:"insecure"`
	ServiceName string  `yaml:"service_name"`
	SampleRate  float64 `yaml:"sample_rate"`
}

type PoolConfig struct {
	Strategy    string            `yaml:"strategy"` // round_robin | least_conn | weighted
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Endpoints   []EndpointConfig  `yaml:"endpoints"`
	Fallback    string            `yaml:"fallback"`
}

type HealthCheckConfig struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

type EndpointConfig struct {
	Name          string        `yaml:"name"`
	URL           string        `yaml:"url"`
	HealthPath    string        `yaml:"health_path"`
	Weight        int           `yaml:"weight"`
	Timeout       time.Duration `yaml:"timeout"`
	TLSSkipVerify bool          `yaml:"tls_skip_verify"`
	AuthToken     string        `yaml:"auth_token"`
	AuthTokenEnv  string        `yaml:"auth_token_env"`
	// Models lists the model names served by this endpoint.
	// Used by the task-aware router to know which models are available.
	Models []string `yaml:"models"`
}

type RouteConfig struct {
	Pattern string `yaml:"pattern"`
	Pool    string `yaml:"pool"`
}

// Load reads and validates a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

// LoadFromBytes parses config from a byte slice (for testing).
func LoadFromBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func validate(cfg *Config) error {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":9090"
	}
	if cfg.Server.Timeout == 0 {
		cfg.Server.Timeout = 120 * time.Second
	}
	if cfg.Server.GracefulShutdown == 0 {
		cfg.Server.GracefulShutdown = 15 * time.Second
	}
	if cfg.RateLimit.GlobalRPS == 0 {
		cfg.RateLimit.GlobalRPS = 100
	}
	if cfg.Retry.MaxRetries == 0 {
		cfg.Retry.MaxRetries = 3
	}
	if cfg.CircuitBreaker.Threshold == 0 {
		cfg.CircuitBreaker.Threshold = 5
	}
	if cfg.CircuitBreaker.Window == 0 {
		cfg.CircuitBreaker.Window = 30 * time.Second
	}
	if cfg.CircuitBreaker.OpenDuration == 0 {
		cfg.CircuitBreaker.OpenDuration = 60 * time.Second
	}
	if cfg.Audit.Enabled && cfg.Audit.Store == "" {
		cfg.Audit.Store = "log"
	}
	if cfg.Audit.Enabled && cfg.Audit.SQLitePath == "" {
		cfg.Audit.SQLitePath = "data/audit.db"
	}
	if cfg.Tracing.Enabled && cfg.Tracing.Exporter == "" {
		cfg.Tracing.Exporter = "stdout"
	}
	if cfg.Tracing.Enabled && cfg.Tracing.SampleRate == 0 {
		cfg.Tracing.SampleRate = 1.0
	}
	if len(cfg.Routes) == 0 {
		return fmt.Errorf("no routes defined")
	}
	for _, r := range cfg.Routes {
		if _, ok := cfg.Pools[r.Pool]; !ok {
			return fmt.Errorf("route %q references unknown pool %q", r.Pattern, r.Pool)
		}
	}
	for name, pool := range cfg.Pools {
		p := pool
		if len(p.Endpoints) == 0 {
			return fmt.Errorf("pool %q has no endpoints", name)
		}
		if p.Fallback != "" {
			if _, ok := cfg.Pools[p.Fallback]; !ok {
				return fmt.Errorf("pool %q fallback references unknown pool %q", name, p.Fallback)
			}
		}
		for i, ep := range p.Endpoints {
			if ep.URL == "" {
				return fmt.Errorf("pool %q endpoint[%d] has no url", name, i)
			}
			if ep.Weight == 0 {
				p.Endpoints[i].Weight = 10
			}
			if ep.Timeout == 0 {
				p.Endpoints[i].Timeout = cfg.Server.Timeout
			}
			if ep.AuthTokenEnv != "" {
				val := os.Getenv(ep.AuthTokenEnv)
				if val == "" {
					return fmt.Errorf("pool %q endpoint[%d] auth_token_env %q is empty or unset", name, i, ep.AuthTokenEnv)
				}
				p.Endpoints[i].AuthToken = val
			}
		}
		if p.Strategy == "" {
			p.Strategy = "round_robin"
		}
		if p.HealthCheck.Interval == 0 {
			p.HealthCheck.Interval = 10 * time.Second
		}
		if p.HealthCheck.Timeout == 0 {
			p.HealthCheck.Timeout = 5 * time.Second
		}
		cfg.Pools[name] = p
	}
	return nil
}

// PluginConfig extracts a plugin's config block and unmarshals it into target.
// Returns false if no config block exists for the plugin.
func PluginConfig[T any](cfg *Config, pluginName string) (T, bool) {
	var zero T
	if cfg.Plugins == nil {
		return zero, false
	}
	raw, ok := cfg.Plugins[pluginName]
	if !ok {
		return zero, false
	}
	// Re-marshal through YAML to get typed config
	data, err := yaml.Marshal(raw)
	if err != nil {
		return zero, false
	}
	var result T
	if err := yaml.Unmarshal(data, &result); err != nil {
		return zero, false
	}
	return result, true
}
