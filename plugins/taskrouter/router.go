// Package taskrouter implements a task-aware routing plugin for the aware-gateway.
//
// It analyzes incoming LLM requests, classifies them by task type (chat, code,
// reasoning, etc.), and selects the best model from available downstream pools
// based on a multi-factor score: capability fit × cost × latency × current load.
//
// Configuration (under plugins.task-router in gateway.yaml):
//
//	plugins:
//	  task-router:
//	    enabled: true
//	    models:
//	      - name: "gpt-4o"
//	        pool: "openai"
//	        capabilities: ["chat", "code", "reasoning", "vision"]
//	        context_window: 128000
//	        input_price: 2.50
//	        output_price: 10.00
//	        max_tokens: 16384
//	    rules:
//	      - task: "reasoning"
//	        strategy: "best_quality"
//	      - task: "chat"
//	        strategy: "cheapest"
//	    default_strategy: "balanced"
package taskrouter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/plugin"
)

// TaskType categorizes LLM requests.
type TaskType string

const (
	TaskChat      TaskType = "chat"
	TaskCode      TaskType = "code"
	TaskReasoning TaskType = "reasoning"
	TaskVision    TaskType = "vision"
	TaskEmbedding TaskType = "embedding"
	TaskASR       TaskType = "asr"
	TaskUnknown   TaskType = "unknown"
)

// Strategy defines how to select a model for a given task.
type Strategy string

const (
	StratBestQuality  Strategy = "best_quality"
	StratCheapest     Strategy = "cheapest"
	StratLowestLatency Strategy = "lowest_latency"
	StratBalanced     Strategy = "balanced"
	StratAny          Strategy = "any"
)

// ModelInfo describes a model's capabilities and cost.
type ModelInfo struct {
	Name          string     `yaml:"name" json:"name"`
	Pool          string     `yaml:"pool" json:"pool"`
	Capabilities  []string   `yaml:"capabilities" json:"capabilities"`
	ContextWindow int        `yaml:"context_window" json:"context_window"`
	InputPrice    float64    `yaml:"input_price" json:"input_price"`       // $/M tokens
	OutputPrice   float64    `yaml:"output_price" json:"output_price"`     // $/M tokens
	MaxTokens     int        `yaml:"max_tokens" json:"max_tokens"`
	Tags          []string   `yaml:"tags" json:"tags,omitempty"`

	// Runtime state (not config)
	mu          sync.Mutex
	avgLatencyMs int64     // rolling average latency
	reqCount     int64     // total requests routed
}

// RoutingRule maps a task type to a selection strategy.
type RoutingRule struct {
	Task     TaskType `yaml:"task" json:"task"`
	Strategy Strategy `yaml:"strategy" json:"strategy"`
}

// Config is the plugin-specific configuration.
type Config struct {
	Enabled          bool          `yaml:"enabled" json:"enabled"`
	Models           []ModelInfo   `yaml:"models" json:"models"`
	Rules            []RoutingRule `yaml:"rules" json:"rules"`
	DefaultStrategy  Strategy      `yaml:"default_strategy" json:"default_strategy"`
	PreferHealthy    bool          `yaml:"prefer_healthy" json:"prefer_healthy"`
}

// Router implements plugin.RequestRouter.
type Router struct {
	cfg      Config
	registry *ModelRegistry
	classifier *TaskClassifier
	scorer   *Scorer
	logger   *slog.Logger
	ctx      *plugin.Context
}

func (r *Router) Name() string { return "task-router" }

func (r *Router) Init(ctx *plugin.Context) error {
	r.ctx = ctx
	r.logger = ctx.Logger

	// Load config from plugin block
	cfg, ok := config.PluginConfig[Config](ctx.Config, "task-router")
	if !ok {
		r.logger.Info("task-router: no config block, plugin disabled")
		r.cfg = Config{Enabled: false}
		return nil
	}
	r.cfg = cfg

	if !cfg.Enabled {
		r.logger.Info("task-router: disabled in config")
		return nil
	}

	// Build model registry
	r.registry = NewModelRegistry()
	for i := range cfg.Models {
		m := &cfg.Models[i]
		// Validate pool exists
		if _, ok := ctx.Pools.Get(m.Pool); !ok {
			r.logger.Warn("task-router: model references unknown pool",
				"model", m.Name,
				"pool", m.Pool,
			)
			continue
		}
		r.registry.Register(m)
	}

	// Build classifier and scorer
	r.classifier = NewTaskClassifier()
	r.scorer = NewScorer(cfg.PreferHealthy)

	// Auto-discover models from pool endpoint configs (static models field).
	// Always runs — augments explicitly configured models with endpoint-declared ones.
	r.autoDiscoverModels(ctx.Pools)

	r.logger.Info("task-router initialized",
		"models", r.registry.Size(),
		"rules", len(cfg.Rules),
		"default_strategy", cfg.DefaultStrategy,
	)
	return nil
}

