// Package smartrouter implements an LLM-based model selection plugin.
//
// It calls a small decision model to choose the best downstream model
// for each request. If the decision model fails, it returns Skip=true
// and the rule-based task-router handles the request as fallback.
//
// Configuration (under plugins.smart-router in gateway.yaml):
//
//	plugins:
//	  smart-router:
//	    enabled: true
//	    endpoint: "http://localhost:18000/v1"
//	    model: "qwen3.8-27b"
//	    timeout_ms: 2000
//	    cache_ttl_seconds: 300
package smartrouter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/plugin"
)

// ModelEntry is a model in the decision menu sent to the LLM.
type ModelEntry struct {
	Name          string   `json:"name"`
	Pool          string   `json:"pool"`
	InputPrice    float64  `json:"input_price"`    // $/M tokens
	OutputPrice   float64  `json:"output_price"`   // $/M tokens
	Capabilities  []string `json:"capabilities"`
	ContextWindow int      `json:"context_window"`
}

// Config is the plugin-specific configuration.
type Config struct {
	Enabled              bool         `yaml:"enabled" json:"enabled"`
	Endpoint             string       `yaml:"endpoint" json:"endpoint"`                  // OpenAI-compatible base URL
	Model                string       `yaml:"model" json:"model"`                         // decision model name
	APIKey               string       `yaml:"api_key" json:"api_key"`                     // optional
	MaxTokens            int          `yaml:"max_tokens" json:"max_tokens"`               // default 100
	Temperature          float64      `yaml:"temperature" json:"temperature"`             // default 0
	TimeoutMs            int          `yaml:"timeout_ms" json:"timeout_ms"`               // default 2000
	PromptPreviewChars   int          `yaml:"prompt_preview_chars" json:"prompt_preview_chars"` // default 500
	IncludeSystemPrompt  bool         `yaml:"include_system_prompt" json:"include_system_prompt"`
	IncludeMessageCount  bool         `yaml:"include_message_count" json:"include_message_count"`
	CacheTTLSeconds      int          `yaml:"cache_ttl_seconds" json:"cache_ttl_seconds"`   // default 300
	CacheMaxEntries      int          `yaml:"cache_max_entries" json:"cache_max_entries"`   // default 10000
	Models               []ModelEntry `yaml:"models" json:"models"`                         // model menu
}

// SmartRouter implements plugin.RequestRouter.
type SmartRouter struct {
	cfg      Config
	menu     []ModelEntry
	menuJSON string // pre-serialized menu for prompt
	client   *http.Client
	cache    *DecisionCache
	logger   *slog.Logger
}

func (s *SmartRouter) Name() string { return "smart-router" }

// Priority returns 50 — runs before task-router (default 100).
func (s *SmartRouter) Priority() int { return 50 }

