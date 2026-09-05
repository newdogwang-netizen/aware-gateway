// Package smartrouter implements an LLM-based model selection plugin.
//
// It calls a decision model to choose the best downstream model
// for each request. If the decision model fails, it routes to the configured
// fallback model when set; otherwise it returns Skip=true so the next router
// can handle the request.
//
// Configuration (under plugins.smart-router in gateway.yaml):
//
//	plugins:
//	  smart-router:
//	    enabled: true
//	    endpoint: "https://openrouter.ai/api/v1"
//	    model: "openai/gpt-5.6-sol"
//	    api_key_env: "GW_OPENROUTER_KEY"
//	    timeout_ms: 2000
//	    fallback_model: "anthropic/claude-opus-5"
//	    fallback_pool: "openrouter"
//	    cache_ttl_seconds: 300
package smartrouter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/plugin"
)

const (
	defaultDecisionHistoryTurns        = 5
	defaultDecisionHistoryContextChars = 220
)

// ModelEntry is a model in the decision menu sent to the LLM.
type ModelEntry struct {
	Name          string   `json:"name"`
	Pool          string   `json:"pool"`
	InputPrice    float64  `json:"input_price"`  // $/M tokens
	OutputPrice   float64  `json:"output_price"` // $/M tokens
	Capabilities  []string `json:"capabilities"`
	ContextWindow int      `json:"context_window"`
}

// Config is the plugin-specific configuration.
type Config struct {
	Enabled                     bool            `yaml:"enabled" json:"enabled"`
	Endpoint                    string          `yaml:"endpoint" json:"endpoint"`                         // OpenAI-compatible base URL
	Model                       string          `yaml:"model" json:"model"`                               // decision model name
	APIKey                      string          `yaml:"api_key" json:"api_key"`                           // optional
	APIKeyEnv                   string          `yaml:"api_key_env" json:"api_key_env"`                   // optional env var for API key
	MaxTokens                   int             `yaml:"max_tokens" json:"max_tokens"`                     // default 100
	Temperature                 float64         `yaml:"temperature" json:"temperature"`                   // default 0
	TimeoutMs                   int             `yaml:"timeout_ms" json:"timeout_ms"`                     // default 2000
	DisableThinking             bool            `yaml:"disable_thinking" json:"disable_thinking"`         // add Qwen/vLLM thinking-disable param
	DecisionRetries             int             `yaml:"decision_retries" json:"decision_retries"`         // retry transient decision model failures
	PromptPreviewChars          int             `yaml:"prompt_preview_chars" json:"prompt_preview_chars"` // default 500
	IncludeSystemPrompt         bool            `yaml:"include_system_prompt" json:"include_system_prompt"`
	IncludeMessageCount         bool            `yaml:"include_message_count" json:"include_message_count"`
	DecisionHistoryTurns        int             `yaml:"decision_history_turns" json:"decision_history_turns"`                 // default 5; <0 disables
	DecisionHistoryContextChars int             `yaml:"decision_history_context_chars" json:"decision_history_context_chars"` // default 220
	CacheTTLSeconds             int             `yaml:"cache_ttl_seconds" json:"cache_ttl_seconds"`                           // default 300
	CacheMaxEntries             int             `yaml:"cache_max_entries" json:"cache_max_entries"`                           // default 10000
	FallbackModel               string          `yaml:"fallback_model" json:"fallback_model"`
	FallbackPool                string          `yaml:"fallback_pool" json:"fallback_pool"`
	WarmStart                   WarmStartConfig `yaml:"warm_start" json:"warm_start"`
	// Decision model pricing ($/M tokens). For self-hosted vLLM, leave as 0
	// (cost is GPU amortization, not per-token). For commercial decision
	// models, set these to enable cost tracking in audit trail.
	DecisionInputPrice  float64      `yaml:"decision_input_price" json:"decision_input_price"`
	DecisionOutputPrice float64      `yaml:"decision_output_price" json:"decision_output_price"`
	Models              []ModelEntry `yaml:"models" json:"models"` // model menu
}

// WarmStartConfig forces the first N auto calls in a session to a configured
// model before letting the decision model route the remaining calls.
type WarmStartConfig struct {
	TriggerModels []string `yaml:"trigger_models" json:"trigger_models"`
	Steps         int      `yaml:"steps" json:"steps"`
	Model         string   `yaml:"model" json:"model"`
	Pool          string   `yaml:"pool" json:"pool"`
}

// SmartRouter implements plugin.RequestRouter.
type SmartRouter struct {
	cfg        Config
	menu       []ModelEntry
	menuJSON   string // pre-serialized menu for prompt
	client     *http.Client
	cache      *DecisionCache
	logger     *slog.Logger
	ctx        *plugin.Context
	auditSinks []plugin.AuditSink
	warmMu     sync.Mutex
	warmCounts map[string]int
	historyMu  sync.Mutex
	histories  map[string][]DecisionHistory
}

func (s *SmartRouter) Name() string { return "smart-router" }

// Priority returns 50 — runs before task-router (default 100).
func (s *SmartRouter) Priority() int { return 50 }