func (r *Router) Close() error { return nil }

// Route implements plugin.RequestRouter.
func (r *Router) Route(req *http.Request, body []byte) (*plugin.RoutingDecision, error) {
	if !r.cfg.Enabled {
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// Parse request to understand the task
	parsed := parseRequest(body, req.Header.Get("Content-Type"))
	if parsed == nil {
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// ASR and embedding routes — not LLM routing decisions
	if parsed.IsASR || parsed.IsEmbedding {
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// If the requested model is explicitly configured in our registry
	// with capabilities, respect the client's choice — don't override.
	// This allows callers to pin specific models when they want to.
	if parsed.Model != "" {
		if _, exists := r.registry.Get(parsed.Model); exists {
			return &plugin.RoutingDecision{Skip: true}, nil
		}
	}

	// Classify the task
	task := r.classifier.Classify(parsed)

	// Find the strategy for this task
	strategy := r.cfg.DefaultStrategy
	if strategy == "" {
		strategy = StratBalanced
	}
	for _, rule := range r.cfg.Rules {
		if rule.Task == task {
			strategy = rule.Strategy
			break
		}
	}

	// Get candidate models that can handle this task
	candidates := r.registry.Candidates(task, parsed.EstimatedTokens, r.ctx.Pools)
	if len(candidates) == 0 {
		r.logger.Warn("task-router: no candidates for task",
			"task", task,
			"original_model", parsed.Model,
		)
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// Score and select
	selected := r.scorer.Select(candidates, strategy, r.ctx.Pools)
	if selected == nil {
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	reason := fmt.Sprintf("task=%s strategy=%s model=%s cost=$%.4f/M",
		task, strategy, selected.Name,
		selected.InputPrice+selected.OutputPrice,
	)

	// Update runtime stats
	selected.recordRequest()

	return &plugin.RoutingDecision{
		Pool:   selected.Pool,
		Model:  selected.Name,
		Reason: reason,
	}, nil
}

// autoDiscoverModels populates the registry from pool endpoint configs.
// It reads both static config (endpoint.models) and dynamically discovered
// models (endpoint.discoveredModels, populated by the pool's /v1/models
// discovery goroutine). Always runs to augment explicitly configured models.
func (r *Router) autoDiscoverModels(pp plugin.PoolProvider) {
	for poolName, p := range pp.All() {
		for _, ep := range p.Endpoints {
			// Register all models this endpoint serves (static + discovered)
			for _, modelName := range ep.AllModels() {
				if _, exists := r.registry.Get(modelName); exists {
					// Model already registered (from explicit config or another endpoint)
					// Only add if the pool differs (same model on different pools = redundancy)
					continue
				}
				m := &ModelInfo{
					Name:         modelName,
					Pool:         poolName,
					Capabilities: []string{string(TaskChat)}, // default capability
				}
				r.registry.Register(m)
				r.logger.Info("task-router: auto-discovered model",
					"model", modelName,
					"pool", poolName,
					"endpoint", ep.Name,
				)
			}
		}
	}
}

// --- ModelRegistry ---

type ModelRegistry struct {
	mu     sync.RWMutex
	models map[string]*ModelInfo
}

func NewModelRegistry() *ModelRegistry {
	return &ModelRegistry{models: make(map[string]*ModelInfo)}
}

func (r *ModelRegistry) Register(m *ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[m.Name] = m
}

func (r *ModelRegistry) Get(name string) (*ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[name]
	return m, ok
}

func (r *ModelRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.models)
}

// Candidates returns models that can handle the given task and have
// sufficient context window, and whose pool has at least one healthy endpoint.
func (r *ModelRegistry) Candidates(task TaskType, estTokens int, pp plugin.PoolProvider) []*ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*ModelInfo
	for _, m := range r.models {
		// Check capability
		if !hasCapability(m.Capabilities, string(task)) && task != TaskUnknown {
			continue
		}
		// Check context window
		if m.ContextWindow > 0 && estTokens > m.ContextWindow {
			continue
		}
		// Check pool health
		p, ok := pp.Get(m.Pool)
		if !ok {
			continue
		}
		hasHealthy := false
		for _, ep := range p.Endpoints {
			if ep.Healthy() && !ep.BreakerOpen() {
				hasHealthy = true
				break
			}
		}
		if !hasHealthy {
			continue
		}
		out = append(out, m)
	}
	return out
}

func hasCapability(caps []string, cap string) bool {
	for _, c := range caps {
		if c == cap || c == "all" {
			return true
		}
	}
	return false
}

func (m *ModelInfo) recordRequest() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqCount++
}

func (m *ModelInfo) recordLatency(ms int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.avgLatencyMs == 0 {
		m.avgLatencyMs = ms
	} else {
		// Exponential moving average
		m.avgLatencyMs = int64(float64(m.avgLatencyMs)*0.9 + float64(ms)*0.1)
	}
}

func (m *ModelInfo) LatencyMs() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.avgLatencyMs
}

func (m *ModelInfo) RequestCount() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reqCount
}

