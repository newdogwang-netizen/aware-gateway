package smartrouter

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aware/gateway/internal/plugin"
)

const (
	defaultRepeatedErrorThreshold        = 2
	defaultPremiumCooldownAfter          = 2
	defaultPremiumCooldownTurns          = 1
	defaultCheapProbeBurstLimit          = 3
	defaultStopCostUSD                   = 3.0
	defaultStopAgentCallThreshold        = defaultEpisodeNoProgressAgentCallThreshold
	defaultStopLengthPressureThreshold   = 3
	defaultLongExplorationThreshold      = 12
	defaultLongExplorationCallThreshold  = 12
	defaultPostReplanNoProgressCallLimit = 3
	defaultPendingRouteOutcomeGrace      = 30 * time.Second
)

// SafeControlConfig enables a local high-confidence routing layer before the
// semantic decision model. It is intentionally conservative: unmatched requests
// continue through the existing prompt-based smart-router.
type SafeControlConfig struct {
	Enabled                       bool    `yaml:"enabled" json:"enabled"`
	RepeatedErrorThreshold        int     `yaml:"repeated_error_threshold" json:"repeated_error_threshold"`
	PremiumCooldownAfter          int     `yaml:"premium_cooldown_after" json:"premium_cooldown_after"`
	PremiumCooldownTurns          int     `yaml:"premium_cooldown_turns" json:"premium_cooldown_turns"`
	CheapProbeBurstLimit          int     `yaml:"cheap_probe_burst_limit" json:"cheap_probe_burst_limit"`
	StopCostUSD                   float64 `yaml:"stop_cost_usd" json:"stop_cost_usd"`
	StopAgentCallThreshold        int     `yaml:"stop_agent_call_threshold" json:"stop_agent_call_threshold"`
	StopLengthPressureThreshold   int     `yaml:"stop_length_pressure_threshold" json:"stop_length_pressure_threshold"`
	LongExplorationThreshold      int     `yaml:"long_exploration_threshold" json:"long_exploration_threshold"`
	LongExplorationCallThreshold  int     `yaml:"long_exploration_call_threshold" json:"long_exploration_call_threshold"`
	PostReplanNoProgressCallLimit int     `yaml:"post_replan_no_progress_call_limit" json:"post_replan_no_progress_call_limit"`
}

type safeControlState struct {
	LastErrorFingerprint     string
	LastEscalatedFingerprint string
	SameFailureCount         int
	ConsecutivePremium       int
	CooldownRemaining        int
	ConsecutiveCheapProbes   int
}

type safeControlObservation struct {
	ErrorFingerprint string
	State            safeControlState
}

var (
	fingerprintLocationRE = regexp.MustCompile(`(?:/[^\s:]+|[A-Za-z0-9_.-]+):\d+(?::\d+)?`)
	fingerprintNumberRE   = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	fingerprintSpaceRE    = regexp.MustCompile(`\s+`)
)

func (s *SmartRouter) safeControlDecision(req *http.Request, parsed *parsedRequest) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	cfg := s.safeControlConfig()
	if !cfg.Enabled || parsed == nil {
		return nil, nil, false
	}

	obs := s.observeSafeControlState(req, parsed)
	message := normalizeForRules(parsed.LatestUserMsg)

	if decision, history, ok := s.episodeStopGateDecision(req, cfg, parsed); ok {
		return decision, history, true
	}

	if obs.ErrorFingerprint != "" &&
		obs.State.SameFailureCount >= cfg.RepeatedErrorThreshold &&
		obs.State.LastEscalatedFingerprint != obs.ErrorFingerprint {
		s.markSafeControlEscalation(req, obs.ErrorFingerprint)
		return s.safeControlRoute(
			req,
			"repeated_error_upgrade",
			"premium_recover",
			true,
			0.94,
			[]string{
				fmt.Sprintf("same_failure_count=%d", obs.State.SameFailureCount),
				"fingerprint=" + compactDecisionText(obs.ErrorFingerprint, 90),
			},
			"recovery",
			"contradicted",
			"same error repeated; change recovery strategy",
			"same error observed repeatedly",
		)
	}

	if looksLikeHypothesisContradiction(message) {
		return s.safeControlRoute(
			req,
			"hypothesis_contradiction_upgrade",
			"premium_recover",
			true,
			0.92,
			[]string{"latest_message=contradicted hypothesis"},
			"recovery",
			"contradicted",
			"core hypothesis appears contradicted",
			"hypothesis contradicted",
		)
	}

	if decision, history, ok := s.episodeDeliveryStateControlDecision(req); ok {
		return decision, history, true
	}

	if decision, history, ok := s.episodePriorityStateControlDecision(req); ok {
		return decision, history, true
	}

	if obs.State.CooldownRemaining > 0 && !looksLikePremiumRequired(message) {
		return s.safeControlCheapRoute(
			req,
			obs.State,
			cfg,
			"premium_cooldown",
			"cheap_probe",
			0.88,
			[]string{
				fmt.Sprintf("cooldown_remaining=%d", obs.State.CooldownRemaining),
				fmt.Sprintf("consecutive_premium=%d", obs.State.ConsecutivePremium),
			},
			"cooldown",
			"stable",
			"cooldown after consecutive premium calls",
			"premium cooldown; gather cheap evidence",
		)
	}

	if decision, history, ok := s.episodeStateControlDecision(req); ok {
		return decision, history, true
	}

	if looksLikeFileReadOrSearch(message) {
		return s.safeControlCheapRoute(
			req,
			obs.State,
			cfg,
			"file_read_search_cheap",
			"cheap_probe",
			0.95,
			[]string{"latest_message=file read or search"},
			"mechanical_probe",
			"stable",
			"bounded file/search observation",
			"file read/search is reversible",
		)
	}

	if looksLikeExistingTestExecution(message) {
		return s.safeControlCheapRoute(
			req,
			obs.State,
			cfg,
			"existing_test_execution_cheap",
			"cheap_execute",
			0.93,
			[]string{"latest_message=run known test command"},
			"validation",
			"stable",
			"known test execution with clear oracle",
			"existing test execution is bounded",
		)
	}

	if looksLikeFixedFormatOutput(message) {
		return s.safeControlCheapRoute(
			req,
			obs.State,
			cfg,
			"fixed_format_output_cheap",
			"cheap_execute",
			0.90,
			[]string{"latest_message=fixed output protocol"},
			"fixed_format",
			"stable",
			"fixed format output without finalization",
			"fixed format is not enough to upgrade",
		)
	}

	return nil, nil, false
}