func (s *SmartRouter) Init(ctx *plugin.Context) error {
	s.ctx = ctx
	s.logger = ctx.Logger

	cfg, ok := config.PluginConfig[Config](ctx.Config, "smart-router")
	if !ok {
		s.logger.Info("smart-router: no config block, plugin disabled")
		s.cfg = Config{Enabled: false}
		return nil
	}
	s.cfg = cfg

	if !cfg.Enabled {
		s.logger.Info("smart-router: disabled in config")
		return nil
	}

	// Capture audit sinks for decision cost tracking.
	// Registry is available via the Registry type, but Context doesn't expose it.
	// We'll set audit sinks from main.go after registration.

	// Apply defaults
	if cfg.MaxTokens == 0 {
		s.cfg.MaxTokens = 100
	}
	if cfg.TimeoutMs == 0 {
		s.cfg.TimeoutMs = 2000
	}
	if cfg.PromptPreviewChars == 0 {
		s.cfg.PromptPreviewChars = 2000
	}
	if cfg.DecisionHistoryTurns == 0 {
		s.cfg.DecisionHistoryTurns = defaultDecisionHistoryTurns
	} else if cfg.DecisionHistoryTurns < 0 {
		s.cfg.DecisionHistoryTurns = 0
	}
	if cfg.DecisionHistoryContextChars == 0 {
		s.cfg.DecisionHistoryContextChars = defaultDecisionHistoryContextChars
	} else if cfg.DecisionHistoryContextChars < 80 {
		s.cfg.DecisionHistoryContextChars = 80
	}
	if s.cfg.APIKey == "" && s.cfg.APIKeyEnv != "" {
		s.cfg.APIKey = os.Getenv(s.cfg.APIKeyEnv)
	}
	if cfg.CacheTTLSeconds == 0 {
		s.cfg.CacheTTLSeconds = 300
	}
	if cfg.CacheMaxEntries == 0 {
		s.cfg.CacheMaxEntries = 10000
	}

	// Build model menu from config, or auto-discover from pools
	s.menu = cfg.Models
	if len(s.menu) == 0 {
		s.menu = s.discoverFromPools(ctx.Pools)
	}
	if len(s.menu) == 0 {
		s.logger.Warn("smart-router: no models in menu, plugin will always Skip")
		return nil
	}

	// Pre-build menu text for prompt
	s.menuJSON = s.buildMenuText()

	// HTTP client for decision model calls
	s.client = &http.Client{
		Timeout: time.Duration(s.cfg.TimeoutMs) * time.Millisecond,
	}

	// Decision cache
	s.cache = NewDecisionCache(s.cfg.CacheMaxEntries, time.Duration(s.cfg.CacheTTLSeconds)*time.Second)
	s.warmCounts = make(map[string]int)
	s.histories = make(map[string][]DecisionHistory)

	s.logger.Info("smart-router initialized",
		"endpoint", s.cfg.Endpoint,
		"decision_model", s.cfg.Model,
		"api_key_env", s.cfg.APIKeyEnv,
		"menu_size", len(s.menu),
		"cache_ttl", s.cfg.CacheTTLSeconds,
		"timeout_ms", s.cfg.TimeoutMs,
		"decision_retries", s.cfg.DecisionRetries,
		"fallback_model", s.cfg.FallbackModel,
		"warm_start_steps", s.cfg.WarmStart.Steps,
		"decision_history_turns", s.cfg.DecisionHistoryTurns,
	)
	return nil
}

func (s *SmartRouter) Close() error { return nil }

// SetAuditSinks allows main.go to inject audit sinks after registry init,
// so decision model calls can be recorded in the trace/billing pipeline.
func (s *SmartRouter) SetAuditSinks(sinks []plugin.AuditSink) {
	s.auditSinks = sinks
}