// --- TaskClassifier ---

type TaskClassifier struct {
	codeKeywords    *regexp.Regexp
	reasoningKeywords *regexp.Regexp
	visionKeywords  *regexp.Regexp
}

func NewTaskClassifier() *TaskClassifier {
	return &TaskClassifier{
		codeKeywords:    regexp.MustCompile(`(?i)(code|function|class|method|bug|debug|compile|runtime|algorithm|api|sql|python|java|golang|typescript|javascript|rust|refactor|implement|stack trace|error trace|unit test)`),
		reasoningKeywords: regexp.MustCompile(`(?i)(analyze|reason|explain why|step by step|derive|prove|mathematic|logic|deduce|infer|conclude|because|therefore|chain of thought|let's think)`),
		visionKeywords:  regexp.MustCompile(`(?i)(image|picture|photo|screenshot|diagram|chart|figure|visual|ocr|read the|describe the image)`),
	}
}

// ParsedRequest holds extracted information from the incoming request.
type ParsedRequest struct {
	Model           string
	Messages        []Message
	IsASR           bool
	IsEmbedding     bool
	IsStream        bool
	EstimatedTokens int
}

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func (c *TaskClassifier) Classify(p *ParsedRequest) TaskType {
	if p.IsASR {
		return TaskASR
	}
	if p.IsEmbedding {
		return TaskEmbedding
	}

	// Check for vision (messages with image_url content)
	for _, msg := range p.Messages {
		if msg.Role == "user" || msg.Role == "system" {
			if hasImageContent(msg.Content) {
				return TaskVision
			}
		}
	}

	// Concatenate all message content for keyword analysis
	text := concatMessages(p.Messages)
	if text == "" {
		return TaskChat
	}

	// Check for code
	if c.codeKeywords.MatchString(text) {
		return TaskCode
	}

	// Check for reasoning
	if c.reasoningKeywords.MatchString(text) {
		return TaskReasoning
	}

	return TaskChat
}

func hasImageContent(content any) bool {
	// Check if content is a slice (OpenAI vision format)
	// content can be: [{"type": "image_url", "image_url": {...}}]
	if arr, ok := content.([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "image_url" {
					return true
				}
			}
		}
	}
	return false
}

func concatMessages(msgs []Message) string {
	var sb strings.Builder
	for _, msg := range msgs {
		switch v := msg.Content.(type) {
		case string:
			sb.WriteString(v)
			sb.WriteString(" ")
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["type"].(string); ok && t == "text" {
						if text, ok := m["text"].(string); ok {
							sb.WriteString(text)
							sb.WriteString(" ")
						}
					}
				}
			}
		}
	}
	return sb.String()
}