func (s *SmartRouter) episodeStopGateDecision(req *http.Request, cfg SafeControlConfig, parsed *parsedRequest) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	episodeCfg := s.episodeConfig()
	if !episodeCfg.Enabled {
		return nil, nil, false
	}
	snapshot := s.episodeSnapshot(req)
	if snapshot.ID == "" {
		return nil, nil, false
	}

	if shouldStopProviderIncomplete(snapshot) {
		return s.localStopGateRoute(
			req,
			"episode_provider_incomplete_stop_gate",
			"gateway_provider_incomplete_stop_gate",
			"aware-gateway stop gate: provider returned incomplete LLM metadata",
			0.98,
			episodeProviderIncompleteEvidence(snapshot),
			"provider incomplete; classify separately before continuing",
			"stop trial after provider incomplete response",
		)
	}

	if shouldRecoverHypothesisApplyLength(snapshot) {
		return nil, nil, false
	}

	if shouldRecoverExecutionStall(snapshot) {
		return nil, nil, false
	}

	if shouldStopCostWithoutVerifier(snapshot, cfg) {
		return s.localStopGateRoute(
			req,
			"episode_cost_without_verifier_stop_gate",
			"gateway_cost_stop_gate",
			"aware-gateway stop gate: cost threshold exceeded before verifier proximity",
			0.96,
			episodeCostStopEvidence(snapshot, cfg),
			"cost threshold exceeded without verifier proximity",
			"stop trial before spending past cost gate",
		)
	}

	if pendingRouteOutcomeAwaitingProjection(snapshot, parsed) {
		return nil, nil, false
	}

	if shouldStopPostReplanNoProgress(snapshot, cfg) {
		return s.localStopGateRoute(
			req,
			"episode_replan_no_progress_stop_gate",
			"gateway_replan_no_progress_stop_gate",
			"aware-gateway stop gate: bounded replan produced no effective progress",
			0.96,
			episodePostReplanStopEvidence(snapshot, cfg),
			"bounded replan produced no effective progress",
			"stop trial after bounded replan produced no progress",
		)
	}

	if shouldStopBlockedPremiumNoProgress(snapshot) {
		return s.localStopGateRoute(
			req,
			"episode_blocked_stop_gate",
			"gateway_stop_gate",
			"aware-gateway stop gate: blocked episode after premium recovery without observable progress",
			0.97,
			episodeBlockedStopEvidence(snapshot),
			"blocked episode after premium recovery without observable progress",
			"stop trial before spending another upstream call",
		)
	}

	if shouldStopAgentCallNoProgress(snapshot, cfg) {
		return s.localStopGateRoute(
			req,
			"episode_agent_call_no_progress_stop_gate",
			"gateway_no_progress_stop_gate",
			"aware-gateway stop gate: agent call threshold exceeded without progress",
			0.95,
			episodeAgentCallStopEvidence(snapshot, cfg),
			"agent call threshold exceeded without progress",
			"stop trial after too many no-progress agent calls",
		)
	}

	if shouldStopLengthPressureWithoutProgress(snapshot, cfg) {
		return s.localStopGateRoute(
			req,
			"episode_length_pressure_stop_gate",
			"gateway_length_pressure_stop_gate",
			"aware-gateway stop gate: repeated length pressure without implementation, validation, delivery, or strong progress",
			0.94,
			episodeLengthPressureStopEvidence(snapshot, cfg),
			"repeated length pressure without effective progress",
			"stop trial after repeated length pressure without progress",
		)
	}

	return nil, nil, false
}

func (s *SmartRouter) localStopGateRoute(req *http.Request, ruleID, abortKind, abortMessage string, confidence float64, evidence []string, summary, shortReason string) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	reason := fmt.Sprintf(
		"smart-router safe-control: decision_source=rule rule_id=%s action=%s confidence=%.2f evidence=%s",
		ruleID,
		budgetActionStopTrial,
		confidence,
		strings.Join(evidence, "; "),
	)
	routing := &plugin.RoutingDecision{
		Pool:         "local",
		Reason:       reason,
		BudgetAction: budgetActionStopTrial,
		Abort:        true,
		AbortStatus:  http.StatusConflict,
		AbortKind:    abortKind,
		AbortMessage: abortMessage,
	}
	criticalPath := true
	history := &DecisionResponse{
		TurnType:        "stop_gate",
		HypothesisState: "blocked",
		CriticalPath:    &criticalPath,
		Recoverability:  "hard",
		BudgetAction:    budgetActionStopTrial,
		ContextSummary:  summary,
		Reason:          shortReason,
	}
	s.attachEpisodeMetadata(req, routing, "continue")
	return routing, history, true
}

func shouldStopBlockedPremiumNoProgress(snapshot EpisodeSnapshot) bool {
	if valueOrDefault(snapshot.NoProgressSeverity, "none") != "blocked" {
		return false
	}
	if snapshot.LastBudgetAction != budgetActionPremiumRecover {
		return false
	}
	if pendingRouteOutcomeInGrace(snapshot) {
		return false
	}
	label := routeOutcomeLabelForSnapshot(snapshot)
	return label == routeOutcomePending || label == routeOutcomeNoProgress
}

func shouldStopProviderIncomplete(snapshot EpisodeSnapshot) bool {
	event, ok := latestLLMEvent(snapshot)
	return ok && event.Outcome == "provider_incomplete"
}

func shouldStopCostWithoutVerifier(snapshot EpisodeSnapshot, cfg SafeControlConfig) bool {
	if cfg.StopCostUSD <= 0 || snapshot.TotalCost <= cfg.StopCostUSD {
		return false
	}
	return !episodeCloseToVerifier(snapshot)
}

func shouldStopAgentCallNoProgress(snapshot EpisodeSnapshot, cfg SafeControlConfig) bool {
	threshold := cfg.StopAgentCallThreshold
	if threshold <= 0 {
		return false
	}
	if snapshot.CallCount <= threshold {
		return false
	}
	if snapshot.CandidateProgressCount > 0 || snapshot.StrongProgressCount > 0 {
		return false
	}
	if pendingRouteOutcomeInGrace(snapshot) {
		return false
	}
	return !episodeCloseToVerifier(snapshot)
}

func shouldReplanLongExploration(snapshot EpisodeSnapshot, cfg SafeControlConfig) bool {
	if snapshot.ID == "" || episodeCloseToVerifier(snapshot) {
		return false
	}
	if snapshot.LastReplanEventID != "" {
		return false
	}
	explorationThreshold := cfg.LongExplorationThreshold
	callThreshold := cfg.LongExplorationCallThreshold
	return (explorationThreshold > 0 && snapshot.ExplorationSinceProgress >= explorationThreshold) ||
		(callThreshold > 0 && snapshot.LLMCallsSinceProgress >= callThreshold)
}

func shouldStopPostReplanNoProgress(snapshot EpisodeSnapshot, cfg SafeControlConfig) bool {
	limit := cfg.PostReplanNoProgressCallLimit
	if limit <= 0 || snapshot.LastReplanEventID == "" || episodeCloseToVerifier(snapshot) {
		return false
	}
	if shouldApplyOpenReplanHypothesis(snapshot) {
		return false
	}
	if pendingRouteOutcomeInGrace(snapshot) {
		return false
	}
	if snapshot.LLMCallsSinceReplan < limit {
		return false
	}
	if snapshot.LastReplanHypothesis != "" &&
		snapshot.ReplanHypothesisStatus == replanHypothesisOpen &&
		snapshot.LLMCallsSinceHypothesis == 0 &&
		snapshot.ExplorationSinceHypothesis == 0 {
		return false
	}
	return postReplanStopEvidenceExhausted(snapshot, cfg)
}

