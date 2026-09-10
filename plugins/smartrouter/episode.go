package smartrouter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aware/gateway/internal/plugin"
)

const (
	defaultEpisodeRecentEvents                 = 5
	defaultEpisodeLengthStreakThreshold        = 1
	defaultEpisodeLengthWindowThreshold        = 2
	defaultEpisodeMaxTokensMultiplier          = 3.0
	defaultEpisodeTimeoutMultiplier            = 2.0
	defaultEpisodeMaxTokensCeiling             = 8192
	defaultEpisodeTimeoutMsCeiling             = 240000
	defaultEpisodeNoProgressAgentCallThreshold = 50

	episodeOperationContinue  = "continue"
	episodeOperationInterrupt = "interrupt"
	episodeOperationResume    = "resume"
	episodeOperationGlobal    = "global"
	episodeOperationUnknown   = "unknown"

	completionReadinessNone              = "none"
	completionReadinessDeliveryCandidate = "delivery_candidate"
	completionReadinessValidationFailed  = "validation_failed"
	completionReadinessValidationPassed  = "validation_passed"
	completionReadinessVerifierFailed    = "verifier_failed"
	completionReadinessVerifierPassed    = "verifier_passed"

	routeOutcomePending          = "pending"
	routeOutcomeToolCall         = "tool_call"
	routeOutcomeDeliveryChanged  = "delivery_changed"
	routeOutcomeWorkspaceChanged = "workspace_changed"
	routeOutcomeTestPassed       = "test_passed"
	routeOutcomeTestFailed       = "test_failed"
	routeOutcomeVerifierPassed   = "verifier_passed"
	routeOutcomeVerifierFailed   = "verifier_failed"
	routeOutcomeNoProgress       = "no_progress"
	routeOutcomeRunException     = "run_exception"
	routeOutcomeNone             = "none"
)

// EpisodeConfig enables a small in-memory event projection for one task line.
// It intentionally keeps only enough state to make the next route less blind.
type EpisodeConfig struct {
	Enabled               bool    `yaml:"enabled" json:"enabled"`
	RecentEvents          int     `yaml:"recent_events" json:"recent_events"`
	LengthStreakThreshold int     `yaml:"length_streak_threshold" json:"length_streak_threshold"`
	LengthWindowThreshold int     `yaml:"length_window_threshold" json:"length_window_threshold"`
	MaxTokensMultiplier   float64 `yaml:"max_tokens_multiplier" json:"max_tokens_multiplier"`
	TimeoutMultiplier     float64 `yaml:"timeout_multiplier" json:"timeout_multiplier"`
	MaxTokensCeiling      int     `yaml:"max_tokens_ceiling" json:"max_tokens_ceiling"`
	TimeoutMsCeiling      int     `yaml:"timeout_ms_ceiling" json:"timeout_ms_ceiling"`
}

type EpisodeEvent struct {
	ID           string
	Kind         string
	Source       string
	Outcome      string
	Model        string
	BudgetAction string
	FinishReason string
	Status       int
	LatencyMs    int64
	Cost         float64
	TotalTokens  int
	Timestamp    time.Time
	Observation  map[string]any
	EvidenceRefs []string
}

type EpisodeState struct {
	ID                          string
	Version                     int
	CallCount                   int
	TotalCost                   float64
	TotalTokens                 int
	ToolCallCount               int
	FileWriteCount              int
	TestRunCount                int
	TestPassedCount             int
	TestFailedCount             int
	DeliveryFileWriteCount      int
	CandidateProgressCount      int
	StrongProgressCount         int
	NoProgressEventCount        int
	ActiveNoProgress            bool
	EventsSinceProgress         int
	LLMCallsSinceProgress       int
	LengthPressureSinceProgress int
	LastProgressEventID         string
	LastProgressKind            string
	VerifierReward              float64
	CompletionReadiness         string
	LastDeliveryEventID         string
	NoProgressSeverity          string
	LastModel                   string
	LastBudgetAction            string
	LastFinishReason            string
	LastRouteTraceID            string
	LastRouteOutcomeLabel       string
	LastRouteOutcomeEventID     string
	LastRouteOutcomeProgress    bool
	LastRouteOutcomeEventCount  int
	RouteOutcomeEventCount      int
	RouteOutcomeProgressCount   int
	RouteOutcomeNegativeCount   int
	LastFailureFingerprint      string
	SameFailureFingerprintCount int
	FailureFrontierSize         int
	ConsecutiveLengthFinishes   int
	RecentLengthFinishes        int
	ConsecutiveErrors           int
	RecentEvents                []EpisodeEvent
	SeenEventIDs                map[string]struct{}
}

type EpisodeSession struct {
	Key             string
	ActiveEpisodeID string
	Stack           []string
	NextEpisode     int
}

type EpisodeResolution struct {
	EpisodeID  string
	Operation  string
	Confidence float64
	Evidence   []string
}

type EpisodeSnapshot struct {
	ID                          string
	Version                     int
	CallCount                   int
	TotalCost                   float64
	TotalTokens                 int
	ToolCallCount               int
	FileWriteCount              int
	TestRunCount                int
	TestPassedCount             int
	TestFailedCount             int
	DeliveryFileWriteCount      int
	CandidateProgressCount      int
	StrongProgressCount         int
	NoProgressEventCount        int
	ActiveNoProgress            bool
	EventsSinceProgress         int
	LLMCallsSinceProgress       int
	LengthPressureSinceProgress int
	LastProgressEventID         string
	LastProgressKind            string
	VerifierReward              float64
	CompletionReadiness         string
	LastDeliveryEventID         string
	NoProgressSeverity          string
	LastModel                   string
	LastBudgetAction            string
	LastFinishReason            string
	LastRouteTraceID            string
	LastRouteOutcomeLabel       string
	LastRouteOutcomeEventID     string
	LastRouteOutcomeProgress    bool
	LastRouteOutcomeEventCount  int
	RouteOutcomeEventCount      int
	RouteOutcomeProgressCount   int
	RouteOutcomeNegativeCount   int
	LastFailureFingerprint      string
	SameFailureFingerprintCount int
	FailureFrontierSize         int
	ConsecutiveLengthFinishes   int
	RecentLengthFinishes        int
	ConsecutiveErrors           int
	RecentEvents                []EpisodeEvent
}