func parseRequest(body []byte, contentType string) *ParsedRequest {
	if len(body) == 0 {
		return nil
	}
	if !strings.HasPrefix(contentType, "application/json") {
		return nil
	}

	var req struct {
		Model    string    `json:"model"`
		Messages []Message `json:"messages"`
		Input    string    `json:"input"`   // embeddings
		Stream   bool      `json:"stream"`
		MaxTokens int      `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	p := &ParsedRequest{
		Model:    req.Model,
		IsStream:   req.Stream,
	}

	// Determine request type by path or fields
	if req.Input != "" && len(req.Messages) == 0 {
		p.IsEmbedding = true
		p.EstimatedTokens = len(req.Input) / 4
		return p
	}

	p.Messages = req.Messages
	p.EstimatedTokens = estimateTokens(req.Messages, req.MaxTokens)

	return p
}

func estimateTokens(msgs []Message, maxTokens int) int {
	total := 0
	for _, msg := range msgs {
		switch v := msg.Content.(type) {
		case string:
			total += len(v) / 4 // rough: 4 chars per token
		case []any:
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["type"].(string); ok && t == "text" {
						if text, ok := m["text"].(string); ok {
							total += len(text) / 4
						}
					}
				}
			}
		}
	}
	if maxTokens > 0 {
		total += maxTokens
	}
	// Add overhead per message
	total += len(msgs) * 4
	return total
}

// --- Scorer ---

type Scorer struct {
	preferHealthy bool
}

func NewScorer(preferHealthy bool) *Scorer {
	return &Scorer{preferHealthy: preferHealthy}
}

// Select picks the best model based on the strategy.
func (s *Scorer) Select(candidates []*ModelInfo, strategy Strategy, pp plugin.PoolProvider) *ModelInfo {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	switch strategy {
	case StratBestQuality:
		return s.selectBestQuality(candidates)
	case StratCheapest:
		return s.selectCheapest(candidates)
	case StratLowestLatency:
		return s.selectLowestLatency(candidates)
	case StratAny:
		return candidates[0]
	default: // balanced
		return s.selectBalanced(candidates, pp)
	}
}

func (s *Scorer) selectBestQuality(candidates []*ModelInfo) *ModelInfo {
	// Best quality = largest context window + most capabilities
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.ContextWindow != b.ContextWindow {
			return a.ContextWindow > b.ContextWindow
		}
		return len(a.Capabilities) > len(b.Capabilities)
	})
	return candidates[0]
}

func (s *Scorer) selectCheapest(candidates []*ModelInfo) *ModelInfo {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		costA := a.InputPrice + a.OutputPrice
		costB := b.InputPrice + b.OutputPrice
		return costA < costB
	})
	return candidates[0]
}

func (s *Scorer) selectLowestLatency(candidates []*ModelInfo) *ModelInfo {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		la, lb := a.LatencyMs(), b.LatencyMs()
		if la == 0 {
			la = math.MaxInt64
		}
		if lb == 0 {
			lb = math.MaxInt64
		}
		return la < lb
	})
	return candidates[0]
}

func (s *Scorer) selectBalanced(candidates []*ModelInfo, pp plugin.PoolProvider) *ModelInfo {
	// Balanced = normalize cost, latency, and load, pick lowest composite
	type scored struct {
		m     *ModelInfo
		score float64
	}
	var items []scored

	// Find ranges for normalization
	minCost, maxCost := math.MaxFloat64, 0.0
	minLatency, maxLatency := int64(math.MaxInt64), int64(0)
	minLoad, maxLoad := int64(math.MaxInt64), int64(0)

	for _, m := range candidates {
		cost := m.InputPrice + m.OutputPrice
		if cost < minCost { minCost = cost }
		if cost > maxCost { maxCost = cost }

		lat := m.LatencyMs()
		if lat == 0 { lat = 500 } // default assumption
		if lat < minLatency { minLatency = lat }
		if lat > maxLatency { maxLatency = lat }

		load := poolLoad(m.Pool, pp)
		if load < minLoad { minLoad = load }
		if load > maxLoad { maxLoad = load }
	}

	for _, m := range candidates {
		cost := m.InputPrice + m.OutputPrice
		lat := m.LatencyMs()
		if lat == 0 { lat = 500 }
		load := poolLoad(m.Pool, pp)

		// Normalize to 0-1 (lower is better)
		costScore := norm(cost, minCost, maxCost)
		latScore := norm(float64(lat), float64(minLatency), float64(maxLatency))
		loadScore := norm(float64(load), float64(minLoad), float64(maxLoad))

		// Weighted composite (lower = better)
		score := costScore*0.3 + latScore*0.3 + loadScore*0.4
		items = append(items, scored{m: m, score: score})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].score < items[j].score
	})
	return items[0].m
}

func poolLoad(poolName string, pp plugin.PoolProvider) int64 {
	p, ok := pp.Get(poolName)
	if !ok {
		return math.MaxInt64
	}
	var total int64
	for _, ep := range p.Endpoints {
		if ep.Healthy() {
			total += ep.InFlight()
		}
	}
	return total
}

func norm(val, min, max float64) float64 {
	if max == min {
		return 0.5
	}
	return (val - min) / (max - min)
}