func postReplanStopEvidenceExhausted(snapshot EpisodeSnapshot, cfg SafeControlConfig) bool {
	switch routeOutcomeLabelForSnapshot(snapshot) {
	case routeOutcomeNone, routeOutcomePending, routeOutcomeNoProgress,
		routeOutcomeRunException, routeOutcomeTestFailed, routeOutcomeVerifierFailed:
		return true
	}
	threshold := cfg.LongExplorationThreshold
	if threshold <= 0 {
		threshold = defaultLongExplorationThreshold
	}
	return threshold > 0 && snapshot.ExplorationSinceReplan >= threshold
}

func shouldStopLengthPressureWithoutProgress(snapshot EpisodeSnapshot, cfg SafeControlConfig) bool {
	threshold := cfg.StopLengthPressureThreshold
	if threshold <= 0 || snapshot.LengthPressureSinceProgress < threshold {
		return false
	}
	if shouldApplyOpenReplanHypothesis(snapshot) {
		return false
	}
	if pendingRouteOutcomeInGrace(snapshot) {
		return false
	}
	if snapshot.ImplementationProgressCount > 0 ||
		snapshot.ValidationProgressCount > 0 ||
		snapshot.DeliveryProgressCount > 0 ||
		snapshot.StrongProgressCount > 0 {
		return false
	}
	return !episodeCloseToVerifier(snapshot)
}

func shouldApplyOpenReplanHypothesis(snapshot EpisodeSnapshot) bool {
	if snapshot.LastReplanEventID == "" || snapshot.LastReplanHypothesis == "" {
		return false
	}
	if snapshot.LastBudgetAction == budgetActionHypothesisApply {
		return false
	}
	switch valueOrDefault(snapshot.ReplanHypothesisStatus, replanHypothesisNone) {
	case replanHypothesisOpen, replanHypothesisStale:
		return snapshot.CandidateProgressCount == 0 && snapshot.StrongProgressCount == 0
	default:
		return false
	}
}

func shouldRecoverHypothesisApplyLength(snapshot EpisodeSnapshot) bool {
	if snapshot.ID == "" || snapshot.LastBudgetAction != budgetActionHypothesisApply {
		return false
	}
	if snapshot.LastFinishReason != "length" {
		return false
	}
	if snapshot.CandidateProgressCount > 0 || snapshot.StrongProgressCount > 0 {
		return false
	}
	label := routeOutcomeLabelForSnapshot(snapshot)
	return label == routeOutcomePending || label == routeOutcomeNoProgress || label == routeOutcomeNone
}

func shouldRecoverExecutionStall(snapshot EpisodeSnapshot) bool {
	if snapshot.ID == "" {
		return false
	}
	if episodeCloseToVerifier(snapshot) {
		return false
	}
	return snapshot.ExecutionStallsSinceProgress >= defaultExecutionStallRecoveryThreshold
}

func shouldRecoverAnalysisProgressApplication(snapshot EpisodeSnapshot) bool {
	if snapshot.ID == "" || episodeCloseToVerifier(snapshot) {
		return false
	}
	if snapshot.LastProgressKind != routeOutcomeAnalysisProgress {
		return false
	}
	return snapshot.AnalysisApplicationAttemptsSinceProgress >= defaultAnalysisProgressApplicationAttemptLimit &&
		snapshot.AnalysisApplicationRecoveriesSinceProgress == 0
}

func shouldApplyAnalysisProgress(snapshot EpisodeSnapshot) bool {
	if snapshot.ID == "" || episodeCloseToVerifier(snapshot) {
		return false
	}
	if shouldRecoverAnalysisProgressApplication(snapshot) {
		return false
	}
	if valueOrDefault(snapshot.NextCapabilityReason, "") != "analysis_progress_needs_application" {
		return false
	}
	if snapshot.LastProgressKind != routeOutcomeAnalysisProgress {
		return false
	}
	return snapshot.StrongProgressCount > 0
}

func pendingRouteOutcomeInGrace(snapshot EpisodeSnapshot) bool {
	if routeOutcomeLabelForSnapshot(snapshot) != routeOutcomePending {
		return false
	}
	if snapshot.LastRouteTraceID == "" || snapshot.LastRouteOutcomeEventCount > 0 {
		return false
	}
	if snapshot.LastBudgetAction == "" || snapshot.LastBudgetAction == budgetActionStopTrial {
		return false
	}
	event, ok := latestLLMEvent(snapshot)
	if !ok || event.Timestamp.IsZero() {
		return false
	}
	age := time.Since(event.Timestamp)
	return age >= 0 && age < defaultPendingRouteOutcomeGrace
}

func pendingRouteOutcomeAwaitingProjection(snapshot EpisodeSnapshot, parsed *parsedRequest) bool {
	if routeOutcomeLabelForSnapshot(snapshot) != routeOutcomePending {
		return false
	}
	if snapshot.LastRouteTraceID == "" || snapshot.LastRouteOutcomeEventCount > 0 {
		return false
	}
	if snapshot.LastBudgetAction == "" || snapshot.LastBudgetAction == budgetActionStopTrial {
		return false
	}
	if parsed != nil && parsed.HasToolObservation {
		return true
	}
	return pendingRouteOutcomeInGrace(snapshot)
}

func episodeLongExplorationEvidence(snapshot EpisodeSnapshot, cfg SafeControlConfig) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("exploration_since_progress=%d", snapshot.ExplorationSinceProgress),
		fmt.Sprintf("long_exploration_threshold=%d", cfg.LongExplorationThreshold),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("long_exploration_call_threshold=%d", cfg.LongExplorationCallThreshold),
		fmt.Sprintf("candidate_progress=%d", snapshot.CandidateProgressCount),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
		"no_progress=" + valueOrDefault(snapshot.NoProgressSeverity, "none"),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func episodePostReplanStopEvidence(snapshot EpisodeSnapshot, cfg SafeControlConfig) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		"last_replan=" + valueOrUnknown(snapshot.LastReplanEventID),
		fmt.Sprintf("replan_count=%d", snapshot.ReplanCount),
		fmt.Sprintf("llm_since_replan=%d", snapshot.LLMCallsSinceReplan),
		fmt.Sprintf("post_replan_no_progress_call_limit=%d", cfg.PostReplanNoProgressCallLimit),
		fmt.Sprintf("exploration_since_replan=%d", snapshot.ExplorationSinceReplan),
		"replan_hypothesis_status=" + valueOrDefault(snapshot.ReplanHypothesisStatus, replanHypothesisNone),
		"last_replan_hypothesis_event=" + valueOrUnknown(snapshot.LastReplanHypothesisEventID),
		"last_replan_hypothesis=" + valueOrUnknown(compactDecisionText(snapshot.LastReplanHypothesis, 90)),
		fmt.Sprintf("candidate_progress=%d", snapshot.CandidateProgressCount),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func latestLLMEvent(snapshot EpisodeSnapshot) (EpisodeEvent, bool) {
	for index := len(snapshot.RecentEvents) - 1; index >= 0; index-- {
		event := snapshot.RecentEvents[index]
		if event.Kind == "llm_call" {
			return event, true
		}
	}
	return EpisodeEvent{}, false
}

func episodeCloseToVerifier(snapshot EpisodeSnapshot) bool {
	switch valueOrDefault(snapshot.CompletionReadiness, completionReadinessNone) {
	case completionReadinessValidationPassed,
		completionReadinessVerifierFailed,
		completionReadinessVerifierPassed:
		return true
	default:
		return snapshot.VerifierReward > 0 || (snapshot.DeliveryFileWriteCount > 0 && snapshot.TestPassedCount > 0)
	}
}