func (s *SmartRouter) resolveEpisodeForRequest(req *http.Request, parsed *parsedRequest) EpisodeResolution {
	cfg := s.episodeConfig()
	if req == nil || parsed == nil || !cfg.Enabled {
		return EpisodeResolution{}
	}

	explicitEpisodeID := strings.TrimSpace(req.Header.Get("X-Episode-ID"))
	sessionKey := episodeSessionKeyFromRequest(req)
	operation := normalizeEpisodeOperation(req.Header.Get("X-Episode-Operation"))
	if explicitEpisodeID != "" {
		if operation == "" {
			operation = episodeOperationContinue
		}
		s.rememberExplicitEpisode(sessionKey, explicitEpisodeID, operation)
		setEpisodeHeaders(req, explicitEpisodeID, operation)
		return EpisodeResolution{
			EpisodeID:  explicitEpisodeID,
			Operation:  operation,
			Confidence: 1,
			Evidence:   []string{"explicit_episode_id"},
		}
	}
	if sessionKey == "" {
		return EpisodeResolution{}
	}

	message := normalizeForRules(parsed.LatestUserMsg)
	if operation == "" {
		operation = inferEpisodeOperation(message)
	}
	resolution := s.resolveSessionEpisode(sessionKey, operation)
	setEpisodeHeaders(req, resolution.EpisodeID, resolution.Operation)
	return resolution
}

func (s *SmartRouter) resolveSessionEpisode(sessionKey, operation string) EpisodeResolution {
	if operation == "" {
		operation = episodeOperationContinue
	}

	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]*EpisodeSession)
	}
	session := s.sessions[sessionKey]
	if session == nil {
		session = &EpisodeSession{Key: sessionKey, ActiveEpisodeID: sessionKey, Stack: []string{sessionKey}}
		s.sessions[sessionKey] = session
	}
	if session.ActiveEpisodeID == "" {
		session.ActiveEpisodeID = sessionKey
	}
	if len(session.Stack) == 0 {
		session.Stack = []string{session.ActiveEpisodeID}
	}

	switch operation {
	case episodeOperationInterrupt:
		session.NextEpisode++
		session.ActiveEpisodeID = fmt.Sprintf("%s#episode-%d", sessionKey, session.NextEpisode)
		session.Stack = append(session.Stack, session.ActiveEpisodeID)
		return EpisodeResolution{
			EpisodeID:  session.ActiveEpisodeID,
			Operation:  episodeOperationInterrupt,
			Confidence: 0.86,
			Evidence:   []string{"detected_side_task_language"},
		}
	case episodeOperationResume:
		if len(session.Stack) > 1 {
			session.Stack = session.Stack[:len(session.Stack)-1]
			session.ActiveEpisodeID = session.Stack[len(session.Stack)-1]
		} else {
			session.ActiveEpisodeID = sessionKey
			session.Stack = []string{sessionKey}
		}
		return EpisodeResolution{
			EpisodeID:  session.ActiveEpisodeID,
			Operation:  episodeOperationResume,
			Confidence: 0.88,
			Evidence:   []string{"detected_resume_language"},
		}
	case episodeOperationGlobal:
		return EpisodeResolution{
			EpisodeID:  session.ActiveEpisodeID,
			Operation:  episodeOperationGlobal,
			Confidence: 0.82,
			Evidence:   []string{"detected_global_constraint_language"},
		}
	case episodeOperationUnknown:
		return EpisodeResolution{
			EpisodeID:  session.ActiveEpisodeID,
			Operation:  episodeOperationUnknown,
			Confidence: 0.5,
			Evidence:   []string{"unknown_episode_operation"},
		}
	default:
		return EpisodeResolution{
			EpisodeID:  session.ActiveEpisodeID,
			Operation:  episodeOperationContinue,
			Confidence: 0.8,
			Evidence:   []string{"active_episode"},
		}
	}
}

func (s *SmartRouter) rememberExplicitEpisode(sessionKey, episodeID, operation string) {
	if sessionKey == "" || episodeID == "" {
		return
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]*EpisodeSession)
	}
	session := s.sessions[sessionKey]
	if session == nil {
		session = &EpisodeSession{Key: sessionKey, ActiveEpisodeID: episodeID, Stack: []string{episodeID}}
		s.sessions[sessionKey] = session
	}
	if operation == episodeOperationInterrupt && session.ActiveEpisodeID != episodeID {
		session.Stack = appendUniqueEpisode(session.Stack, episodeID)
	}
	if operation == episodeOperationResume {
		session.Stack = trimStackToEpisode(session.Stack, episodeID)
	}
	session.ActiveEpisodeID = episodeID
	if len(session.Stack) == 0 {
		session.Stack = []string{episodeID}
	}
}

func setEpisodeHeaders(req *http.Request, episodeID, operation string) {
	if req == nil || episodeID == "" {
		return
	}
	req.Header.Set("X-Episode-ID", episodeID)
	if operation != "" {
		req.Header.Set("X-Episode-Operation", operation)
	}
}

func inferEpisodeOperation(message string) string {
	switch {
	case looksLikeEpisodeResume(message):
		return episodeOperationResume
	case looksLikeEpisodeInterrupt(message):
		return episodeOperationInterrupt
	case looksLikeEpisodeGlobal(message):
		return episodeOperationGlobal
	default:
		return episodeOperationContinue
	}
}

func looksLikeEpisodeInterrupt(message string) bool {
	return containsAny(message, []string{
		"顺便",
		"支线",
		"临时",
		"另外",
		"先处理",
		"先看一下",
		"插一下",
		"插个",
		"side task",
		"side quest",
		"quick detour",
		"separate task",
		"unrelated",
		"before that",
	})
}

func looksLikeEpisodeResume(message string) bool {
	return containsAny(message, []string{
		"回到",
		"回主线",
		"回主任务",
		"恢复",
		"继续主任务",
		"继续原来的",
		"继续之前",
		"继续刚才",
		"resume",
		"back to",
		"return to",
		"switch back",
	})
}

func looksLikeEpisodeGlobal(message string) bool {
	return containsAny(message, []string{
		"全局",
		"以后都",
		"所有任务",
		"默认",
		"总是",
		"from now on",
		"for all tasks",
		"always",
		"default behavior",
	})
}

func normalizeEpisodeOperation(operation string) string {
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case episodeOperationContinue, episodeOperationInterrupt, episodeOperationResume, episodeOperationGlobal, episodeOperationUnknown:
		return strings.ToLower(strings.TrimSpace(operation))
	default:
		return ""
	}
}

