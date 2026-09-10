package smartrouter

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/aware/gateway/internal/plugin"
)

const (
	defaultRepeatedErrorThreshold = 2
	defaultPremiumCooldownAfter   = 2
	defaultPremiumCooldownTurns   = 1
	defaultCheapProbeBurstLimit   = 3
)

// SafeControlConfig enables a local high-confidence routing layer before the
// semantic decision model. It is intentionally conservative: unmatched requests
// continue through the existing prompt-based smart-router.
type SafeControlConfig struct {
	Enabled                bool `yaml:"enabled" json:"enabled"`
	RepeatedErrorThreshold int  `yaml:"repeated_error_threshold" json:"repeated_error_threshold"`
	PremiumCooldownAfter   int  `yaml:"premium_cooldown_after" json:"premium_cooldown_after"`
	PremiumCooldownTurns   int  `yaml:"premium_cooldown_turns" json:"premium_cooldown_turns"`
	CheapProbeBurstLimit   int  `yaml:"cheap_probe_burst_limit" json:"cheap_probe_burst_limit"`
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

	return nil, nil, false
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