func episodeBlockedStopEvidence(snapshot EpisodeSnapshot) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		"no_progress=" + valueOrDefault(snapshot.NoProgressSeverity, "none"),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("events_since_progress=%d", snapshot.EventsSinceProgress),
		fmt.Sprintf("length_since_progress=%d", snapshot.LengthPressureSinceProgress),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_route_trace=" + valueOrUnknown(snapshot.LastRouteTraceID),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		fmt.Sprintf("last_route_outcome_events=%d", snapshot.LastRouteOutcomeEventCount),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func episodeProviderIncompleteEvidence(snapshot EpisodeSnapshot) []string {
	event, _ := latestLLMEvent(snapshot)
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("call_count=%d", snapshot.CallCount),
		fmt.Sprintf("total_cost=$%.4f", snapshot.TotalCost),
		"last_event=" + valueOrUnknown(event.ID),
		"last_outcome=" + valueOrUnknown(event.Outcome),
		"last_model=" + valueOrUnknown(snapshot.LastModel),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_finish=" + valueOrUnknown(snapshot.LastFinishReason),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
	}
}

func episodeCostStopEvidence(snapshot EpisodeSnapshot, cfg SafeControlConfig) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("total_cost=$%.4f", snapshot.TotalCost),
		fmt.Sprintf("stop_cost_usd=$%.4f", cfg.StopCostUSD),
		fmt.Sprintf("call_count=%d", snapshot.CallCount),
		"completion_readiness=" + valueOrDefault(snapshot.CompletionReadiness, completionReadinessNone),
		fmt.Sprintf("delivery_file_writes=%d", snapshot.DeliveryFileWriteCount),
		fmt.Sprintf("test_passed=%d", snapshot.TestPassedCount),
		fmt.Sprintf("verifier_reward=%.3f", snapshot.VerifierReward),
		"no_progress=" + valueOrDefault(snapshot.NoProgressSeverity, "none"),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func episodeAgentCallStopEvidence(snapshot EpisodeSnapshot, cfg SafeControlConfig) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("call_count=%d", snapshot.CallCount),
		fmt.Sprintf("stop_agent_call_threshold=%d", cfg.StopAgentCallThreshold),
		"no_progress=" + valueOrDefault(snapshot.NoProgressSeverity, "none"),
		fmt.Sprintf("candidate_progress=%d", snapshot.CandidateProgressCount),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("events_since_progress=%d", snapshot.EventsSinceProgress),
		fmt.Sprintf("length_since_progress=%d", snapshot.LengthPressureSinceProgress),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func episodeLengthPressureStopEvidence(snapshot EpisodeSnapshot, cfg SafeControlConfig) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("length_since_progress=%d", snapshot.LengthPressureSinceProgress),
		fmt.Sprintf("stop_length_pressure_threshold=%d", cfg.StopLengthPressureThreshold),
		fmt.Sprintf("recent_length=%d", snapshot.RecentLengthFinishes),
		fmt.Sprintf("length_streak=%d", snapshot.ConsecutiveLengthFinishes),
		fmt.Sprintf("file_writes=%d", snapshot.FileWriteCount),
		fmt.Sprintf("test_runs=%d", snapshot.TestRunCount),
		fmt.Sprintf("implementation_progress=%d", snapshot.ImplementationProgressCount),
		fmt.Sprintf("validation_progress=%d", snapshot.ValidationProgressCount),
		fmt.Sprintf("delivery_progress=%d", snapshot.DeliveryProgressCount),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func (s *SmartRouter) episodeDeliveryStateControlDecision(req *http.Request) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	episodeCfg := s.episodeConfig()
	if !episodeCfg.Enabled {
		return nil, nil, false
	}
	snapshot := s.episodeSnapshot(req)
	if snapshot.ID == "" {
		return nil, nil, false
	}
	expected := valueOrDefault(snapshot.NextMinCapability, nextMinCapabilityUnknown)
	if expected != nextMinCapabilityCheapExecute &&
		expected != nextMinCapabilityPremiumAssess &&
		expected != nextMinCapabilityPremiumReason &&
		expected != nextMinCapabilityPremiumRecover {
		return nil, nil, false
	}
	reason := valueOrDefault(snapshot.NextCapabilityReason, "state_floor")
	if !isDeliveryCapabilityFloor(snapshot, expected, reason) {
		return nil, nil, false
	}

	action := budgetActionForCapabilityFloor(expected)
	if hint, ok := normalizeBudgetAction(snapshot.NextBudgetActionHint); ok {
		action = hint
	}
	if action == "" {
		return nil, nil, false
	}

	ruleID := episodeDeliveryFloorRuleID(expected, reason)
	turnType := "assessment"
	hypothesisState := "stable"
	summary := "delivery evidence requires premium assessment"
	shortReason := "episode delivery state requires premium assessment"
	confidence := 0.95
	premium := true
	if expected == nextMinCapabilityCheapExecute {
		turnType = "finalization"
		summary = "implementation candidate needs delivery or direct validation"
		shortReason = "episode delivery candidate should be delivered or validated now"
		confidence = 0.93
		premium = false
	} else if expected == nextMinCapabilityPremiumRecover {
		turnType = "recovery"
		hypothesisState = "contradicted"
		summary = "delivery evidence requires premium recovery"
		shortReason = "episode delivery state requires premium recovery"
		confidence = 0.96
	}

	decision, history, ok := s.safeControlRoute(
		req,
		ruleID,
		action,
		premium,
		confidence,
		episodeDeliveryFloorEvidence(snapshot, expected, reason),
		turnType,
		hypothesisState,
		summary,
		shortReason,
	)
	if ok && expected == nextMinCapabilityCheapExecute {
		decision.AgentInstruction = deliveryCandidateAgentInstruction
	}
	return decision, history, ok
}

func episodeDeliveryFloorRuleID(expected, reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "verifier_failed_current_delivery":
		return "episode_verifier_recovery_floor"
	case "validation_failed_current_delivery":
		return "episode_validation_recovery_floor"
	case "verifier_passed_current_delivery":
		return "episode_verifier_assessment_floor"
	case "validation_passed_assess_hidden_gap":
		return "episode_validation_assessment_floor"
	case "delivery_candidate_needs_delivery":
		return "episode_delivery_candidate_floor"
	case "delivery_candidate_floor_exhausted":
		return "episode_delivery_candidate_recovery_floor"
	case "last_route_verifier_failed", "last_route_outcome_negative", "last_route_validation_failed":
		return "episode_route_outcome_recovery_floor"
	case "last_route_verifier_passed", "last_route_validation_passed":
		return "episode_route_outcome_assessment_floor"
	default:
		if expected == nextMinCapabilityPremiumRecover {
			return "episode_delivery_recovery_floor"
		}
		return "episode_delivery_assessment_floor"
	}
}