// Route implements plugin.RequestRouter.
func (s *SmartRouter) Route(req *http.Request, body []byte) (*plugin.RoutingDecision, error) {
	if !s.cfg.Enabled || len(s.menu) == 0 {
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// Parse the request
	parsed := parseRequest(body)
	if parsed == nil {
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// If client pinned a known model, respect it.
	if parsed.Model != "" {
		modelAliases := pinnedModelAliases(parsed.Model)
		for _, m := range s.menu {
			if containsModelAlias(modelAliases, m.Name) {
				return &plugin.RoutingDecision{Skip: true}, nil
			}
		}
		if containsModelAlias(modelAliases, s.cfg.FallbackModel) {
			return &plugin.RoutingDecision{Skip: true}, nil
		}
	}

	if isTaskCompletionConfirmation(parsed.LatestUserMsg) {
		if strongest, ok := s.strongestConfiguredModel(); ok {
			s.clearDecisionHistory(req)
			return &plugin.RoutingDecision{
				Pool:   strongest.Pool,
				Model:  strongest.Name,
				Reason: "smart-router guardrail: task completion confirmation requires exact agent-control output",
			}, nil
		}
	}

	if decision, ok := s.warmStartDecision(req, parsed); ok {
		s.appendDecisionHistory(req, decision.Model, &DecisionResponse{
			TurnType:        "warm_start",
			HypothesisState: "forming",
			Recoverability:  "medium",
			ContextSummary:  "front-loaded premium reasoning",
			Reason:          decision.Reason,
		})
		return decision, nil
	}

	history := s.recentDecisionHistory(req)
	historyText := s.renderDecisionHistory(history)

	// Check cache (skip if disabled via cache_ttl_seconds < 0)
	if s.cache != nil && s.cfg.CacheTTLSeconds >= 0 {
		cacheKey := s.cache.Key(s.menu, parsed.MessageCount, parsed.SystemMsg, parsed.LatestUserMsg, historyText)
		if cached, hit := s.cache.Get(cacheKey); hit {
			s.logger.Debug("smart-router: cache hit",
				"model", cached.Model,
				"reason", cached.Reason,
			)
			s.appendDecisionHistory(req, cached.Model, &DecisionResponse{
				TurnType:       "cached",
				Recoverability: "easy",
				ContextSummary: "cached route for repeated context",
				Reason:         cached.Reason,
			})
			return &plugin.RoutingDecision{
				Pool:   cached.Pool,
				Model:  cached.Model,
				Reason: "cached: " + cached.Reason,
			}, nil
		}
	}

	// Build prompt
	prompt := s.buildPrompt(parsed, historyText)

	// Call decision model (pass req for trial/step/task header extraction)
	decision, err := s.callDecisionModel(prompt, req)
	if err != nil {
		s.logger.Warn("smart-router: decision model failed",
			"error", err,
			"endpoint", s.cfg.Endpoint,
			"fallback_model", s.cfg.FallbackModel,
		)
		fallback := s.fallbackDecision("decision-model-error")
		if fallback != nil && !fallback.Skip {
			s.appendDecisionHistory(req, fallback.Model, &DecisionResponse{
				TurnType:       "fallback",
				Recoverability: "hard",
				ContextSummary: "decision model failed",
				Reason:         "decision-model-error",
			})
		}
		return fallback, nil
	}

	// Validate: model must exist in menu. Accept LiteLLM/OpenAI provider
	// aliases from decision models, but route using the canonical menu name.
	var pool string
	var selectedModel string
	decisionAliases := pinnedModelAliases(decision.Model)
	for _, m := range s.menu {
		if containsModelAlias(decisionAliases, m.Name) {
			pool = m.Pool
			selectedModel = m.Name
			break
		}
	}
	if pool == "" {
		s.logger.Debug("smart-router: decision model returned unknown model",
			"model", decision.Model,
			"fallback_model", s.cfg.FallbackModel,
		)
		fallback := s.fallbackDecision("unknown-model")
		if fallback != nil && !fallback.Skip {
			s.appendDecisionHistory(req, fallback.Model, &DecisionResponse{
				TurnType:       "fallback",
				Recoverability: "hard",
				ContextSummary: "decision model returned unknown model",
				Reason:         "unknown-model",
			})
		}
		return fallback, nil
	}

	routingReason := decision.RoutingReason()

	// Cache the decision (skip if disabled)
	if s.cache != nil && s.cfg.CacheTTLSeconds >= 0 {
		cacheKey := s.cache.Key(s.menu, parsed.MessageCount, parsed.SystemMsg, parsed.LatestUserMsg, historyText)
		s.cache.Set(cacheKey, CachedDecision{
			Model:  selectedModel,
			Pool:   pool,
			Reason: routingReason,
		})
		s.logger.Info("smart-router: routed",
			"model", selectedModel,
			"decision_model_output", decision.Model,
			"pool", pool,
			"reason", routingReason,
			"cached_key", cacheKey[:8],
		)
	} else {
		s.logger.Info("smart-router: routed",
			"model", selectedModel,
			"decision_model_output", decision.Model,
			"pool", pool,
			"reason", routingReason,
		)
	}
	s.appendDecisionHistory(req, selectedModel, decision)

	return &plugin.RoutingDecision{
		Pool:   pool,
		Model:  selectedModel,
		Reason: fmt.Sprintf("smart-router: %s", routingReason),
	}, nil
}

func (s *SmartRouter) warmStartDecision(req *http.Request, parsed *parsedRequest) (*plugin.RoutingDecision, bool) {
	cfg := s.cfg.WarmStart
	if cfg.Steps <= 0 || cfg.Model == "" || !s.isWarmStartModel(parsed.Model) {
		return nil, false
	}

	key := req.Header.Get("X-Session-ID")
	if key == "" {
		key = req.Header.Get("X-Trial-Name")
	}
	if key == "" {
		if s.logger != nil {
			s.logger.Warn("smart-router warm-start skipped: missing session/trial key")
		}
		return nil, false
	}

	s.warmMu.Lock()
	callIndex := s.warmCounts[key] + 1
	s.warmCounts[key] = callIndex
	s.warmMu.Unlock()

	if callIndex > cfg.Steps {
		return nil, false
	}

	pool := cfg.Pool
	if pool == "" {
		pool = s.poolForModel(cfg.Model)
	}
	if pool == "" {
		if s.logger != nil {
			s.logger.Warn("smart-router warm-start skipped: model has no pool",
				"model", cfg.Model,
				"session_id", key,
			)
		}
		return nil, false
	}

	return &plugin.RoutingDecision{
		Pool:   pool,
		Model:  cfg.Model,
		Reason: fmt.Sprintf("smart-router warm-start: first %d calls use %s (call %d/%d)", cfg.Steps, cfg.Model, callIndex, cfg.Steps),
	}, true
}

// DecisionHistory is the compact per-session memory fed back into later
// router prompts for the same benchmark trial/session.
type DecisionHistory struct {
	Model           string `json:"model"`
	TurnType        string `json:"turn_type,omitempty"`
	HypothesisState string `json:"hypothesis_state,omitempty"`
	CriticalPath    *bool  `json:"critical_path,omitempty"`
	Recoverability  string `json:"recoverability,omitempty"`
	ContextSummary  string `json:"context_summary,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

func decisionHistoryKey(req *http.Request) string {
	if req == nil {
		return ""
	}
	if key := req.Header.Get("X-Session-ID"); key != "" {
		return key
	}
	return req.Header.Get("X-Trial-Name")
}

func (s *SmartRouter) recentDecisionHistory(req *http.Request) []DecisionHistory {
	if s.cfg.DecisionHistoryTurns <= 0 {
		return nil
	}
	key := decisionHistoryKey(req)
	if key == "" {
		return nil
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	history := s.histories[key]
	if len(history) == 0 {
		return nil
	}
	limit := s.cfg.DecisionHistoryTurns
	if len(history) > limit {
		history = history[len(history)-limit:]
	}
	out := make([]DecisionHistory, len(history))
	copy(out, history)
	return out
}

func (s *SmartRouter) appendDecisionHistory(req *http.Request, selectedModel string, decision *DecisionResponse) {
	if s.cfg.DecisionHistoryTurns <= 0 || decision == nil {
		return
	}
	key := decisionHistoryKey(req)
	if key == "" {
		return
	}
	entry := DecisionHistory{
		Model:           selectedModel,
		TurnType:        compactDecisionText(decision.TurnType, 32),
		HypothesisState: compactDecisionText(decision.HypothesisState, 32),
		CriticalPath:    decision.CriticalPath,
		Recoverability:  compactDecisionText(decision.Recoverability, 32),
		ContextSummary:  compactDecisionText(decision.ContextSummary, s.cfg.DecisionHistoryContextChars),
		Reason:          compactDecisionText(decision.Reason, s.cfg.DecisionHistoryContextChars),
	}
	if entry.ContextSummary == "" {
		entry.ContextSummary = entry.Reason
	}

	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if s.histories == nil {
		s.histories = make(map[string][]DecisionHistory)
	}
	history := append(s.histories[key], entry)
	if len(history) > s.cfg.DecisionHistoryTurns {
		history = history[len(history)-s.cfg.DecisionHistoryTurns:]
	}
	s.histories[key] = history
}

func (s *SmartRouter) clearDecisionHistory(req *http.Request) {
	key := decisionHistoryKey(req)
	if key == "" {
		return
	}
	s.historyMu.Lock()
	delete(s.histories, key)
	s.historyMu.Unlock()

	s.warmMu.Lock()
	delete(s.warmCounts, key)
	s.warmMu.Unlock()
}

func (s *SmartRouter) renderDecisionHistory(history []DecisionHistory) string {
	if len(history) == 0 {
		return ""
	}
	lines := make([]string, 0, len(history))
	for i, entry := range history {
		critical := "unknown"
		if entry.CriticalPath != nil {
			critical = fmt.Sprintf("%t", *entry.CriticalPath)
		}
		lines = append(lines, fmt.Sprintf(
			"%d. model=%s turn=%s state=%s critical=%s recover=%s ctx=%q reason=%q",
			i+1,
			entry.Model,
			valueOrUnknown(entry.TurnType),
			valueOrUnknown(entry.HypothesisState),
			critical,
			valueOrUnknown(entry.Recoverability),
			compactDecisionText(entry.ContextSummary, s.cfg.DecisionHistoryContextChars),
			compactDecisionText(entry.Reason, 120),
		))
	}
	return strings.Join(lines, "\n")
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func compactDecisionText(value string, maxChars int) string {
	value = strings.Join(strings.Fields(value), " ")
	if maxChars <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value
	}
	if maxChars <= 3 {
		return string(runes[:maxChars])
	}
	return string(runes[:maxChars-3]) + "..."
}

func (s *SmartRouter) isWarmStartModel(model string) bool {
	aliases := pinnedModelAliases(model)
	for _, trigger := range s.cfg.WarmStart.TriggerModels {
		if containsModelAlias(aliases, trigger) {
			return true
		}
		triggerAliases := pinnedModelAliases(trigger)
		for _, alias := range triggerAliases {
			if alias == model {
				return true
			}
		}
	}
	return false
}

func (s *SmartRouter) poolForModel(model string) string {
	for _, m := range s.menu {
		if m.Name == model {
			return m.Pool
		}
	}
	if model == s.cfg.FallbackModel {
		return s.cfg.FallbackPool
	}
	return ""
}

func (s *SmartRouter) strongestConfiguredModel() (ModelEntry, bool) {
	if len(s.menu) == 0 {
		return ModelEntry{}, false
	}

	strongest := s.menu[0]
	strongestCost := strongest.InputPrice + strongest.OutputPrice
	for _, model := range s.menu[1:] {
		cost := model.InputPrice + model.OutputPrice
		if cost >= strongestCost {
			strongest = model
			strongestCost = cost
		}
	}
	return strongest, true
}

func (s *SmartRouter) fallbackDecision(reason string) *plugin.RoutingDecision {
	if s.cfg.FallbackModel == "" {
		return &plugin.RoutingDecision{Skip: true}
	}

	pool := s.cfg.FallbackPool
	if pool == "" {
		for _, m := range s.menu {
			if m.Name == s.cfg.FallbackModel {
				pool = m.Pool
				break
			}
		}
	}
	if pool == "" {
		if s.logger != nil {
			s.logger.Warn("smart-router: fallback model configured without fallback pool",
				"fallback_model", s.cfg.FallbackModel,
				"reason", reason,
			)
		}
		return &plugin.RoutingDecision{Skip: true}
	}

	return &plugin.RoutingDecision{
		Pool:   pool,
		Model:  s.cfg.FallbackModel,
		Reason: fmt.Sprintf("smart-router fallback=%s", reason),
	}
}

// discoverFromPools builds the model menu from pool endpoints.
func (s *SmartRouter) discoverFromPools(pp plugin.PoolProvider) []ModelEntry {
	var entries []ModelEntry
	for poolName, p := range pp.All() {
		for _, ep := range p.Endpoints {
			for _, modelName := range ep.AllModels() {
				// Skip the decision model itself
				if modelName == s.cfg.Model {
					continue
				}
				entries = append(entries, ModelEntry{
					Name:          modelName,
					Pool:          poolName,
					Capabilities:  []string{"chat", "code"},
					ContextWindow: 131072, // default
				})
			}
		}
	}
	return entries
}

// buildMenuText creates the numbered model list for the prompt.
func (s *SmartRouter) buildMenuText() string {
	// Sort by cost (cheapest first)
	sorted := make([]ModelEntry, len(s.menu))
	copy(sorted, s.menu)
	sortByCost(sorted)

	var lines []string
	for i, m := range sorted {
		total := m.InputPrice + m.OutputPrice
		caps := strings.Join(m.Capabilities, ",")
		ctx := formatCtx(m.ContextWindow)

		// Add tier label based on price
		tier := ""
		if total < 1.0 {
			tier = "ultra-cheap"
		} else if total < 3.0 {
			tier = "budget"
		} else if total < 10.0 {
			tier = "mid"
		} else {
			tier = "premium"
		}

		lines = append(lines, fmt.Sprintf("%d. %s [%s] $%.2f/$%.2f perM %s %s ctx",
			i+1, m.Name, tier, m.InputPrice, m.OutputPrice, caps, ctx))
	}
	return strings.Join(lines, "\n")
}

func formatCtx(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	if n >= 1000 {
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

func sortByCost(models []ModelEntry) {
	for i := 1; i < len(models); i++ {
		for j := i; j > 0; j-- {
			costJ := models[j].InputPrice + models[j].OutputPrice
			costPrev := models[j-1].InputPrice + models[j-1].OutputPrice
			if costJ < costPrev {
				models[j], models[j-1] = models[j-1], models[j]
			} else {
				break
			}
		}
	}
}

func pinnedModelAliases(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	aliases := []string{model}
	if strings.HasPrefix(model, "openai/") {
		aliases = append(aliases, strings.TrimPrefix(model, "openai/"))
	}
	return aliases
}

func containsModelAlias(aliases []string, model string) bool {
	for _, alias := range aliases {
		if alias == model {
			return true
		}
	}
	return false
}

// callDecisionModel sends the routing prompt to the decision LLM.
// The request is passed to extract trial/step/task correlation headers
// for the audit record, and to calculate decision cost from the
// decision model pricing config.
func (s *SmartRouter) callDecisionModel(prompt string, req *http.Request) (*DecisionResponse, error) {
	// Build request body. disable_thinking is only for Qwen/vLLM-compatible
	// endpoints; commercial endpoints may reject provider-specific fields.
	requestBody := map[string]any{
		"model":       s.cfg.Model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  s.cfg.MaxTokens,
		"temperature": s.cfg.Temperature,
	}
	if s.cfg.DisableThinking {
		requestBody["chat_template_kwargs"] = map[string]bool{
			"enable_thinking": false,
		}
	}
	body, _ := json.Marshal(requestBody)

	url := strings.TrimRight(s.cfg.Endpoint, "/") + "/chat/completions"
	attempts := s.cfg.DecisionRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	var parsedDecision *DecisionResponse
	for attempt := 1; attempt <= attempts; attempt++ {
		var result struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}

		httpReq, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if s.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
		}

		resp, err := s.client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("call decision model: %w", err)
		} else {
			func() {
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					lastErr = fmt.Errorf("decision model returned %d", resp.StatusCode)
					return
				}
				if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
					lastErr = fmt.Errorf("decode response: %w", err)
					return
				}

				if len(result.Choices) == 0 {
					lastErr = fmt.Errorf("no choices in response")
					return
				}

				content := result.Choices[0].Message.Content

				// Calculate decision model cost from pricing config. Self-hosted decision
				// endpoints can keep this at zero; commercial decision models should set
				// DecisionInputPrice/DecisionOutputPrice for accounting.
				decisionCost := 0.0
				if s.cfg.DecisionInputPrice > 0 || s.cfg.DecisionOutputPrice > 0 {
					const perM = 1_000_000.0
					decisionCost = float64(result.Usage.PromptTokens)*s.cfg.DecisionInputPrice/perM +
						float64(result.Usage.CompletionTokens)*s.cfg.DecisionOutputPrice/perM
				}

				// Record decision model cost in audit trail. Successful decision-model
				// responses are recorded even when the visible JSON is malformed, because
				// providers may still bill those tokens and retries must remain auditable.
				trialName := req.Header.Get("X-Trial-Name")
				taskName := req.Header.Get("X-Task-Name")
				sessionID := req.Header.Get("X-Session-ID")
				decisionStep := "router-decision"
				if sn := req.Header.Get("X-Step-Name"); sn != "" {
					decisionStep = "router-decision-" + sn
				}

				for _, sink := range s.auditSinks {
					sink.Record(&plugin.AuditRecord{
						Timestamp:     time.Now(),
						Method:        "POST",
						Path:          "/v1/chat/completions",
						Endpoint:      s.cfg.Endpoint,
						Status:        200,
						Model:         s.cfg.Model,
						RoutedModel:   s.cfg.Model,
						Pool:          "decision-model",
						PromptTokens:  result.Usage.PromptTokens,
						CompTokens:    result.Usage.CompletionTokens,
						TotalTokens:   result.Usage.TotalTokens,
						Cost:          decisionCost,
						StepName:      decisionStep,
						TaskName:      taskName,
						TrialName:     trialName,
						SessionID:     sessionID,
						RoutingReason: "smart-router decision call",
					})
				}

				decision, err := parseDecisionJSON(content)
				if err != nil {
					lastErr = fmt.Errorf("parse decision response: %w", err)
					return
				}
				parsedDecision = decision
				lastErr = nil
			}()
		}

		if lastErr == nil {
			return parsedDecision, nil
		}
		if attempt < attempts {
			s.logger.Warn("smart-router: decision model attempt failed; retrying",
				"attempt", attempt,
				"max_attempts", attempts,
				"error", lastErr,
			)
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("decision model failed without error")
}

// DecisionResponse is the parsed output from the decision model.
type DecisionResponse struct {
	Model           string `json:"model"`
	TurnType        string `json:"turn_type,omitempty"`
	HypothesisState string `json:"hypothesis_state,omitempty"`
	CriticalPath    *bool  `json:"critical_path,omitempty"`
	Recoverability  string `json:"recoverability,omitempty"`
	ContextSummary  string `json:"context_summary,omitempty"`
	Reason          string `json:"reason"`
}

func (d *DecisionResponse) RoutingReason() string {
	var parts []string
	if d.TurnType != "" {
		parts = append(parts, "turn="+d.TurnType)
	}
	if d.HypothesisState != "" {
		parts = append(parts, "state="+d.HypothesisState)
	}
	if d.CriticalPath != nil {
		parts = append(parts, fmt.Sprintf("critical=%t", *d.CriticalPath))
	}
	if d.Recoverability != "" {
		parts = append(parts, "recover="+d.Recoverability)
	}
	if d.ContextSummary != "" {
		parts = append(parts, fmt.Sprintf("ctx=%q", compactDecisionText(d.ContextSummary, 80)))
	}
	if len(parts) == 0 {
		return d.Reason
	}
	if d.Reason == "" {
		return strings.Join(parts, " ")
	}
	return strings.Join(parts, " ") + " | " + d.Reason
}

// parseDecisionJSON extracts the JSON decision from the LLM response.
// Handles cases where the model wraps JSON in markdown code blocks or
// outputs thinking content before the JSON (e.g. Qwen3.8 <think>...</think>).
func parseDecisionJSON(content string) (*DecisionResponse, error) {
	content = strings.TrimSpace(content)

	// Strip <think>...</think> block if present (Qwen3.8 thinking mode)
	if idx := strings.Index(content, "</think>"); idx != -1 {
		content = strings.TrimSpace(content[idx+len("</think>"):])
	}

	// Strip markdown code block if present
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		var inside []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				if inBlock {
					break
				}
				inBlock = true
				continue
			}
			if inBlock {
				inside = append(inside, line)
			}
		}
		if len(inside) > 0 {
			content = strings.Join(inside, "\n")
		}
	}

	// Find JSON object in the content
	start := strings.Index(content, "{")
	if start == -1 {
		return nil, fmt.Errorf("no JSON object found in response: %s", content[:min(100, len(content))])
	}

	end := strings.LastIndex(content, "}")
	if end == -1 || end <= start {
		// JSON was truncated (max_tokens cut off before closing brace).
		// Try to extract just the model field using regex-like string search.
		// Look for "model":"xxx" pattern.
		modelStart := strings.Index(content[start:], `"model"`)
		if modelStart == -1 {
			return nil, fmt.Errorf("no JSON closing brace and no model field: %s", content[:min(100, len(content))])
		}
		modelStart += start
		// Find the value after "model":"
		valStart := strings.Index(content[modelStart:], `":"`)
		if valStart == -1 {
			return nil, fmt.Errorf("no model value found: %s", content[:min(100, len(content))])
		}
		valStart += modelStart + 3
		valEnd := strings.Index(content[valStart:], `"`)
		if valEnd == -1 {
			return nil, fmt.Errorf("no model value end: %s", content[:min(100, len(content))])
		}
		modelID := content[valStart : valStart+valEnd]
		if modelID == "" {
			return nil, fmt.Errorf("empty model in truncated JSON")
		}
		return &DecisionResponse{
			Model:  modelID,
			Reason: "(truncated response — reason not fully generated)",
		}, nil
	}

	jsonStr := content[start : end+1]
	var dec DecisionResponse
	if err := json.Unmarshal([]byte(jsonStr), &dec); err != nil {
		return nil, fmt.Errorf("parse JSON: %w (raw: %s)", err, jsonStr[:min(100, len(jsonStr))])
	}

	if dec.Model == "" {
		return nil, fmt.Errorf("empty model in decision")
	}

	return &dec, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- Request parsing ---

type parsedRequest struct {
	Model           string
	MessageCount    int
	LatestUserMsg   string
	SystemMsg       string
	EstimatedTokens int
}

func parseRequest(body []byte) *parsedRequest {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	p := &parsedRequest{
		Model:        strings.TrimSpace(req.Model),
		MessageCount: len(req.Messages),
	}

	for _, msg := range req.Messages {
		if msg.Role == "system" && p.SystemMsg == "" {
			p.SystemMsg = msg.Content
		}
		if msg.Role == "user" {
			p.LatestUserMsg = msg.Content
		}
		// Rough token estimate: ~4 chars per token
		p.EstimatedTokens += len(msg.Content) / 4
	}

	return p
}

func isTaskCompletionConfirmation(message string) bool {
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "are you sure you want to mark the task as complete") {
		return false
	}
	return strings.Contains(lower, `"task_complete": true`) ||
		strings.Contains(lower, "<task_complete>true</task_complete>")
}

// --- Prompt building ---

func (s *SmartRouter) buildPrompt(p *parsedRequest, historyText string) string {
	var sb strings.Builder

	// System instruction: explain the routing task with clear criteria.
	sb.WriteString("You are routing one LLM call inside a terminal coding agent. ")
	sb.WriteString("Pick the lowest-cost model that is likely to succeed for this call. ")
	sb.WriteString("Optimize for final task quality per dollar, not for speed. ")
	sb.WriteString("There is no automatic retry for this exact call, but ordinary terminal-agent work is iterative; use the stronger model only when it is likely to materially improve final success or avoid expensive rework.\n\n")

	// Model menu with tier descriptions
	sb.WriteString("Models (cheapest first):\n")
	sb.WriteString(s.menuJSON)
	sb.WriteString("\n\n")
	sb.WriteString("Cost signal:\n")
	sb.WriteString("- Model prices in the menu are USD per 1M input/output tokens.\n")
	sb.WriteString("- Treat price differences as first-class evidence. A model that is 50-100x more expensive must have a clearly higher chance of improving final task success for this specific next turn.\n")
	sb.WriteString("- The cheapest model is still a capable coding and reasoning model. Do not treat cheap as low quality by default; treat it as the default when the next turn can be corrected by ordinary iteration.\n")
	sb.WriteString("- Use this benchmark prior unless the local context clearly contradicts it: the cheapest flash model has about 75% of the strongest model's general intelligence, reasoning, and coding ability, while the strongest model costs about 70-100x more per token.\n")
	sb.WriteString("- Because of that prior, the strongest model needs a large local advantage to be worth it. Upgrade when this turn's expected quality gap is likely much larger than 25%, or when a mistake would cause expensive unrecoverable rework.\n")
	sb.WriteString("- Pay for the strongest model when the expected next response is a high-stakes implementation, debugging, or finalization step where a mistake is likely to waste more money than the upgrade costs.\n\n")
	sb.WriteString("Critical-path policy:\n")
	sb.WriteString("- Spend the strongest model on path-setting reasoning, not on every hard-looking turn. Critical-path turns establish or revise the core hypothesis that later work depends on.\n")
	sb.WriteString("- Treat these as critical path: protocol/schema inference, VM/bytecode semantics, cryptographic/KDF design, root-cause diagnosis after failed tests, concurrency/data-corruption reasoning, first solver architecture, and recovery from a clearly wrong hypothesis.\n")
	sb.WriteString("- A probe can be critical if its result will choose the main direction of the solution. A probe is cheap if it only checks a bounded fact under an already stable hypothesis.\n")
	sb.WriteString("- After the core hypothesis is stable, prefer the cheapest model for mechanical extraction, repetitive brute-force sweeps, formatting outputs, file reads, narrow validation, and simple follow-up checks.\n")
	sb.WriteString("- Final task completion and exact agent-control confirmations remain strongest-model turns.\n\n")

	sb.WriteString("Oracle-gap policy:\n")
	sb.WriteString("- Do not treat all validation as cheap. Running a known test with a clear oracle is cheap; deciding whether the validation covers hidden requirements is critical-path reasoning.\n")
	sb.WriteString("- Upgrade when the next turn must design tests, judge coverage, interpret ambiguous failures, reason about adversarial/malformed/edge-case inputs, or decide that local evidence is sufficient for final success.\n")
	sb.WriteString("- Prefer the cheapest model when validation is a bounded execution of already-chosen checks, and failures would produce concrete, easy-to-act-on output.\n")
	sb.WriteString("- If local tests pass but the real grader may include hidden cases, ask whether the next response is merely running one more check or reasoning about the gap between local evidence and hidden success. Use the strongest model for the latter.\n\n")

	sb.WriteString("Decision process:\n")
	sb.WriteString("1. Identify the next response type: orientation, critical_hypothesis, implementation, mechanical_probe, validation, recovery, or finalization.\n")
	sb.WriteString("2. Identify hypothesis_state: none, forming, stable, contradicted, or solved.\n")
	sb.WriteString("3. Before choosing the strongest model, name the core hypothesis this turn will establish or revise. If there is no such hypothesis, prefer the cheapest model.\n")
	sb.WriteString("4. Read recent router memory for this same trial. Use it to detect repeated bottlenecks, stable hypotheses, prior premium spending, and whether the next turn should continue or change strategy.\n")
	sb.WriteString("5. If the same bottleneck has already consumed multiple strongest-model turns, do not buy more blind probing. Use the strongest model only to change the search strategy; use the cheapest model for bounded sweeps and mechanical validation.\n")
	sb.WriteString("6. Prefer the strongest model for early critical-path modeling, but prefer the cheapest model for late execution once the problem model is stable.\n\n")

	// Turn phase context: terminal coding agents often move from a first
	// probe directly into file-writing, so the second assistant turn is not
	// necessarily a harmless planning-only turn.
	phase := "unknown"
	switch {
	case p.MessageCount <= 2:
		phase = "understand — agent is reading and summarizing the task"
	case p.MessageCount <= 4:
		phase = "plan-or-first-implementation — agent may now create or edit files"
	case p.MessageCount <= 6:
		phase = "code — agent is writing implementation code"
	case p.MessageCount <= 8:
		phase = "review — agent is analyzing code output and debugging"
	default:
		phase = "deep-context — agent has accumulated state; judge the next action, not depth alone"
	}
	sb.WriteString(fmt.Sprintf("Turn phase: %s\n", phase))
	sb.WriteString(fmt.Sprintf("Conversation depth: %d messages, ~%d input tokens\n\n", p.MessageCount, p.EstimatedTokens))

	if historyText != "" {
		sb.WriteString("Recent router memory for this same trial, oldest to newest:\n")
		sb.WriteString(historyText)
		sb.WriteString("\n\n")
		sb.WriteString("Use recent memory as routing evidence, not as a rule to repeat the last model. ")
		sb.WriteString("If previous summaries show the hypothesis is stable or repeated premium calls did not change strategy, prefer the cheaper model unless this turn establishes a new critical hypothesis. ")
		sb.WriteString("If previous summaries show contradiction, ambiguous validation, or a failed core hypothesis, consider the strongest model for recovery.\n\n")
	}

	// Request preview — give enough context to judge complexity.
	if s.cfg.IncludeSystemPrompt && p.SystemMsg != "" {
		sys := p.SystemMsg
		if len(sys) > 200 {
			sys = sys[:200] + "..."
		}
		sb.WriteString(fmt.Sprintf("System: %s\n", sys))
	}

	if p.LatestUserMsg != "" {
		msg := p.LatestUserMsg
		if len(msg) > s.cfg.PromptPreviewChars {
			msg = msg[:s.cfg.PromptPreviewChars] + "..."
		}
		sb.WriteString(fmt.Sprintf("Latest user message:\n%s\n\n", msg))
	}

	// Decision criteria with concrete guidance.
	sb.WriteString("Guidelines:\n")
	sb.WriteString("- Use the cheapest model for short or reversible progress: comprehension, environment inspection, command-output summaries, narrow planning, short answers, drafting diagnostic commands/tests, and routine code edits whose mistakes are easy to detect and repair.\n")
	sb.WriteString("- Do not upgrade solely because the task domain mentions security, performance, data, or systems work. Upgrade when this specific turn changes safety-critical logic, diagnoses failed tests, handles subtle edge cases, or makes a hard-to-recover decision.\n")
	sb.WriteString("- Use the strongest model for high-leverage critical-path turns: nontrivial core code changes, debugging failing tests or unknown errors, multi-file/stateful changes, concurrency/performance/data-corruption risks, specialized domain reasoning, or fragile final fixes.\n")
	sb.WriteString("- Choose based on the next agent response that must be generated, not only the previous command. If the next likely response must turn dense probe output into core implementation commands, write substantial file contents, or preserve a strict JSON command protocol while doing complex synthesis, use the strongest model.\n")
	sb.WriteString("- If the first real implementation is small, local, and testable, the cheapest model is acceptable. Use the strongest model for first implementation only when the design space is uncertain or the edit is hard to recover from.\n")
	sb.WriteString("- Heredocs, generated tests, and apply_patch-style edits are normal terminal-agent actions. Upgrade only when the content itself is complex, large, fragile, or hard to validate.\n")
	sb.WriteString("- Do not send long synthesis/code-authoring turns to the cheapest model merely because the previous shell command was observational. Cheap turns can still write code, but should stay compact and easy to check.\n")
	sb.WriteString("- Judge both the underlying task and the current turn. A hard task can still have cheap observation/planning turns; a short follow-up like \"fix it\" can still need the strongest model if the state is complex.\n")
	sb.WriteString("- Conversation depth is evidence of accumulated state, not an automatic reason to upgrade. If the next step is another bounded probe, summary, or mechanical edit under a stable hypothesis, keep it cheap.\n")
	sb.WriteString("- Use the strongest model for finalization or submission turns after local checks pass, even if the latest message looks like a simple confirmation; a malformed task_complete or missed last check loses the whole trial.\n")
	sb.WriteString("- Treat exact agent-control protocols as high risk. If the message asks to confirm task completion or emit task_complete, use the strongest model.\n")
	sb.WriteString("- Do not upgrade merely because the terminal agent must reply in JSON or issue shell-command batches; that is the normal low-risk protocol unless the turn is final submission or otherwise hard to recover.\n")
	sb.WriteString("- Prefer the cheaper model when it can safely advance the task; choose the stronger model when its expected quality gain justifies its much higher cost.\n")
	sb.WriteString("- Write context_summary as a compact memory for future routing: summarize the current decision context, bottleneck, or hypothesis state in under 14 words.\n")
	sb.WriteString("- Return exactly one model id from the menu.\n\n")

	sb.WriteString("Return JSON only with keys: model, turn_type, hypothesis_state, critical_path, recoverability, context_summary, reason. ")
	sb.WriteString("critical_path must be a JSON boolean. recoverability must be easy, medium, or hard. ")
	sb.WriteString("Keep context_summary under 14 words and reason under 12 words. Do not include any text outside the JSON.")

	return sb.String()
}

// PoolMu is a placeholder to avoid unused import; pools are read-only after init.
var _ sync.Mutex