func (s *SmartRouter) episodeConfig() EpisodeConfig {
	cfg := s.cfg.EpisodeRuntime
	if cfg.RecentEvents <= 0 {
		cfg.RecentEvents = defaultEpisodeRecentEvents
	}
	if cfg.LengthStreakThreshold <= 0 {
		cfg.LengthStreakThreshold = defaultEpisodeLengthStreakThreshold
	}
	if cfg.LengthWindowThreshold <= 0 {
		cfg.LengthWindowThreshold = defaultEpisodeLengthWindowThreshold
	}
	if cfg.MaxTokensMultiplier <= 0 {
		cfg.MaxTokensMultiplier = defaultEpisodeMaxTokensMultiplier
	}
	if cfg.MaxTokensMultiplier < 1 {
		cfg.MaxTokensMultiplier = 1
	}
	if cfg.TimeoutMultiplier <= 0 {
		cfg.TimeoutMultiplier = defaultEpisodeTimeoutMultiplier
	}
	if cfg.TimeoutMultiplier < 1 {
		cfg.TimeoutMultiplier = 1
	}
	if cfg.MaxTokensCeiling <= 0 {
		cfg.MaxTokensCeiling = defaultEpisodeMaxTokensCeiling
	}
	if cfg.TimeoutMsCeiling <= 0 {
		cfg.TimeoutMsCeiling = defaultEpisodeTimeoutMsCeiling
	}
	return cfg
}

func (s *SmartRouter) Record(record *plugin.AuditRecord) error {
	if record == nil || !s.cfg.Enabled {
		return nil
	}
	key := episodeKeyFromRecord(record)
	if key == "" {
		return nil
	}
	completionGuardrail := record.BudgetAction == budgetActionCompletionGuardrail
	if isDecisionModelRecord(record) {
		return nil
	}
	cfg := s.episodeConfig()
	if !cfg.Enabled {
		if completionGuardrail {
			s.clearDecisionStateByKey(key)
		}
		return nil
	}

	event := EpisodeEvent{
		ID:           record.TraceID,
		Kind:         "llm_call",
		Source:       "gateway_trace",
		Outcome:      episodeOutcome(record),
		Model:        record.RoutedModel,
		BudgetAction: record.BudgetAction,
		FinishReason: strings.ToLower(strings.TrimSpace(record.FinishReason)),
		Status:       record.Status,
		LatencyMs:    record.LatencyMs,
		Cost:         record.Cost,
		TotalTokens:  record.TotalTokens,
		Timestamp:    record.Timestamp,
	}
	if event.Model == "" {
		event.Model = record.Model
	}

	s.episodeMu.Lock()
	if s.episodes == nil {
		s.episodes = make(map[string]*EpisodeState)
	}
	state := s.episodes[key]
	if state == nil {
		state = &EpisodeState{ID: key}
		s.episodes[key] = state
	}
	projectUniqueEpisodeEvent(state, event, cfg)
	if record.EpisodeID == "" {
		record.EpisodeID = key
	}
	if record.EpisodeOp == "" {
		record.EpisodeOp = "continue"
	}
	record.StateAfter = renderEpisodeStateJSON(snapshotFromEpisodeState(state))
	s.episodeMu.Unlock()

	if completionGuardrail {
		s.clearDecisionStateByKey(key)
	}
	return nil
}

func (s *SmartRouter) RecordEpisodeEvent(event *plugin.EpisodeEvent) error {
	if event == nil || !s.cfg.Enabled {
		return nil
	}
	key := strings.TrimSpace(event.EpisodeID)
	if key == "" {
		key = strings.TrimSpace(event.SessionID)
	}
	if key == "" {
		key = strings.TrimSpace(event.TrialName)
	}
	if key == "" {
		return nil
	}
	cfg := s.episodeConfig()
	if !cfg.Enabled {
		return nil
	}

	projected := episodeEventFromPluginEvent(event)

	s.episodeMu.Lock()
	if s.episodes == nil {
		s.episodes = make(map[string]*EpisodeState)
	}
	state := s.episodes[key]
	if state == nil {
		state = &EpisodeState{ID: key}
		s.episodes[key] = state
	}
	projectUniqueEpisodeEvent(state, projected, cfg)
	s.episodeMu.Unlock()
	return nil
}

func episodeEventFromPluginEvent(event *plugin.EpisodeEvent) EpisodeEvent {
	if event == nil {
		return EpisodeEvent{}
	}
	projected := EpisodeEvent{
		ID:           event.EventID,
		Kind:         strings.TrimSpace(event.Kind),
		Source:       strings.TrimSpace(event.Source),
		Outcome:      stringFromObservation(event.Observation, "outcome"),
		Model:        firstNonEmpty(stringFromObservation(event.Observation, "routed_model"), stringFromObservation(event.Observation, "model")),
		BudgetAction: stringFromObservation(event.Observation, "budget_action"),
		FinishReason: strings.ToLower(strings.TrimSpace(stringFromObservation(event.Observation, "finish_reason"))),
		Status:       intFromObservation(event.Observation, "status"),
		LatencyMs:    int64(intFromObservation(event.Observation, "latency_ms")),
		Cost:         floatFromObservation(event.Observation, "cost_usd"),
		TotalTokens:  intFromObservation(event.Observation, "total_tokens"),
		Timestamp:    event.Timestamp,
		Observation:  copyObservation(event.Observation),
		EvidenceRefs: append([]string(nil), event.EvidenceRefs...),
	}
	if projected.Kind == "" {
		projected.Kind = "unknown"
	}
	if projected.Outcome == "" && projected.Kind == "llm_call" {
		projected.Outcome = "unknown"
	}
	return projected
}

func episodeEventFromTraceEntry(trace plugin.TraceEntry) EpisodeEvent {
	timestamp, _ := time.Parse(time.RFC3339Nano, trace.Timestamp)
	outcome := "unknown"
	if trace.Status >= 400 {
		outcome = "error"
	} else if strings.EqualFold(trace.FinishReason, "length") {
		outcome = "length_truncated"
	} else if strings.EqualFold(trace.FinishReason, "stop") {
		outcome = "response_completed"
	} else if trace.Status >= 200 && trace.Status < 300 && trace.FinishReason == "" && trace.TotalTokens == 0 {
		outcome = "provider_incomplete"
	}
	model := trace.RoutedModel
	if model == "" {
		model = trace.Model
	}
	return EpisodeEvent{
		ID:           trace.TraceID,
		Kind:         "llm_call",
		Source:       "audit_trace_backfill",
		Outcome:      outcome,
		Model:        model,
		BudgetAction: trace.BudgetAction,
		FinishReason: strings.ToLower(strings.TrimSpace(trace.FinishReason)),
		Status:       trace.Status,
		LatencyMs:    trace.LatencyMs,
		Cost:         trace.Cost,
		TotalTokens:  trace.TotalTokens,
		Timestamp:    timestamp,
		Observation: map[string]any{
			"trace_id":           trace.TraceID,
			"outcome":            outcome,
			"model":              trace.Model,
			"routed_model":       trace.RoutedModel,
			"pool":               trace.Pool,
			"budget_action":      trace.BudgetAction,
			"route_max_tokens":   trace.RouteMaxTokens,
			"route_timeout_ms":   trace.RouteTimeoutMs,
			"finish_reason":      trace.FinishReason,
			"status":             trace.Status,
			"latency_ms":         trace.LatencyMs,
			"cost_usd":           trace.Cost,
			"total_tokens":       trace.TotalTokens,
			"routing_reason":     trace.RoutingReason,
			"backfill_source":    "audit_trace",
			"original_step_name": trace.StepName,
		},
		EvidenceRefs: []string{"trace:" + trace.TraceID},
	}
}