func isDeliveryCapabilityFloor(snapshot EpisodeSnapshot, expected, reason string) bool {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	switch normalized {
	case "verifier_failed_current_delivery",
		"verifier_passed_current_delivery":
		return true
	case "validation_passed_assess_hidden_gap":
		return snapshot.DeliveryFileWriteCount > 0 || snapshot.LastDeliveryEventID != ""
	case "delivery_candidate_needs_delivery":
		return expected == nextMinCapabilityCheapExecute &&
			snapshot.CompletionReadiness == completionReadinessDeliveryCandidate &&
			snapshot.DeliveryFileWriteCount == 0 &&
			snapshot.ImplementationProgressCount >= defaultDeliveryCandidateProgressThreshold
	case "delivery_candidate_floor_exhausted":
		return expected == nextMinCapabilityPremiumRecover &&
			snapshot.CompletionReadiness == completionReadinessDeliveryCandidate &&
			snapshot.DeliveryFileWriteCount == 0 &&
			snapshot.ImplementationProgressCount >= defaultDeliveryCandidateProgressThreshold &&
			recentRuleCallCount(snapshot.RecentEvents, "episode_delivery_candidate_floor") >= defaultDeliveryCandidateFloorAttemptLimit
	default:
		return false
	}
}

func episodeDeliveryFloorEvidence(snapshot EpisodeSnapshot, expected, reason string) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		"capability_floor status=local",
		"expected=" + expected,
		"reason=" + valueOrUnknown(reason),
		"budget_hint=" + valueOrUnknown(snapshot.NextBudgetActionHint),
		"completion_readiness=" + valueOrDefault(snapshot.CompletionReadiness, completionReadinessNone),
		fmt.Sprintf("implementation_progress=%d", snapshot.ImplementationProgressCount),
		fmt.Sprintf("validation_progress=%d", snapshot.ValidationProgressCount),
		fmt.Sprintf("delivery_file_writes=%d", snapshot.DeliveryFileWriteCount),
		fmt.Sprintf("delivery_progress=%d", snapshot.DeliveryProgressCount),
		fmt.Sprintf("recent_delivery_floor_attempts=%d", recentRuleCallCount(snapshot.RecentEvents, "episode_delivery_candidate_floor")),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("exploration_since_progress=%d", snapshot.ExplorationSinceProgress),
		fmt.Sprintf("test_passed=%d", snapshot.TestPassedCount),
		fmt.Sprintf("test_failed=%d", snapshot.TestFailedCount),
		fmt.Sprintf("verifier_reward=%.3f", snapshot.VerifierReward),
		"last_delivery=" + valueOrUnknown(snapshot.LastDeliveryEventID),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
	}
}

func (s *SmartRouter) episodePriorityStateControlDecision(req *http.Request) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	episodeCfg := s.episodeConfig()
	if !episodeCfg.Enabled {
		return nil, nil, false
	}
	snapshot := s.episodeSnapshot(req)
	if snapshot.ID == "" {
		return nil, nil, false
	}

	if shouldRecoverHypothesisApplyLength(snapshot) {
		decision, history, ok := s.safeControlRoute(
			req,
			"episode_hypothesis_apply_length_recovery",
			budgetActionPremiumRecover,
			true,
			0.95,
			episodeHypothesisApplyLengthRecoveryEvidence(snapshot),
			"recovery",
			"open",
			"hypothesis application was truncated before a usable action",
			"recover truncated hypothesis application",
		)
		if ok {
			decision.AgentInstruction = hypothesisApplyLengthRecoveryAgentInstruction
		}
		return decision, history, ok
	}

	if shouldRecoverExecutionStall(snapshot) {
		decision, history, ok := s.safeControlRoute(
			req,
			"episode_execution_stall_recovery",
			budgetActionPremiumRecover,
			true,
			0.94,
			episodeExecutionStallRecoveryEvidence(snapshot),
			"recovery",
			"stalled",
			"execution channel is stuck before useful outcome projection",
			"recover stuck shell or truncated command execution",
		)
		if ok {
			decision.AgentInstruction = executionStallRecoveryAgentInstruction
		}
		return decision, history, ok
	}

	if shouldRecoverAnalysisProgressApplication(snapshot) {
		decision, history, ok := s.safeControlRoute(
			req,
			"episode_analysis_progress_application_recovery",
			budgetActionPremiumRecover,
			true,
			0.94,
			episodeAnalysisProgressApplicationRecoveryEvidence(snapshot),
			"recovery",
			"stalled",
			"cheap application attempts after analysis progress did not produce delivery",
			"recover stalled analysis application",
		)
		if ok {
			decision.AgentInstruction = analysisProgressApplicationRecoveryAgentInstruction
		}
		return decision, history, ok
	}

	if shouldApplyAnalysisProgress(snapshot) {
		decision, history, ok := s.safeControlRoute(
			req,
			"episode_analysis_progress_application",
			budgetActionCheapExecute,
			false,
			0.93,
			episodeAnalysisProgressApplicationEvidence(snapshot),
			"validation",
			"stable",
			"latest strong analysis progress needs direct application",
			"apply latest verified facts before further broad exploration",
		)
		if ok {
			decision.AgentInstruction = analysisProgressApplicationAgentInstruction
		}
		return decision, history, ok
	}

	return nil, nil, false
}

func (s *SmartRouter) episodeStateControlDecision(req *http.Request) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	episodeCfg := s.episodeConfig()
	if !episodeCfg.Enabled {
		return nil, nil, false
	}
	snapshot := s.episodeSnapshot(req)
	if snapshot.ID == "" {
		return nil, nil, false
	}

	controlCfg := s.safeControlConfig()
	if snapshot.LastFailureFingerprint != "" &&
		snapshot.SameFailureFingerprintCount >= controlCfg.RepeatedErrorThreshold {
		escalationFingerprint := "episode_test:" + snapshot.LastFailureFingerprint
		if !s.safeControlEscalated(req, escalationFingerprint) {
			s.markSafeControlEscalation(req, escalationFingerprint)
			return s.safeControlRoute(
				req,
				"episode_repeated_failure_recovery",
				budgetActionPremiumRecover,
				true,
				0.93,
				[]string{
					fmt.Sprintf("episode_id=%s", snapshot.ID),
					fmt.Sprintf("state_version=%d", snapshot.Version),
					fmt.Sprintf("same_failure_count=%d", snapshot.SameFailureFingerprintCount),
					fmt.Sprintf("failure_frontier_size=%d", snapshot.FailureFrontierSize),
					"failure_fingerprint=" + compactDecisionText(snapshot.LastFailureFingerprint, 90),
					fmt.Sprintf("test_failed_count=%d", snapshot.TestFailedCount),
					fmt.Sprintf("events_since_progress=%d", snapshot.EventsSinceProgress),
					"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
				},
				"recovery",
				"contradicted",
				"same test failure frontier repeated without progress",
				"repeated test failure frontier needs a new recovery strategy",
			)
		}
	}

	if decision, history, ok := s.episodePriorityStateControlDecision(req); ok {
		return decision, history, true
	}

	if shouldReplanLongExploration(snapshot, controlCfg) {
		return s.safeControlRoute(
			req,
			"episode_long_exploration_replan",
			budgetActionFreezeOrReplan,
			true,
			0.92,
			episodeLongExplorationEvidence(snapshot, controlCfg),
			"recovery",
			"forming",
			"long exploration requires a bounded replan before more execution",
			"long exploration; freeze budget and replan",
		)
	}

	if shouldApplyOpenReplanHypothesis(snapshot) {
		return s.safeControlRoute(
			req,
			"episode_replan_hypothesis_apply",
			budgetActionHypothesisApply,
			false,
			0.94,
			episodeHypothesisApplyEvidence(snapshot),
			"validation",
			"stable",
			"open replan hypothesis needs direct application",
			"validate or apply open replan hypothesis",
		)
	}

	severity := valueOrDefault(snapshot.NoProgressSeverity, "none")
	if severity != "stale" && severity != "blocked" {
		return nil, nil, false
	}

	evidence := []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("no_progress=%s", severity),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("events_since_progress=%d", snapshot.EventsSinceProgress),
		fmt.Sprintf("length_since_progress=%d", snapshot.LengthPressureSinceProgress),
		fmt.Sprintf("recent_length=%d", snapshot.RecentLengthFinishes),
		fmt.Sprintf("error_streak=%d", snapshot.ConsecutiveErrors),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
	ruleID := "episode_no_progress_recovery"
	summary := "episode state shows repeated pressure without progress"
	reason := "episode no-progress state requires recovery strategy"
	confidence := 0.91
	if severity == "blocked" {
		ruleID = "episode_blocked_recovery"
		summary = "episode state is blocked after many calls without progress"
		reason = "episode blocked; recover before spending more cheap turns"
		confidence = 0.96
	}

	return s.safeControlRoute(
		req,
		ruleID,
		budgetActionPremiumRecover,
		true,
		confidence,
		evidence,
		"recovery",
		"contradicted",
		summary,
		reason,
	)
}

