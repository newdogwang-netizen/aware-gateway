package smartrouter

import (
	"fmt"
	"strings"

	"github.com/aware/gateway/internal/plugin"
)

const (
	budgetActionCheapProbe          = "cheap_probe"
	budgetActionCheapExecute        = "cheap_execute"
	budgetActionPremiumReason       = "premium_reason"
	budgetActionPremiumRecover      = "premium_recover"
	budgetActionCompletionGuardrail = "completion_guardrail"
)

// BudgetedRouteConfig lets a routing decision carry execution budget, not only
// a model choice. Profiles are keyed by route action names.
type BudgetedRouteConfig struct {
	Enabled  bool                          `yaml:"enabled" json:"enabled"`
	Profiles map[string]RouteBudgetProfile `yaml:"profiles" json:"profiles"`
}

type RouteBudgetProfile struct {
	MaxTokens int `yaml:"max_tokens" json:"max_tokens"`
	TimeoutMs int `yaml:"timeout_ms" json:"timeout_ms"`
}

func (s *SmartRouter) applyRouteBudget(decision *plugin.RoutingDecision, action string) {
	if decision == nil || decision.Skip {
		return
	}
	cfg := s.budgetedRouteConfig()
	if !cfg.Enabled || action == "" {
		return
	}
	profile, ok := cfg.Profiles[action]
	if !ok {
		return
	}

	decision.BudgetAction = action
	decision.MaxTokens = profile.MaxTokens
	decision.TimeoutMs = profile.TimeoutMs
	if decision.Reason == "" {
		decision.Reason = fmt.Sprintf("budget_action=%s", action)
		return
	}
	decision.Reason = fmt.Sprintf(
		"%s budget_action=%s route_max_tokens=%d route_timeout_ms=%d",
		decision.Reason,
		action,
		profile.MaxTokens,
		profile.TimeoutMs,
	)
}

func (s *SmartRouter) budgetedRouteConfig() BudgetedRouteConfig {
	cfg := s.cfg.BudgetedRoute
	if !cfg.Enabled {
		return cfg
	}

	defaults := defaultBudgetProfiles()
	if cfg.Profiles == nil {
		cfg.Profiles = defaults
		return cfg
	}
	for action, profile := range defaults {
		current, ok := cfg.Profiles[action]
		if !ok {
			cfg.Profiles[action] = profile
			continue
		}
		if current.MaxTokens <= 0 {
			current.MaxTokens = profile.MaxTokens
		}
		if current.TimeoutMs <= 0 {
			current.TimeoutMs = profile.TimeoutMs
		}
		cfg.Profiles[action] = current
	}
	return cfg
}

func defaultBudgetProfiles() map[string]RouteBudgetProfile {
	return map[string]RouteBudgetProfile{
		budgetActionCheapProbe: {
			MaxTokens: 2048,
			TimeoutMs: 60000,
		},
		budgetActionCheapExecute: {
			MaxTokens: 1536,
			TimeoutMs: 60000,
		},
		budgetActionPremiumReason: {
			MaxTokens: 4096,
			TimeoutMs: 180000,
		},
		budgetActionPremiumRecover: {
			MaxTokens: 4096,
			TimeoutMs: 180000,
		},
		budgetActionCompletionGuardrail: {
			MaxTokens: 1024,
			TimeoutMs: 60000,
		},
	}
}

func (s *SmartRouter) inferBudgetAction(selectedModel string, decision *DecisionResponse) string {
	if decision != nil {
		if action, ok := normalizeBudgetAction(decision.BudgetAction); ok {
			return action
		}
	}

	turnType := ""
	hypothesisState := ""
	recoverability := ""
	criticalPath := false
	if decision != nil {
		turnType = strings.ToLower(strings.TrimSpace(decision.TurnType))
		hypothesisState = strings.ToLower(strings.TrimSpace(decision.HypothesisState))
		recoverability = strings.ToLower(strings.TrimSpace(decision.Recoverability))
		criticalPath = decision.CriticalPath != nil && *decision.CriticalPath
	}

	if s.isStrongestConfiguredModel(selectedModel) || criticalPath {
		if turnType == "recovery" || hypothesisState == "contradicted" || recoverability == "hard" {
			return budgetActionPremiumRecover
		}
		return budgetActionPremiumReason
	}
	if turnType == "validation" || turnType == "implementation" || turnType == "finalization" {
		return budgetActionCheapExecute
	}
	return budgetActionCheapProbe
}

func isKnownBudgetAction(action string) bool {
	_, ok := normalizeBudgetAction(action)
	return ok
}

func normalizeBudgetAction(action string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(action))
	switch normalized {
	case budgetActionCheapProbe,
		budgetActionCheapExecute,
		budgetActionPremiumReason,
		budgetActionPremiumRecover,
		budgetActionCompletionGuardrail:
		return normalized, true
	default:
		return "", false
	}
}