func projectTestFailureState(state *EpisodeState, event EpisodeEvent) {
	fingerprint := failureFingerprintFromEvent(event)
	if fingerprint == "" {
		return
	}
	frontierSize := failureFrontierSizeFromEvent(event)
	if fingerprint == state.LastFailureFingerprint {
		if state.FailureFrontierSize <= 0 || frontierSize >= state.FailureFrontierSize {
			state.SameFailureFingerprintCount++
		} else {
			state.SameFailureFingerprintCount = 1
		}
	} else {
		state.LastFailureFingerprint = fingerprint
		state.SameFailureFingerprintCount = 1
	}
	state.FailureFrontierSize = frontierSize
}

func failureFingerprintFromEvent(event EpisodeEvent) string {
	single := stringFromObservation(event.Observation, "failure_fingerprint")
	if single != "" {
		return normalizeEpisodeFailureFingerprint(single)
	}
	multiple := stringListFromObservation(event.Observation, "failure_fingerprints")
	if len(multiple) > 0 {
		normalized := make([]string, 0, len(multiple))
		seen := make(map[string]struct{}, len(multiple))
		for _, value := range multiple {
			fingerprint := normalizeEpisodeFailureFingerprint(value)
			if fingerprint == "" {
				continue
			}
			if _, exists := seen[fingerprint]; exists {
				continue
			}
			seen[fingerprint] = struct{}{}
			normalized = append(normalized, fingerprint)
		}
		sort.Strings(normalized)
		return compactDecisionText(strings.Join(normalized, " | "), 180)
	}
	command := normalizeEpisodeFailureFingerprint(stringFromObservation(event.Observation, "command"))
	if command == "" {
		return ""
	}
	return compactDecisionText("failed command: "+command, 180)
}

func normalizeEpisodeFailureFingerprint(value string) string {
	return normalizeErrorFingerprint(strings.ToLower(strings.TrimSpace(value)))
}

func failureFrontierSizeFromEvent(event EpisodeEvent) int {
	for _, key := range []string{"failed_count", "failing_count", "failure_count", "failures"} {
		if count := intFromObservation(event.Observation, key); count > 0 {
			return count
		}
	}
	if multiple := stringListFromObservation(event.Observation, "failure_fingerprints"); len(multiple) > 0 {
		return len(multiple)
	}
	return 1
}

func completionAffectingWrite(event EpisodeEvent) bool {
	return boolFromObservation(event.Observation, "delivery_target") ||
		boolFromObservation(event.Observation, "workspace_target")
}

func beginRouteOutcomeWindow(state *EpisodeState, event EpisodeEvent) {
	state.LastRouteTraceID = event.ID
	state.LastRouteOutcomeLabel = routeOutcomePending
	state.LastRouteOutcomeEventID = ""
	state.LastRouteOutcomeProgress = false
	state.LastRouteOutcomeEventCount = 0
}

func projectRouteOutcomeState(state *EpisodeState, event EpisodeEvent) {
	if state == nil || state.CallCount == 0 {
		return
	}
	label, progress, negative := routeOutcomeFromEvent(event)
	if label == "" {
		return
	}
	state.LastRouteOutcomeLabel = label
	state.LastRouteOutcomeEventID = event.ID
	state.LastRouteOutcomeProgress = progress
	state.LastRouteOutcomeEventCount++
	state.RouteOutcomeEventCount++
	if progress {
		state.RouteOutcomeProgressCount++
	}
	if negative {
		state.RouteOutcomeNegativeCount++
	}
}

func routeOutcomeFromEvent(event EpisodeEvent) (string, bool, bool) {
	switch event.Kind {
	case "tool_call":
		return routeOutcomeToolCall, false, false
	case "file_written", "file_modified":
		if boolFromObservation(event.Observation, "delivery_target") {
			return routeOutcomeDeliveryChanged, true, false
		}
		if boolFromObservation(event.Observation, "workspace_target") ||
			intFromObservation(event.Observation, "path_count") > 0 {
			return routeOutcomeWorkspaceChanged, true, false
		}
	case "test_run":
		switch strings.ToLower(strings.TrimSpace(stringFromObservation(event.Observation, "outcome"))) {
		case "passed":
			return routeOutcomeTestPassed, true, false
		case "failed":
			return routeOutcomeTestFailed, false, true
		}
	case "test_passed":
		return routeOutcomeTestPassed, true, false
	case "test_failed":
		return routeOutcomeTestFailed, false, true
	case "verifier_result":
		if floatFromObservation(event.Observation, "reward") > 0 {
			return routeOutcomeVerifierPassed, true, false
		}
		return routeOutcomeVerifierFailed, false, true
	case "no_progress":
		return routeOutcomeNoProgress, false, true
	case "run_exception":
		return routeOutcomeRunException, false, true
	}
	return "", false, false
}

func routeOutcomeLabelForSnapshot(snapshot EpisodeSnapshot) string {
	if snapshot.LastRouteTraceID == "" && snapshot.LastRouteOutcomeLabel == "" {
		return routeOutcomeNone
	}
	return valueOrDefault(snapshot.LastRouteOutcomeLabel, routeOutcomePending)
}

func projectUniqueEpisodeEvent(state *EpisodeState, event EpisodeEvent, cfg EpisodeConfig) bool {
	if event.ID != "" {
		if state.SeenEventIDs == nil {
			state.SeenEventIDs = make(map[string]struct{})
		}
		if _, exists := state.SeenEventIDs[event.ID]; exists {
			return false
		}
		state.SeenEventIDs[event.ID] = struct{}{}
	}
	projectEpisodeEvent(state, event, cfg)
	return true
}