func episodeHypothesisApplyEvidence(snapshot EpisodeSnapshot) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		"replan_hypothesis_status=" + valueOrDefault(snapshot.ReplanHypothesisStatus, replanHypothesisNone),
		"last_replan=" + valueOrUnknown(snapshot.LastReplanEventID),
		"last_replan_hypothesis_event=" + valueOrUnknown(snapshot.LastReplanHypothesisEventID),
		"last_replan_hypothesis=" + valueOrUnknown(compactDecisionText(snapshot.LastReplanHypothesis, 90)),
		fmt.Sprintf("llm_since_hypothesis=%d", snapshot.LLMCallsSinceHypothesis),
		fmt.Sprintf("exploration_since_hypothesis=%d", snapshot.ExplorationSinceHypothesis),
		fmt.Sprintf("llm_since_replan=%d", snapshot.LLMCallsSinceReplan),
		fmt.Sprintf("exploration_since_replan=%d", snapshot.ExplorationSinceReplan),
		fmt.Sprintf("length_since_progress=%d", snapshot.LengthPressureSinceProgress),
		fmt.Sprintf("candidate_progress=%d", snapshot.CandidateProgressCount),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func episodeHypothesisApplyLengthRecoveryEvidence(snapshot EpisodeSnapshot) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_finish=" + valueOrUnknown(snapshot.LastFinishReason),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"replan_hypothesis_status=" + valueOrDefault(snapshot.ReplanHypothesisStatus, replanHypothesisNone),
		"last_replan=" + valueOrUnknown(snapshot.LastReplanEventID),
		"last_replan_hypothesis_event=" + valueOrUnknown(snapshot.LastReplanHypothesisEventID),
		"last_replan_hypothesis=" + valueOrUnknown(compactDecisionText(snapshot.LastReplanHypothesis, 90)),
		fmt.Sprintf("llm_since_hypothesis=%d", snapshot.LLMCallsSinceHypothesis),
		fmt.Sprintf("exploration_since_hypothesis=%d", snapshot.ExplorationSinceHypothesis),
		fmt.Sprintf("llm_since_replan=%d", snapshot.LLMCallsSinceReplan),
		fmt.Sprintf("exploration_since_replan=%d", snapshot.ExplorationSinceReplan),
		fmt.Sprintf("length_since_progress=%d", snapshot.LengthPressureSinceProgress),
		fmt.Sprintf("candidate_progress=%d", snapshot.CandidateProgressCount),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
	}
}

func episodeExecutionStallRecoveryEvidence(snapshot EpisodeSnapshot) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("execution_stalls=%d", snapshot.ExecutionStallCount),
		fmt.Sprintf("execution_stalls_since_progress=%d", snapshot.ExecutionStallsSinceProgress),
		fmt.Sprintf("threshold=%d", defaultExecutionStallRecoveryThreshold),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("exploration_since_progress=%d", snapshot.ExplorationSinceProgress),
		"last_budget=" + valueOrUnknown(snapshot.LastBudgetAction),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
	}
}

func episodeAnalysisProgressApplicationEvidence(snapshot EpisodeSnapshot) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
		fmt.Sprintf("analysis_apply_attempts_since_progress=%d", snapshot.AnalysisApplicationAttemptsSinceProgress),
		fmt.Sprintf("analysis_apply_recoveries_since_progress=%d", snapshot.AnalysisApplicationRecoveriesSinceProgress),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
		"last_progress_event=" + valueOrUnknown(snapshot.LastProgressEventID),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"next_capability=" + valueOrDefault(snapshot.NextMinCapability, nextMinCapabilityUnknown),
		"reason=" + valueOrDefault(snapshot.NextCapabilityReason, "analysis_progress_needs_application"),
		"budget_hint=" + valueOrUnknown(snapshot.NextBudgetActionHint),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("exploration_since_progress=%d", snapshot.ExplorationSinceProgress),
		fmt.Sprintf("delivery_file_writes=%d", snapshot.DeliveryFileWriteCount),
		fmt.Sprintf("test_passed=%d", snapshot.TestPassedCount),
		fmt.Sprintf("verifier_reward=%.3f", snapshot.VerifierReward),
	}
}

func episodeAnalysisProgressApplicationRecoveryEvidence(snapshot EpisodeSnapshot) []string {
	return []string{
		fmt.Sprintf("episode_id=%s", snapshot.ID),
		fmt.Sprintf("state_version=%d", snapshot.Version),
		fmt.Sprintf("strong_progress=%d", snapshot.StrongProgressCount),
		fmt.Sprintf("analysis_apply_attempts_since_progress=%d", snapshot.AnalysisApplicationAttemptsSinceProgress),
		fmt.Sprintf("analysis_apply_recoveries_since_progress=%d", snapshot.AnalysisApplicationRecoveriesSinceProgress),
		fmt.Sprintf("analysis_apply_attempt_limit=%d", defaultAnalysisProgressApplicationAttemptLimit),
		"last_progress=" + valueOrUnknown(snapshot.LastProgressKind),
		"last_progress_event=" + valueOrUnknown(snapshot.LastProgressEventID),
		"last_route_outcome=" + routeOutcomeLabelForSnapshot(snapshot),
		"next_capability=" + valueOrDefault(snapshot.NextMinCapability, nextMinCapabilityUnknown),
		"reason=" + valueOrDefault(snapshot.NextCapabilityReason, "analysis_progress_application_exhausted"),
		"budget_hint=" + valueOrUnknown(snapshot.NextBudgetActionHint),
		fmt.Sprintf("llm_since_progress=%d", snapshot.LLMCallsSinceProgress),
		fmt.Sprintf("exploration_since_progress=%d", snapshot.ExplorationSinceProgress),
		fmt.Sprintf("delivery_file_writes=%d", snapshot.DeliveryFileWriteCount),
		fmt.Sprintf("test_passed=%d", snapshot.TestPassedCount),
		fmt.Sprintf("verifier_reward=%.3f", snapshot.VerifierReward),
	}
}

