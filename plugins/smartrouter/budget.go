package smartrouter

import (
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/aware/gateway/internal/plugin"
)

const (
	budgetActionCheapProbe          = "cheap_probe"
	budgetActionCheapExecute        = "cheap_execute"
	budgetActionPremiumReason       = "premium_reason"
	budgetActionPremiumRecover      = "premium_recover"
	budgetActionCompletionGuardrail = "completion_guardrail"
	budgetActionStopTrial           = "stop_trial"
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

func (s *SmartRouter) applyRouteBudget(req *http.Request, decision *plugin.RoutingDecision, action string) {
	if decision == nil || decision.Skip {
		return
	}
	if action != "" {
		decision.BudgetAction = action
	}
	cfg := s.budgetedRouteConfig()
	if !cfg.Enabled || action == "" {
		return
	}
	profile, ok := cfg.Profiles[action]
	if !ok {
		return
	}
	profile, adjustment := s.adjustBudgetForEpisode(req, action, profile)

	decision.MaxTokens = profile.MaxTokens
	decision.TimeoutMs = profile.TimeoutMs
	budgetReason := fmt.Sprintf(
		"budget_action=%s route_max_tokens=%d route_timeout_ms=%d",
		action,
		profile.MaxTokens,
		profile.TimeoutMs,
	)
	if decision.Reason == "" {
		decision.Reason = budgetReason
	} else {
		decision.Reason = decision.Reason + " " + budgetReason
	}
	if adjustment != "" {
		decision.Reason += " " + adjustment
	}
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

func (s *SmartRouter) adjustBudgetForEpisode(req *http.Request, action string, profile RouteBudgetProfile) (RouteBudgetProfile, string) {
	cfg := s.episodeConfig()
	if !cfg.Enabled {
		return profile, ""
	}
	snapshot := s.episodeSnapshot(req)
	if snapshot.ID == "" {
		return profile, ""
	}

	severity := valueOrDefault(snapshot.NoProgressSeverity, "none")
	if snapshot.ActiveNoProgress || severity == "stale" || severity == "blocked" {
		return profile, fmt.Sprintf(
			"episode_adjust=no_progress_freeze episode_calls=%d episode_no_progress=%s episode_events_since_progress=%d episode_length_streak=%d episode_recent_length=%d",
			snapshot.CallCount,
			severity,
			snapshot.EventsSinceProgress,
			snapshot.ConsecutiveLengthFinishes,
			snapshot.RecentLengthFinishes,
		)
	}

	streakPressure := snapshot.ConsecutiveLengthFinishes >= cfg.LengthStreakThreshold
	windowPressure := snapshot.RecentLengthFinishes >= cfg.LengthWindowThreshold
	if !streakPressure && !windowPressure {
		return profile, ""
	}

	pressure := snapshot.ConsecutiveLengthFinishes
	if snapshot.RecentLengthFinishes > pressure {
		pressure = snapshot.RecentLengthFinishes
	}
	maxTokenMultiplier := 1.0 + float64(pressure)
	if maxTokenMultiplier > cfg.MaxTokensMultiplier {
		maxTokenMultiplier = cfg.MaxTokensMultiplier
	}
	timeoutMultiplier := 1.0 + float64(pressure)
	if timeoutMultiplier > cfg.TimeoutMultiplier {
		timeoutMultiplier = cfg.TimeoutMultiplier
	}

	adjusted := profile
	if adjusted.MaxTokens > 0 && maxTokenMultiplier > 1 {
		adjusted.MaxTokens = ceilBudget(adjusted.MaxTokens, maxTokenMultiplier, cfg.MaxTokensCeiling)
	}
	if adjusted.TimeoutMs > 0 && timeoutMultiplier > 1 {
		adjusted.TimeoutMs = ceilBudget(adjusted.TimeoutMs, timeoutMultiplier, cfg.TimeoutMsCeiling)
	}
	if adjusted == profile {
		return profile, ""
	}

	return adjusted, fmt.Sprintf(
		"episode_adjust=length_boost episode_calls=%d episode_length_streak=%d episode_recent_length=%d",
		snapshot.CallCount,
		snapshot.ConsecutiveLengthFinishes,
		snapshot.RecentLengthFinishes,
	)
}

func ceilBudget(value int, multiplier float64, ceiling int) int {
	if value <= 0 || multiplier <= 1 {
		return value
	}
	out := int(math.Ceil(float64(value) * multiplier))
	if ceiling > 0 && out > ceiling {
		return ceiling
	}
	return out
}