func projectEpisodeEvent(state *EpisodeState, event EpisodeEvent, cfg EpisodeConfig) {
	state.Version++
	progress := isStrongProgressEvent(event) || isCandidateProgressEvent(event)

	switch event.Kind {
	case "llm_call":
		state.CallCount++
		state.TotalCost += event.Cost
		state.TotalTokens += event.TotalTokens
		state.LastModel = event.Model
		state.LastBudgetAction = event.BudgetAction
		state.LastFinishReason = event.FinishReason

		if event.FinishReason == "length" || event.Outcome == "length_truncated" {
			state.ConsecutiveLengthFinishes++
			state.LengthPressureSinceProgress++
		} else if event.FinishReason != "" || event.Status < 400 {
			state.ConsecutiveLengthFinishes = 0
		}

		if event.Status >= 400 || event.Outcome == "error" {
			state.ConsecutiveErrors++
		} else {
			state.ConsecutiveErrors = 0
		}
		state.LLMCallsSinceProgress++
		beginRouteOutcomeWindow(state, event)
	case "tool_call":
		state.ToolCallCount++
	case "file_written", "file_modified":
		state.FileWriteCount++
		if boolFromObservation(event.Observation, "delivery_target") {
			state.DeliveryFileWriteCount++
			state.LastDeliveryEventID = event.ID
		}
		if completionAffectingWrite(event) {
			state.CompletionReadiness = completionReadinessDeliveryCandidate
			state.VerifierReward = 0
		}
	case "test_run":
		state.TestRunCount++
		switch strings.ToLower(strings.TrimSpace(stringFromObservation(event.Observation, "outcome"))) {
		case "passed":
			state.TestPassedCount++
			state.CompletionReadiness = completionReadinessValidationPassed
		case "failed":
			state.TestFailedCount++
			state.CompletionReadiness = completionReadinessValidationFailed
			state.VerifierReward = 0
			projectTestFailureState(state, event)
		}
	case "test_passed":
		state.TestPassedCount++
		state.CompletionReadiness = completionReadinessValidationPassed
	case "test_failed":
		state.TestFailedCount++
		state.CompletionReadiness = completionReadinessValidationFailed
		state.VerifierReward = 0
		projectTestFailureState(state, event)
	case "verifier_result":
		state.VerifierReward = floatFromObservation(event.Observation, "reward")
		if state.VerifierReward > 0 {
			state.CompletionReadiness = completionReadinessVerifierPassed
		} else {
			state.CompletionReadiness = completionReadinessVerifierFailed
		}
	case "no_progress":
		state.NoProgressEventCount++
		state.ActiveNoProgress = true
	}
	if event.Kind != "llm_call" {
		projectRouteOutcomeState(state, event)
	}

	if progress {
		if isStrongProgressEvent(event) {
			state.StrongProgressCount++
		} else {
			state.CandidateProgressCount++
		}
		state.EventsSinceProgress = 0
		state.LLMCallsSinceProgress = 0
		state.LengthPressureSinceProgress = 0
		state.ConsecutiveLengthFinishes = 0
		state.ActiveNoProgress = false
		if clearsFailureFrontier(event) {
			state.LastFailureFingerprint = ""
			state.SameFailureFingerprintCount = 0
			state.FailureFrontierSize = 0
		}
		state.LastProgressEventID = event.ID
		state.LastProgressKind = event.Kind
	} else {
		state.EventsSinceProgress++
	}

	state.RecentEvents = append(state.RecentEvents, event)
	if len(state.RecentEvents) > cfg.RecentEvents {
		state.RecentEvents = state.RecentEvents[len(state.RecentEvents)-cfg.RecentEvents:]
	}
	state.RecentLengthFinishes = countLengthFinishes(state.RecentEvents)
	state.NoProgressSeverity = noProgressSeverity(state, cfg)
}

func (s *SmartRouter) episodeSnapshot(req *http.Request) EpisodeSnapshot {
	cfg := s.episodeConfig()
	if !cfg.Enabled {
		return EpisodeSnapshot{}
	}
	key := episodeKeyFromRequest(req)
	if key == "" {
		return EpisodeSnapshot{}
	}
	s.ensureEpisodeStateLoaded(key, cfg)

	s.episodeMu.Lock()
	defer s.episodeMu.Unlock()
	state := s.episodes[key]
	if state == nil {
		return EpisodeSnapshot{ID: key}
	}
	return snapshotFromEpisodeState(state)
}

func snapshotFromEpisodeState(state *EpisodeState) EpisodeSnapshot {
	if state == nil {
		return EpisodeSnapshot{}
	}
	recent := make([]EpisodeEvent, len(state.RecentEvents))
	copy(recent, state.RecentEvents)
	return EpisodeSnapshot{
		ID:                          state.ID,
		Version:                     state.Version,
		CallCount:                   state.CallCount,
		TotalCost:                   state.TotalCost,
		TotalTokens:                 state.TotalTokens,
		ToolCallCount:               state.ToolCallCount,
		FileWriteCount:              state.FileWriteCount,
		TestRunCount:                state.TestRunCount,
		TestPassedCount:             state.TestPassedCount,
		TestFailedCount:             state.TestFailedCount,
		DeliveryFileWriteCount:      state.DeliveryFileWriteCount,
		CandidateProgressCount:      state.CandidateProgressCount,
		StrongProgressCount:         state.StrongProgressCount,
		NoProgressEventCount:        state.NoProgressEventCount,
		ActiveNoProgress:            state.ActiveNoProgress,
		EventsSinceProgress:         state.EventsSinceProgress,
		LLMCallsSinceProgress:       state.LLMCallsSinceProgress,
		LengthPressureSinceProgress: state.LengthPressureSinceProgress,
		LastProgressEventID:         state.LastProgressEventID,
		LastProgressKind:            state.LastProgressKind,
		VerifierReward:              state.VerifierReward,
		CompletionReadiness:         state.CompletionReadiness,
		LastDeliveryEventID:         state.LastDeliveryEventID,
		NoProgressSeverity:          state.NoProgressSeverity,
		LastModel:                   state.LastModel,
		LastBudgetAction:            state.LastBudgetAction,
		LastFinishReason:            state.LastFinishReason,
		LastRouteTraceID:            state.LastRouteTraceID,
		LastRouteOutcomeLabel:       state.LastRouteOutcomeLabel,
		LastRouteOutcomeEventID:     state.LastRouteOutcomeEventID,
		LastRouteOutcomeProgress:    state.LastRouteOutcomeProgress,
		LastRouteOutcomeEventCount:  state.LastRouteOutcomeEventCount,
		RouteOutcomeEventCount:      state.RouteOutcomeEventCount,
		RouteOutcomeProgressCount:   state.RouteOutcomeProgressCount,
		RouteOutcomeNegativeCount:   state.RouteOutcomeNegativeCount,
		LastFailureFingerprint:      state.LastFailureFingerprint,
		SameFailureFingerprintCount: state.SameFailureFingerprintCount,
		FailureFrontierSize:         state.FailureFrontierSize,
		ConsecutiveLengthFinishes:   state.ConsecutiveLengthFinishes,
		RecentLengthFinishes:        state.RecentLengthFinishes,
		ConsecutiveErrors:           state.ConsecutiveErrors,
		RecentEvents:                recent,
	}
}