func (s *SmartRouter) safeControlEscalated(req *http.Request, fingerprint string) bool {
	key := decisionHistoryKey(req)
	if key == "" || fingerprint == "" {
		return false
	}

	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	state := s.controlStates[key]
	return state != nil && state.LastEscalatedFingerprint == fingerprint
}

func (s *SmartRouter) safeControlConfig() SafeControlConfig {
	cfg := s.cfg.SafeControl
	if cfg.RepeatedErrorThreshold <= 0 {
		cfg.RepeatedErrorThreshold = defaultRepeatedErrorThreshold
	}
	if cfg.PremiumCooldownAfter <= 0 {
		cfg.PremiumCooldownAfter = defaultPremiumCooldownAfter
	}
	if cfg.PremiumCooldownTurns <= 0 {
		cfg.PremiumCooldownTurns = defaultPremiumCooldownTurns
	}
	if cfg.CheapProbeBurstLimit <= 0 {
		cfg.CheapProbeBurstLimit = defaultCheapProbeBurstLimit
	}
	if cfg.StopCostUSD <= 0 {
		cfg.StopCostUSD = defaultStopCostUSD
	}
	if cfg.StopAgentCallThreshold <= 0 {
		cfg.StopAgentCallThreshold = defaultStopAgentCallThreshold
	}
	if cfg.StopLengthPressureThreshold <= 0 {
		cfg.StopLengthPressureThreshold = defaultStopLengthPressureThreshold
	}
	if cfg.LongExplorationThreshold <= 0 {
		cfg.LongExplorationThreshold = defaultLongExplorationThreshold
	}
	if cfg.LongExplorationCallThreshold <= 0 {
		cfg.LongExplorationCallThreshold = defaultLongExplorationCallThreshold
	}
	if cfg.PostReplanNoProgressCallLimit <= 0 {
		cfg.PostReplanNoProgressCallLimit = defaultPostReplanNoProgressCallLimit
	}
	return cfg
}

func (s *SmartRouter) safeControlCheapRoute(req *http.Request, state safeControlState, cfg SafeControlConfig, ruleID, action string, confidence float64, evidence []string, turnType, hypothesisState, summary, shortReason string) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	if state.ConsecutiveCheapProbes >= cfg.CheapProbeBurstLimit {
		return nil, nil, false
	}
	return s.safeControlRoute(
		req,
		ruleID,
		action,
		false,
		confidence,
		append(evidence, fmt.Sprintf("cheap_probe_streak=%d/%d", state.ConsecutiveCheapProbes, cfg.CheapProbeBurstLimit)),
		turnType,
		hypothesisState,
		summary,
		shortReason,
	)
}

func (s *SmartRouter) safeControlRoute(req *http.Request, ruleID, action string, premium bool, confidence float64, evidence []string, turnType, hypothesisState, summary, shortReason string) (*plugin.RoutingDecision, *DecisionResponse, bool) {
	model, ok := s.cheapestConfiguredModel()
	if premium {
		model, ok = s.strongestConfiguredModel()
	}
	if !ok {
		return nil, nil, false
	}

	criticalPath := premium
	reason := fmt.Sprintf(
		"smart-router safe-control: decision_source=rule rule_id=%s action=%s confidence=%.2f evidence=%s",
		ruleID,
		action,
		confidence,
		strings.Join(evidence, "; "),
	)

	routing := &plugin.RoutingDecision{
		Pool:   model.Pool,
		Model:  model.Name,
		Reason: reason,
	}
	history := &DecisionResponse{
		Model:           model.Name,
		TurnType:        turnType,
		HypothesisState: hypothesisState,
		CriticalPath:    &criticalPath,
		Recoverability:  recoverabilityForRule(premium),
		BudgetAction:    action,
		ContextSummary:  summary,
		Reason:          shortReason,
	}
	s.applyRouteBudget(req, routing, action)
	s.attachEpisodeMetadata(req, routing, "continue")
	return routing, history, true
}

func recoverabilityForRule(premium bool) string {
	if premium {
		return "hard"
	}
	return "easy"
}

func (s *SmartRouter) observeSafeControlState(req *http.Request, parsed *parsedRequest) safeControlObservation {
	fingerprint := safeControlErrorFingerprint(parsed.LatestUserMsg)
	key := decisionHistoryKey(req)
	if key == "" {
		state := safeControlState{}
		if fingerprint != "" {
			state.LastErrorFingerprint = fingerprint
			state.SameFailureCount = 1
		}
		return safeControlObservation{ErrorFingerprint: fingerprint, State: state}
	}

	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if s.controlStates == nil {
		s.controlStates = make(map[string]*safeControlState)
	}
	state := s.controlStates[key]
	if state == nil {
		state = &safeControlState{}
		s.controlStates[key] = state
	}

	if fingerprint != "" {
		if fingerprint == state.LastErrorFingerprint {
			state.SameFailureCount++
		} else {
			state.LastErrorFingerprint = fingerprint
			state.SameFailureCount = 1
		}
	} else if looksLikeProgressEvidence(parsed.LatestUserMsg) {
		state.LastErrorFingerprint = ""
		state.LastEscalatedFingerprint = ""
		state.SameFailureCount = 0
		state.ConsecutiveCheapProbes = 0
	}

	return safeControlObservation{
		ErrorFingerprint: fingerprint,
		State:            *state,
	}
}

func (s *SmartRouter) markSafeControlEscalation(req *http.Request, fingerprint string) {
	key := decisionHistoryKey(req)
	if key == "" || fingerprint == "" {
		return
	}

	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if s.controlStates == nil {
		s.controlStates = make(map[string]*safeControlState)
	}
	state := s.controlStates[key]
	if state == nil {
		state = &safeControlState{}
		s.controlStates[key] = state
	}
	state.LastEscalatedFingerprint = fingerprint
}

func (s *SmartRouter) recordSafeControlOutcome(req *http.Request, selectedModel string) {
	cfg := s.safeControlConfig()
	if !cfg.Enabled || selectedModel == "" {
		return
	}
	key := decisionHistoryKey(req)
	if key == "" {
		return
	}

	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if s.controlStates == nil {
		s.controlStates = make(map[string]*safeControlState)
	}
	state := s.controlStates[key]
	if state == nil {
		state = &safeControlState{}
		s.controlStates[key] = state
	}

	if s.isStrongestConfiguredModel(selectedModel) {
		state.ConsecutivePremium++
		state.ConsecutiveCheapProbes = 0
		if state.ConsecutivePremium >= cfg.PremiumCooldownAfter && state.CooldownRemaining == 0 {
			state.CooldownRemaining = cfg.PremiumCooldownTurns
		}
		return
	}

	state.ConsecutivePremium = 0
	if s.isCheapestConfiguredModel(selectedModel) {
		state.ConsecutiveCheapProbes++
	} else {
		state.ConsecutiveCheapProbes = 0
	}
	if state.CooldownRemaining > 0 {
		state.CooldownRemaining--
	}
}

