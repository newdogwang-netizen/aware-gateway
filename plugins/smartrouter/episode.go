package smartrouter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aware/gateway/internal/plugin"
)

const (
	defaultEpisodeRecentEvents          = 5
	defaultEpisodeLengthStreakThreshold = 1
	defaultEpisodeLengthWindowThreshold = 2
	defaultEpisodeMaxTokensMultiplier   = 3.0
	defaultEpisodeTimeoutMultiplier     = 2.0
	defaultEpisodeMaxTokensCeiling      = 8192
	defaultEpisodeTimeoutMsCeiling      = 240000

	episodeOperationContinue  = "continue"
	episodeOperationInterrupt = "interrupt"
	episodeOperationResume    = "resume"
	episodeOperationGlobal    = "global"
	episodeOperationUnknown   = "unknown"
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
	Kind         string
	Outcome      string
	Model        string
	BudgetAction string
	FinishReason string
	Status       int
	LatencyMs    int64
	Cost         float64
	TotalTokens  int
	Timestamp    time.Time
}

type EpisodeState struct {
	ID                        string
	Version                   int
	CallCount                 int
	TotalCost                 float64
	TotalTokens               int
	LastModel                 string
	LastBudgetAction          string
	LastFinishReason          string
	ConsecutiveLengthFinishes int
	RecentLengthFinishes      int
	ConsecutiveErrors         int
	RecentEvents              []EpisodeEvent
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
	ID                        string
	Version                   int
	CallCount                 int
	TotalCost                 float64
	TotalTokens               int
	LastModel                 string
	LastBudgetAction          string
	LastFinishReason          string
	ConsecutiveLengthFinishes int
	RecentLengthFinishes      int
	ConsecutiveErrors         int
	RecentEvents              []EpisodeEvent
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
		Kind:         "llm_call",
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
	projectEpisodeEvent(state, event, cfg)
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

func projectEpisodeEvent(state *EpisodeState, event EpisodeEvent, cfg EpisodeConfig) {
	state.Version++
	state.CallCount++
	state.TotalCost += event.Cost
	state.TotalTokens += event.TotalTokens
	state.LastModel = event.Model
	state.LastBudgetAction = event.BudgetAction
	state.LastFinishReason = event.FinishReason

	if event.FinishReason == "length" {
		state.ConsecutiveLengthFinishes++
	} else if event.FinishReason != "" || event.Status < 400 {
		state.ConsecutiveLengthFinishes = 0
	}

	if event.Status >= 400 {
		state.ConsecutiveErrors++
	} else {
		state.ConsecutiveErrors = 0
	}

	state.RecentEvents = append(state.RecentEvents, event)
	if len(state.RecentEvents) > cfg.RecentEvents {
		state.RecentEvents = state.RecentEvents[len(state.RecentEvents)-cfg.RecentEvents:]
	}
	state.RecentLengthFinishes = countLengthFinishes(state.RecentEvents)
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
		ID:                        state.ID,
		Version:                   state.Version,
		CallCount:                 state.CallCount,
		TotalCost:                 state.TotalCost,
		TotalTokens:               state.TotalTokens,
		LastModel:                 state.LastModel,
		LastBudgetAction:          state.LastBudgetAction,
		LastFinishReason:          state.LastFinishReason,
		ConsecutiveLengthFinishes: state.ConsecutiveLengthFinishes,
		RecentLengthFinishes:      state.RecentLengthFinishes,
		ConsecutiveErrors:         state.ConsecutiveErrors,
		RecentEvents:              recent,
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
	payload := map[string]any{
		"episode_id":                  snapshot.ID,
		"state_version":               snapshot.Version,
		"call_count":                  snapshot.CallCount,
		"total_cost":                  snapshot.TotalCost,
		"total_tokens":                snapshot.TotalTokens,
		"last_model":                  snapshot.LastModel,
		"last_budget_action":          snapshot.LastBudgetAction,
		"last_finish_reason":          snapshot.LastFinishReason,
		"consecutive_length_finishes": snapshot.ConsecutiveLengthFinishes,
		"recent_length_finishes":      snapshot.RecentLengthFinishes,
		"consecutive_errors":          snapshot.ConsecutiveErrors,
	}
	if len(snapshot.RecentEvents) > 0 {
		recent := make([]map[string]any, 0, len(snapshot.RecentEvents))
		for _, event := range snapshot.RecentEvents {
			recent = append(recent, map[string]any{
				"kind":          event.Kind,
				"outcome":       event.Outcome,
				"model":         event.Model,
				"budget_action": event.BudgetAction,
				"finish_reason": event.FinishReason,
				"status":        event.Status,
				"total_tokens":  event.TotalTokens,
				"cost":          event.Cost,
				"latency_ms":    event.LatencyMs,
			})
		}
		payload["recent_events"] = recent
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
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
		if event.FinishReason == "length" {
			count++
		}
	}
	return count
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