func (s *SmartRouter) renderEpisodeSnapshot(snapshot EpisodeSnapshot) string {
	if snapshot.ID == "" {
		return ""
	}
	lines := []string{
		fmt.Sprintf(
			"episode_id=%s state_version=%d calls=%d total_cost=$%.4f total_tokens=%d length_streak=%d recent_length=%d error_streak=%d last_model=%s last_budget=%s last_finish=%s",
			snapshot.ID,
			snapshot.Version,
			snapshot.CallCount,
			snapshot.TotalCost,
			snapshot.TotalTokens,
			snapshot.ConsecutiveLengthFinishes,
			snapshot.RecentLengthFinishes,
			snapshot.ConsecutiveErrors,
			valueOrUnknown(snapshot.LastModel),
			valueOrUnknown(snapshot.LastBudgetAction),
			valueOrUnknown(snapshot.LastFinishReason),
		),
		fmt.Sprintf(
			"progress tools=%d file_writes=%d test_runs=%d test_passed=%d test_failed=%d candidate=%d strong=%d no_progress_events=%d active_no_progress=%t no_progress=%s events_since_progress=%d llm_since_progress=%d length_since_progress=%d last_progress=%s verifier_reward=%.3f",
			snapshot.ToolCallCount,
			snapshot.FileWriteCount,
			snapshot.TestRunCount,
			snapshot.TestPassedCount,
			snapshot.TestFailedCount,
			snapshot.CandidateProgressCount,
			snapshot.StrongProgressCount,
			snapshot.NoProgressEventCount,
			snapshot.ActiveNoProgress,
			valueOrDefault(snapshot.NoProgressSeverity, "none"),
			snapshot.EventsSinceProgress,
			snapshot.LLMCallsSinceProgress,
			snapshot.LengthPressureSinceProgress,
			valueOrUnknown(snapshot.LastProgressKind),
			snapshot.VerifierReward,
		),
		fmt.Sprintf(
			"delivery readiness=%s delivery_file_writes=%d last_delivery=%s",
			valueOrDefault(snapshot.CompletionReadiness, completionReadinessNone),
			snapshot.DeliveryFileWriteCount,
			valueOrUnknown(snapshot.LastDeliveryEventID),
		),
		fmt.Sprintf(
			"route_outcome trace=%s label=%s event=%s progress=%t events=%d total_events=%d progress_events=%d negative_events=%d",
			valueOrUnknown(snapshot.LastRouteTraceID),
			routeOutcomeLabelForSnapshot(snapshot),
			valueOrUnknown(snapshot.LastRouteOutcomeEventID),
			snapshot.LastRouteOutcomeProgress,
			snapshot.LastRouteOutcomeEventCount,
			snapshot.RouteOutcomeEventCount,
			snapshot.RouteOutcomeProgressCount,
			snapshot.RouteOutcomeNegativeCount,
		),
		fmt.Sprintf(
			"failure_frontier size=%d same_failure_count=%d last_failure=%s",
			snapshot.FailureFrontierSize,
			snapshot.SameFailureFingerprintCount,
			valueOrUnknown(snapshot.LastFailureFingerprint),
		),
	}
	if len(snapshot.RecentEvents) > 0 {
		lines = append(lines, "recent_events:")
		for i, event := range snapshot.RecentEvents {
			lines = append(lines, fmt.Sprintf(
				"%d. kind=%s outcome=%s model=%s budget=%s finish_reason=%s status=%d tokens=%d cost=$%.4f latency_ms=%d",
				i+1,
				valueOrUnknown(event.Kind),
				valueOrUnknown(event.Outcome),
				valueOrUnknown(event.Model),
				valueOrUnknown(event.BudgetAction),
				valueOrUnknown(event.FinishReason),
				event.Status,
				event.TotalTokens,
				event.Cost,
				event.LatencyMs,
			))
		}
	}
	return strings.Join(lines, "\n")
}

func (s *SmartRouter) attachEpisodeMetadata(req *http.Request, decision *plugin.RoutingDecision, operation string) {
	if decision == nil || decision.Skip {
		return
	}
	if headerOperation := normalizeEpisodeOperation(req.Header.Get("X-Episode-Operation")); headerOperation != "" {
		operation = headerOperation
	}
	if operation == "" {
		operation = episodeOperationContinue
	}
	snapshot := s.episodeSnapshot(req)
	if snapshot.ID == "" {
		return
	}
	decision.EpisodeID = snapshot.ID
	decision.EpisodeOperation = operation
	decision.EpisodeStateVersion = snapshot.Version
	decision.EpisodeStateBefore = renderEpisodeStateJSON(snapshot)
}

func renderEpisodeStateJSON(snapshot EpisodeSnapshot) string {
	if snapshot.ID == "" {
		return ""
	}
	encoded, err := json.Marshal(episodeStatePayload(snapshot))
	if err != nil {
		return ""
	}
	return string(encoded)
}