func (s *SmartRouter) cheapestConfiguredModel() (ModelEntry, bool) {
	if len(s.menu) == 0 {
		return ModelEntry{}, false
	}
	cheapest := s.menu[0]
	cheapestCost := cheapest.InputPrice + cheapest.OutputPrice
	for _, model := range s.menu[1:] {
		cost := model.InputPrice + model.OutputPrice
		if cost < cheapestCost {
			cheapest = model
			cheapestCost = cost
		}
	}
	return cheapest, true
}

func (s *SmartRouter) isStrongestConfiguredModel(model string) bool {
	strongest, ok := s.strongestConfiguredModel()
	if !ok {
		return false
	}
	return modelsMatch(model, strongest.Name)
}

func (s *SmartRouter) isCheapestConfiguredModel(model string) bool {
	cheapest, ok := s.cheapestConfiguredModel()
	if !ok {
		return false
	}
	return modelsMatch(model, cheapest.Name)
}

func modelsMatch(a, b string) bool {
	aAliases := pinnedModelAliases(a)
	if containsModelAlias(aAliases, b) {
		return true
	}
	bAliases := pinnedModelAliases(b)
	return containsModelAlias(bAliases, a)
}

func normalizeForRules(message string) string {
	return strings.ToLower(strings.Join(strings.Fields(message), " "))
}

func looksLikeFileReadOrSearch(message string) bool {
	if looksLikeImplementationOrEdit(message) {
		return false
	}
	if containsAny(message, []string{
		"root cause",
		"debug",
		"diagnose",
		"failing",
		"failed",
		"hypothesis",
		"counterexample",
		"hidden test",
	}) {
		return false
	}
	return containsAny(message, []string{
		"read file",
		"open file",
		"inspect file",
		"view file",
		"look at ",
		"search ",
		"grep",
		"ripgrep",
		" rg ",
		"`rg",
		"find references",
		"list files",
		" ls ",
		"`ls",
		" cat ",
		"`cat",
		"sed -n",
		" head ",
		"`head",
		" tail ",
		"`tail",
		"wc -l",
		"tree",
		"读取文件",
		"查看文件",
		"搜索",
	})
}

func looksLikeExistingTestExecution(message string) bool {
	if containsAny(message, []string{
		"debug",
		"diagnose",
		"root cause",
		"fix failing",
		"failing test",
		"failed test",
		"test failed",
		"failure",
		"error:",
		"panic:",
		"traceback",
	}) {
		return false
	}
	return containsAny(message, []string{
		"run tests",
		"run the tests",
		"rerun tests",
		"run unit tests",
		"existing tests",
		"go test",
		"pytest",
		"npm test",
		"pnpm test",
		"yarn test",
		"cargo test",
		"make test",
		"mvn test",
		"gradle test",
		"run verifier",
		"run validation",
	})
}

func looksLikeFixedFormatOutput(message string) bool {
	if looksLikePremiumRequired(message) || looksLikeImplementationOrEdit(message) {
		return false
	}
	return containsAny(message, []string{
		"json only",
		"return json",
		"respond with json",
		"valid json",
		"fixed format",
		"output schema",
		"exact output",
		"return exactly",
		"respond exactly",
		"no prose",
		"do not include any text outside",
		"strict format",
		"固定格式",
		"只返回 json",
	})
}

func looksLikeHypothesisContradiction(message string) bool {
	if strings.Contains(message, "hypothesis") && containsAny(message, []string{"contradict", "wrong", "invalid", "failed", "false"}) {
		return true
	}
	if strings.Contains(message, "assumption") && containsAny(message, []string{"wrong", "invalid", "false", "failed"}) {
		return true
	}
	return containsAny(message, []string{
		"approach failed",
		"approach is wrong",
		"approach was wrong",
		"dead end",
		"counterexample",
		"invalidates the approach",
		"hidden test failed",
		"hidden tests failed",
		"grader failed",
		"local tests pass but",
		"假设被反证",
		"思路错了",
	})
}

func looksLikePremiumRequired(message string) bool {
	return containsAny(message, []string{
		"task_complete",
		"mark the task as complete",
		"final answer",
		"finalization",
		"root cause",
		"diagnose",
		"debug",
		"failing",
		"failed",
		"panic:",
		"exception",
		"traceback",
		"concurrency",
		"data corruption",
		"schema migration",
		"architecture",
		"multi-file",
		"security-critical",
		"cryptographic",
		"hidden test",
		"hypothesis",
		"counterexample",
	})
}

func looksLikeImplementationOrEdit(message string) bool {
	return containsAny(message, []string{
		"implement",
		"implementation",
		"write code",
		"write the code",
		"edit ",
		"modify ",
		"patch",
		"apply_patch",
		"create file",
		"create the file",
		"update file",
		"update the file",
		"change code",
		"rewrite",
		"refactor",
		"修复",
		"实现",
		"改代码",
		"编辑",
	})
}

func looksLikeProgressEvidence(message string) bool {
	message = normalizeForRules(message)
	return containsAny(message, []string{
		"all tests passed",
		"tests passed",
		"test passed",
		"success",
		"resolved",
		"fixed",
		"verifier passed",
	})
}

func safeControlErrorFingerprint(message string) string {
	message = strings.ToLower(strings.ReplaceAll(message, "\r", "\n"))
	if looksLikeProviderFailure(message) {
		return ""
	}

	fallback := ""
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !looksLikeErrorSignalLine(line) {
			continue
		}
		normalized := normalizeErrorFingerprint(line)
		if isGenericErrorSignalLine(line) {
			if fallback == "" {
				fallback = normalized
			}
			continue
		}
		if looksLikeSpecificErrorLine(line) {
			return normalized
		}
		if fallback == "" {
			fallback = normalized
		}
	}
	return fallback
}

func looksLikeProviderFailure(message string) bool {
	return containsAny(message, []string{
		"provider timeout",
		"provider timed out",
		"openrouter 5",
		"http 502",
		"http 503",
		"http 504",
		"502 bad gateway",
		"503 service unavailable",
		"504 gateway timeout",
		"rate limit",
		"429 too many requests",
		"upstream eof",
	})
}

func looksLikeErrorSignalLine(line string) bool {
	return containsAny(line, []string{
		"error:",
		"exception",
		"traceback",
		"panic:",
		"failed",
		"failure",
		"assert",
		"exit status",
		"exit code",
		"segmentation fault",
		"timed out",
	})
}

func isGenericErrorSignalLine(line string) bool {
	line = strings.TrimSpace(line)
	return line == "traceback (most recent call last):" ||
		line == "error:" ||
		line == "exception:" ||
		line == "failed" ||
		line == "failure"
}

func looksLikeSpecificErrorLine(line string) bool {
	return containsAny(line, []string{
		"assertionerror",
		"valueerror",
		"keyerror",
		"typeerror",
		"runtimeerror",
		"modulenotfounderror",
		"importerror",
		"panic:",
		"error:",
		"exception:",
		"exit status",
		"exit code",
		"segmentation fault",
		"timed out",
	})
}

func normalizeErrorFingerprint(line string) string {
	line = fingerprintLocationRE.ReplaceAllString(line, "<loc>")
	line = fingerprintNumberRE.ReplaceAllString(line, "#")
	line = fingerprintSpaceRE.ReplaceAllString(line, " ")
	return compactDecisionText(strings.TrimSpace(line), 180)
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