func (s *SmartRouter) Init(ctx *plugin.Context) error {
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

	// Apply defaults
	if cfg.MaxTokens == 0 {
		s.cfg.MaxTokens = 100
	}
	if cfg.TimeoutMs == 0 {
		s.cfg.TimeoutMs = 2000
	}
	if cfg.PromptPreviewChars == 0 {
		s.cfg.PromptPreviewChars = 500
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

	s.logger.Info("smart-router initialized",
		"endpoint", s.cfg.Endpoint,
		"decision_model", s.cfg.Model,
		"menu_size", len(s.menu),
		"cache_ttl", s.cfg.CacheTTLSeconds,
		"timeout_ms", s.cfg.TimeoutMs,
	)
	return nil
}

func (s *SmartRouter) Close() error { return nil }

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

	// If client pinned a known model, respect it
	if parsed.Model != "" {
		for _, m := range s.menu {
			if m.Name == parsed.Model {
				return &plugin.RoutingDecision{Skip: true}, nil
			}
		}
	}

	// Check cache
	cacheKey := s.cache.Key(s.menu, parsed.MessageCount, parsed.LatestUserMsg)
	if cached, hit := s.cache.Get(cacheKey); hit {
		s.logger.Debug("smart-router: cache hit",
			"model", cached.Model,
			"reason", cached.Reason,
		)
		return &plugin.RoutingDecision{
			Pool:   cached.Pool,
			Model:  cached.Model,
			Reason: "cached: " + cached.Reason,
		}, nil
	}

	// Build prompt
	prompt := s.buildPrompt(parsed)

	// Call decision model
	decision, err := s.callDecisionModel(prompt)
	if err != nil {
		s.logger.Warn("smart-router: decision model failed, falling back to task-router",
			"error", err,
			"endpoint", s.cfg.Endpoint,
		)
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// Validate: model must exist in menu
	var pool string
	for _, m := range s.menu {
		if m.Name == decision.Model {
			pool = m.Pool
			break
		}
	}
	if pool == "" {
		s.logger.Debug("smart-router: decision model returned unknown model, skipping",
			"model", decision.Model,
		)
		return &plugin.RoutingDecision{Skip: true}, nil
	}

	// Cache the decision
	s.cache.Set(cacheKey, CachedDecision{
		Model:  decision.Model,
		Pool:   pool,
		Reason: decision.Reason,
	})

	s.logger.Info("smart-router: routed",
		"model", decision.Model,
		"pool", pool,
		"reason", decision.Reason,
		"cached_key", cacheKey[:8],
	)

	return &plugin.RoutingDecision{
		Pool:   pool,
		Model:  decision.Model,
		Reason: fmt.Sprintf("smart-router: %s", decision.Reason),
	}, nil
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

// callDecisionModel sends the routing prompt to the decision LLM.
func (s *SmartRouter) callDecisionModel(prompt string) (*DecisionResponse, error) {
	// Build request body. chat_template_kwargs.enable_thinking=false
	// disables Qwen3.8's thinking mode for fast JSON-only output (~500ms vs ~10s).
	body, _ := json.Marshal(map[string]any{
		"model":       s.cfg.Model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  s.cfg.MaxTokens,
		"temperature": s.cfg.Temperature,
		"chat_template_kwargs": map[string]bool{
			"enable_thinking": false,
		},
	})

	url := strings.TrimRight(s.cfg.Endpoint, "/") + "/chat/completions"
	req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call decision model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("decision model returned %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	content := result.Choices[0].Message.Content
	return parseDecisionJSON(content)
}

// DecisionResponse is the parsed output from the decision model.
type DecisionResponse struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
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
	end := strings.LastIndex(content, "}")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("no JSON object found in response: %s", content[:min(100, len(content))])
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
		Model:        req.Model,
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

// --- Prompt building ---

func (s *SmartRouter) buildPrompt(p *parsedRequest) string {
	var sb strings.Builder

	// System instruction: explain the routing task with clear criteria
	sb.WriteString("You are routing an AI agent's LLM call. Pick the cheapest model that can handle this turn well.\n\n")

	// Model menu with tier descriptions
	sb.WriteString("Models (cheapest first):\n")
	sb.WriteString(s.menuJSON)
	sb.WriteString("\n\n")

	// Turn phase context — tell the decision model what the agent is doing
	// This is derived from message count (2=understand, 4=plan, 6=code, 8=review, 10=fix)
	phase := "unknown"
	switch {
	case p.MessageCount <= 2:
		phase = "understand — agent is reading and summarizing the task"
	case p.MessageCount <= 4:
		phase = "plan — agent is creating an implementation plan"
	case p.MessageCount <= 6:
		phase = "code — agent is writing implementation code"
	case p.MessageCount <= 8:
		phase = "review — agent is analyzing code output and debugging"
	default:
		phase = "fix — agent is fixing issues and finalizing"
	}
	sb.WriteString(fmt.Sprintf("Turn phase: %s\n", phase))
	sb.WriteString(fmt.Sprintf("Conversation depth: %d messages, ~%d input tokens\n\n", p.MessageCount, p.EstimatedTokens))

	// Request preview — give enough context to judge complexity
	if p.LatestUserMsg != "" {
		msg := p.LatestUserMsg
		if len(msg) > s.cfg.PromptPreviewChars {
			msg = msg[:s.cfg.PromptPreviewChars] + "..."
		}
		sb.WriteString(fmt.Sprintf("Latest user message:\n%s\n\n", msg))
	}

	// Decision criteria with concrete guidance
	sb.WriteString("Guidelines:\n")
	sb.WriteString("- Simple formatting, summarization, or short answers → use the cheapest model\n")
	sb.WriteString("- Writing production code, debugging complex systems, or multi-step reasoning → use a stronger model\n")
	sb.WriteString("- Consider conversation depth: later turns with more context may need stronger models\n")
	sb.WriteString("- When unsure, prefer cheaper — the agent can retry with a stronger model if needed\n\n")

	sb.WriteString("JSON only: {\"model\":\"id\",\"reason\":\"why\"}")

	return sb.String()
}

// PoolMu is a placeholder to avoid unused import; pools are read-only after init.
var _ sync.Mutex