func episodeStatePayload(snapshot EpisodeSnapshot) map[string]any {
	payload := map[string]any{
		"episode_id":                     snapshot.ID,
		"state_version":                  snapshot.Version,
		"call_count":                     snapshot.CallCount,
		"total_cost":                     snapshot.TotalCost,
		"total_tokens":                   snapshot.TotalTokens,
		"tool_call_count":                snapshot.ToolCallCount,
		"file_write_count":               snapshot.FileWriteCount,
		"test_run_count":                 snapshot.TestRunCount,
		"test_passed_count":              snapshot.TestPassedCount,
		"test_failed_count":              snapshot.TestFailedCount,
		"delivery_file_write_count":      snapshot.DeliveryFileWriteCount,
		"candidate_progress_count":       snapshot.CandidateProgressCount,
		"strong_progress_count":          snapshot.StrongProgressCount,
		"no_progress_event_count":        snapshot.NoProgressEventCount,
		"active_no_progress":             snapshot.ActiveNoProgress,
		"events_since_progress":          snapshot.EventsSinceProgress,
		"llm_calls_since_progress":       snapshot.LLMCallsSinceProgress,
		"length_pressure_since_progress": snapshot.LengthPressureSinceProgress,
		"last_progress_event_id":         snapshot.LastProgressEventID,
		"last_progress_kind":             snapshot.LastProgressKind,
		"verifier_reward":                snapshot.VerifierReward,
		"completion_readiness":           valueOrDefault(snapshot.CompletionReadiness, completionReadinessNone),
		"last_delivery_event_id":         snapshot.LastDeliveryEventID,
		"no_progress_severity":           valueOrDefault(snapshot.NoProgressSeverity, "none"),
		"last_model":                     snapshot.LastModel,
		"last_budget_action":             snapshot.LastBudgetAction,
		"last_finish_reason":             snapshot.LastFinishReason,
		"last_route_trace_id":            snapshot.LastRouteTraceID,
		"last_route_outcome_label":       routeOutcomeLabelForSnapshot(snapshot),
		"last_route_outcome_event_id":    snapshot.LastRouteOutcomeEventID,
		"last_route_outcome_progress":    snapshot.LastRouteOutcomeProgress,
		"last_route_outcome_event_count": snapshot.LastRouteOutcomeEventCount,
		"route_outcome_event_count":      snapshot.RouteOutcomeEventCount,
		"route_outcome_progress_count":   snapshot.RouteOutcomeProgressCount,
		"route_outcome_negative_count":   snapshot.RouteOutcomeNegativeCount,
		"last_failure_fingerprint":       snapshot.LastFailureFingerprint,
		"same_failure_fingerprint_count": snapshot.SameFailureFingerprintCount,
		"failure_frontier_size":          snapshot.FailureFrontierSize,
		"consecutive_length_finishes":    snapshot.ConsecutiveLengthFinishes,
		"recent_length_finishes":         snapshot.RecentLengthFinishes,
		"consecutive_errors":             snapshot.ConsecutiveErrors,
	}
	if len(snapshot.RecentEvents) > 0 {
		recent := make([]map[string]any, 0, len(snapshot.RecentEvents))
		for _, event := range snapshot.RecentEvents {
			recent = append(recent, map[string]any{
				"event_id":      event.ID,
				"kind":          event.Kind,
				"source":        event.Source,
				"outcome":       event.Outcome,
				"model":         event.Model,
				"budget_action": event.BudgetAction,
				"finish_reason": event.FinishReason,
				"status":        event.Status,
				"total_tokens":  event.TotalTokens,
				"cost":          event.Cost,
				"latency_ms":    event.LatencyMs,
				"evidence_refs": event.EvidenceRefs,
			})
		}
		payload["recent_events"] = recent
	}
	return payload
}

func (s *SmartRouter) QueryEpisodeStates(filter plugin.EpisodeStateFilter) ([]plugin.EpisodeStateEntry, error) {
	cfg := s.episodeConfig()
	if !cfg.Enabled {
		return nil, nil
	}
	if filter.EpisodeID != "" {
		s.ensureEpisodeStateLoaded(filter.EpisodeID, cfg)
	}

	s.episodeMu.Lock()
	defer s.episodeMu.Unlock()
	if len(s.episodes) == 0 {
		return nil, nil
	}

	if filter.EpisodeID != "" {
		state := s.episodes[filter.EpisodeID]
		if state == nil {
			return nil, nil
		}
		snapshot := snapshotFromEpisodeState(state)
		return []plugin.EpisodeStateEntry{episodeStateEntry(snapshot, s.Name())}, nil
	}

	keys := make([]string, 0, len(s.episodes))
	for key := range s.episodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	limit := filter.Limit
	if limit <= 0 || limit > len(keys) {
		limit = len(keys)
	}
	out := make([]plugin.EpisodeStateEntry, 0, limit)
	for _, key := range keys[:limit] {
		out = append(out, episodeStateEntry(snapshotFromEpisodeState(s.episodes[key]), s.Name()))
	}
	return out, nil
}

func (s *SmartRouter) ensureEpisodeStateLoaded(key string, cfg EpisodeConfig) {
	if key == "" || !cfg.Enabled {
		return
	}
	if len(s.traceQueryers) == 0 && len(s.eventQueryers) == 0 {
		return
	}

	s.episodeMu.Lock()
	if _, exists := s.episodes[key]; exists {
		s.episodeMu.Unlock()
		return
	}
	s.episodeMu.Unlock()

	s.backfillMu.Lock()
	if s.backfilled == nil {
		s.backfilled = make(map[string]struct{})
	}
	if _, alreadyTried := s.backfilled[key]; alreadyTried {
		s.backfillMu.Unlock()
		return
	}
	s.backfilled[key] = struct{}{}
	s.backfillMu.Unlock()

	events := s.loadPersistedEpisodeEvents(key)
	if len(events) == 0 {
		return
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].ID < events[j].ID
		}
		if events[i].Timestamp.IsZero() {
			return false
		}
		if events[j].Timestamp.IsZero() {
			return true
		}
		return events[i].Timestamp.Before(events[j].Timestamp)
	})

	s.episodeMu.Lock()
	defer s.episodeMu.Unlock()
	state := s.episodes[key]
	if state == nil {
		state = &EpisodeState{ID: key}
		s.episodes[key] = state
	}
	for _, event := range events {
		projectUniqueEpisodeEvent(state, event, cfg)
	}
}

func (s *SmartRouter) loadPersistedEpisodeEvents(key string) []EpisodeEvent {
	var events []EpisodeEvent
	seenTraceIDs := map[string]struct{}{}
	traceFilters := []plugin.TraceFilter{
		{EpisodeID: key, Limit: 1000},
		{SessionID: key, Limit: 1000},
		{TrialName: key, Limit: 1000},
	}
	for _, queryer := range s.traceQueryers {
		for _, filter := range traceFilters {
			traces, err := queryer.QueryTraces(filter)
			if err != nil {
				continue
			}
			for _, trace := range traces {
				if trace.TraceID == "" {
					continue
				}
				if _, exists := seenTraceIDs[trace.TraceID]; exists {
					continue
				}
				seenTraceIDs[trace.TraceID] = struct{}{}
				if trace.Pool == "decision-model" || strings.HasPrefix(trace.StepName, "router-decision") {
					continue
				}
				events = append(events, episodeEventFromTraceEntry(trace))
			}
		}
	}

	seenEventIDs := map[string]struct{}{}
	for _, queryer := range s.eventQueryers {
		explicitEvents, err := queryer.QueryEpisodeEvents(plugin.EpisodeEventFilter{EpisodeID: key, Limit: 1000})
		if err != nil {
			continue
		}
		for _, event := range explicitEvents {
			if event.EventID != "" {
				if _, exists := seenEventIDs[event.EventID]; exists {
					continue
				}
				seenEventIDs[event.EventID] = struct{}{}
			}
			events = append(events, episodeEventFromPluginEvent(&event))
		}
	}
	return events
}

func episodeStateEntry(snapshot EpisodeSnapshot, source string) plugin.EpisodeStateEntry {
	return plugin.EpisodeStateEntry{
		EpisodeID:    snapshot.ID,
		StateVersion: snapshot.Version,
		Source:       source,
		State:        episodeStatePayload(snapshot),
	}
}

func episodeKeyFromRequest(req *http.Request) string {
	if req == nil {
		return ""
	}
	if key := strings.TrimSpace(req.Header.Get("X-Episode-ID")); key != "" {
		return key
	}
	if key := strings.TrimSpace(req.Header.Get("X-Session-ID")); key != "" {
		return key
	}
	return strings.TrimSpace(req.Header.Get("X-Trial-Name"))
}

func episodeSessionKeyFromRequest(req *http.Request) string {
	if req == nil {
		return ""
	}
	if key := strings.TrimSpace(req.Header.Get("X-Session-ID")); key != "" {
		return key
	}
	if key := strings.TrimSpace(req.Header.Get("X-Trial-Name")); key != "" {
		return key
	}
	return strings.TrimSpace(req.Header.Get("X-Episode-ID"))
}

func episodeKeyFromRecord(record *plugin.AuditRecord) string {
	if record.EpisodeID != "" {
		return record.EpisodeID
	}
	if record.SessionID != "" {
		return record.SessionID
	}
	return record.TrialName
}

func appendUniqueEpisode(stack []string, episodeID string) []string {
	if episodeID == "" {
		return stack
	}
	for _, existing := range stack {
		if existing == episodeID {
			return stack
		}
	}
	return append(stack, episodeID)
}

func trimStackToEpisode(stack []string, episodeID string) []string {
	if episodeID == "" {
		return stack
	}
	for index := len(stack) - 1; index >= 0; index-- {
		if stack[index] == episodeID {
			return stack[:index+1]
		}
	}
	return append(stack, episodeID)
}

func isDecisionModelRecord(record *plugin.AuditRecord) bool {
	return record.Pool == "decision-model" || strings.HasPrefix(record.StepName, "router-decision")
}

func episodeOutcome(record *plugin.AuditRecord) string {
	if record.Status >= 400 || record.ErrorKind != "" {
		return "error"
	}
	if strings.EqualFold(record.FinishReason, "length") {
		return "length_truncated"
	}
	if strings.EqualFold(record.FinishReason, "stop") {
		return "response_completed"
	}
	if record.Status >= 200 && record.Status < 300 && record.FinishReason == "" && record.TotalTokens == 0 {
		return "provider_incomplete"
	}
	return "unknown"
}

func countLengthFinishes(events []EpisodeEvent) int {
	count := 0
	for _, event := range events {
		if event.FinishReason == "length" || event.Outcome == "length_truncated" {
			count++
		}
	}
	return count
}

func countRecentEpisodeErrors(events []EpisodeEvent) int {
	count := 0
	for _, event := range events {
		if event.Status >= 400 || event.Outcome == "error" || event.Kind == "run_exception" {
			count++
		}
	}
	return count
}

func countRecentProgress(events []EpisodeEvent) int {
	count := 0
	for _, event := range events {
		if isStrongProgressEvent(event) || isCandidateProgressEvent(event) {
			count++
		}
	}
	return count
}

func noProgressSeverity(state *EpisodeState, cfg EpisodeConfig) string {
	if state == nil {
		return "none"
	}
	recentPressure := state.RecentLengthFinishes + countRecentEpisodeErrors(state.RecentEvents)
	recentProgress := countRecentProgress(state.RecentEvents)
	if state.LLMCallsSinceProgress >= defaultEpisodeNoProgressAgentCallThreshold {
		return "blocked"
	}
	if state.ActiveNoProgress {
		return "stale"
	}
	if recentPressure >= cfg.LengthWindowThreshold && recentProgress == 0 {
		return "stale"
	}
	if recentPressure > 0 {
		return "watch"
	}
	return "none"
}

func isStrongProgressEvent(event EpisodeEvent) bool {
	switch event.Kind {
	case "test_passed":
		return intFromObservation(event.Observation, "failed_count") == 0
	case "verifier_result":
		return floatFromObservation(event.Observation, "reward") > 0
	default:
		return false
	}
}

func clearsFailureFrontier(event EpisodeEvent) bool {
	switch event.Kind {
	case "test_passed":
		return intFromObservation(event.Observation, "failed_count") == 0
	case "test_run":
		return strings.EqualFold(stringFromObservation(event.Observation, "outcome"), "passed")
	case "verifier_result":
		return floatFromObservation(event.Observation, "reward") > 0
	default:
		return false
	}
}

func isCandidateProgressEvent(event EpisodeEvent) bool {
	switch event.Kind {
	case "file_modified":
		return intFromObservation(event.Observation, "path_count") > 0
	case "file_written":
		return boolFromObservation(event.Observation, "delivery_target") || boolFromObservation(event.Observation, "workspace_target")
	case "test_run":
		return strings.EqualFold(stringFromObservation(event.Observation, "outcome"), "passed")
	default:
		return false
	}
}

func copyObservation(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func stringFromObservation(observation map[string]any, key string) string {
	if observation == nil {
		return ""
	}
	switch value := observation[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		if value == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func stringListFromObservation(observation map[string]any, key string) []string {
	if observation == nil {
		return nil
	}
	switch values := observation[key].(type) {
	case []string:
		out := make([]string, 0, len(values))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value != "" {
				out = append(out, value)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			text := strings.TrimSpace(fmt.Sprint(value))
			if text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		value := strings.TrimSpace(values)
		if value == "" {
			return nil
		}
		return []string{value}
	default:
		return nil
	}
}

func intFromObservation(observation map[string]any, key string) int {
	if observation == nil {
		return 0
	}
	switch value := observation[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		out, _ := value.Int64()
		return int(out)
	case string:
		var out int
		if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &out); err == nil {
			return out
		}
	}
	return 0
}

func floatFromObservation(observation map[string]any, key string) float64 {
	if observation == nil {
		return 0
	}
	switch value := observation[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		out, _ := value.Float64()
		return out
	case string:
		var out float64
		if _, err := fmt.Sscanf(strings.TrimSpace(value), "%f", &out); err == nil {
			return out
		}
	}
	return 0
}

func boolFromObservation(observation map[string]any, key string) bool {
	if observation == nil {
		return false
	}
	switch value := observation[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func joinRouterContext(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return strings.Join(nonEmpty, "\n")
}
