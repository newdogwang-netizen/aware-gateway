package genai

import (
	"encoding/json"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// RequestAttrs holds GenAI attributes extracted from an LLM request body.
type RequestAttrs struct {
	System          string  // detected provider system
	Operation       string  // "chat", "completion", "embeddings"
	Model           string  // requested model
	MaxTokens       int     // max_tokens
	Temperature     float64 // temperature (0 = unset)
	TopP            float64 // top_p (0 = unset)
	FrequencyPenalty float64
	PresencePenalty  float64
	StopSequences   []string
	Stream          bool
}

// ResponseAttrs holds GenAI attributes extracted from the upstream response.
type ResponseAttrs struct {
	Model            string // actual model (from response body or header)
	ResponseID       string
	FinishReasons    []string
	SystemFingerprint string
	InputTokens      int
	OutputTokens     int
}

// ParseRequestAttrs extracts GenAI request attributes from a JSON body.
// The body must be the OpenAI-compatible request JSON.
func ParseRequestAttrs(body []byte) *RequestAttrs {
	if len(body) == 0 {
		return nil
	}
	var req struct {
		Model          string   `json:"model"`
		MaxTokens      int      `json:"max_tokens"`
		Temperature    *float64 `json:"temperature"`
		TopP           *float64 `json:"top_p"`
		FrequencyPenalty *float64 `json:"frequency_penalty"`
		PresencePenalty  *float64 `json:"presence_penalty"`
		Stop           []string `json:"stop"`
		Stream         bool     `json:"stream"`
		Messages       []any    `json:"messages"`
		Input          any      `json:"input"` // embeddings
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	a := &RequestAttrs{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		StopSequences: req.Stop,
		Stream:        req.Stream,
	}
	if req.Temperature != nil {
		a.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		a.TopP = *req.TopP
	}
	if req.FrequencyPenalty != nil {
		a.FrequencyPenalty = *req.FrequencyPenalty
	}
	if req.PresencePenalty != nil {
		a.PresencePenalty = *req.PresencePenalty
	}

	// Determine operation type
	if req.Input != nil && len(req.Messages) == 0 {
		a.Operation = "embeddings"
	} else {
		a.Operation = "chat"
	}

	// Infer system from model name
	a.System = InferSystem(req.Model)

	return a
}

// ParseResponseAttrs extracts GenAI response attributes from a JSON body.
// Used for non-streaming responses. For streaming, use ParseSSETail.
func ParseResponseAttrs(body []byte) *ResponseAttrs {
	if len(body) == 0 {
		return nil
	}
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		SystemFingerprint string `json:"system_fingerprint"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}

	a := &ResponseAttrs{
		ResponseID:        resp.ID,
		Model:             resp.Model,
		SystemFingerprint: resp.SystemFingerprint,
	}
	for _, c := range resp.Choices {
		if c.FinishReason != "" {
			a.FinishReasons = append(a.FinishReasons, c.FinishReason)
		}
	}
	if resp.Usage != nil {
		a.InputTokens = resp.Usage.PromptTokens
		a.OutputTokens = resp.Usage.CompletionTokens
	}
	return a
}

// InferSystem guesses the AI system from a model name prefix.
func InferSystem(model string) string {
	if model == "" {
		return "unknown"
	}
	lower := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lower, "gpt") || strings.HasPrefix(lower, "openai/"):
		return "openai"
	case strings.HasPrefix(lower, "claude") || strings.HasPrefix(lower, "anthropic/"):
		return "anthropic"
	case strings.HasPrefix(lower, "gemini") || strings.HasPrefix(lower, "google/"):
		return "gcp_gemini"
	case strings.HasPrefix(lower, "command") || strings.HasPrefix(lower, "cohere/"):
		return "cohere"
	case strings.HasPrefix(lower, "mistral") || strings.HasPrefix(lower, "mixtral"):
		return "mistralai"
	case strings.HasPrefix(lower, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(lower, "qwen") || strings.HasPrefix(lower, "gemma"):
		return "vllm" // typical on-prem
	case strings.HasPrefix(lower, "llama") || strings.HasPrefix(lower, "meta/"):
		return "meta"
	case strings.HasPrefix(lower, "whisper"):
		return "openai" // whisper is an OpenAI model
	case strings.HasPrefix(lower, "accounts/fireworks"):
		return "fireworks"
	default:
		return "unknown"
	}
}

// --- Span Attribute Builders ---

// RequestSpanAttrs builds OTel attributes for request-side GenAI metadata.
func RequestSpanAttrs(a *RequestAttrs, conv Convention) []attribute.KeyValue {
	var attrs []attribute.KeyValue

	switch conv {
	case ConvOTelGenAI:
		attrs = append(attrs, attribute.String(AttrGenAISystem, a.System))
		attrs = append(attrs, attribute.String(AttrGenAIOperation, a.Operation))
		if a.Model != "" {
			attrs = append(attrs, attribute.String(AttrGenAIRequestModel, a.Model))
		}
		if a.MaxTokens > 0 {
			attrs = append(attrs, attribute.Int(AttrGenAIRequestMaxTokens, a.MaxTokens))
		}
		if a.Temperature > 0 {
			attrs = append(attrs, attribute.Float64(AttrGenAIRequestTemperature, a.Temperature))
		}
		if a.TopP > 0 {
			attrs = append(attrs, attribute.Float64(AttrGenAIRequestTopP, a.TopP))
		}
		if a.FrequencyPenalty != 0 {
			attrs = append(attrs, attribute.Float64(AttrGenAIRequestFreqPenalty, a.FrequencyPenalty))
		}
		if a.PresencePenalty != 0 {
			attrs = append(attrs, attribute.Float64(AttrGenAIRequestPresPenalty, a.PresencePenalty))
		}
		if len(a.StopSequences) > 0 {
			attrs = append(attrs, attribute.StringSlice(AttrGenAIRequestStopSequences, a.StopSequences))
		}
		attrs = append(attrs, attribute.Bool(AttrGenAIRequestStream, a.Stream))

	case ConvOpenInference:
		attrs = append(attrs, attribute.String(AttrLLMVendor, a.System))
		attrs = append(attrs, attribute.String(AttrLLMModelName, a.Model))
		// invocation_parameters as JSON
		params := map[string]any{}
		if a.MaxTokens > 0 {
			params["max_tokens"] = a.MaxTokens
		}
		if a.Temperature > 0 {
			params["temperature"] = a.Temperature
		}
		if a.TopP > 0 {
			params["top_p"] = a.TopP
		}
		if len(a.StopSequences) > 0 {
			params["stop"] = a.StopSequences
		}
		if data, err := json.Marshal(params); err == nil && len(data) > 2 {
			attrs = append(attrs, attribute.String(AttrLLMInvocationParams, string(data)))
		}

	case ConvBoth:
		attrs = append(attrs, RequestSpanAttrs(a, ConvOTelGenAI)...)
		attrs = append(attrs, RequestSpanAttrs(a, ConvOpenInference)...)
	}

	return attrs
}

// ResponseSpanAttrs builds OTel attributes for response-side GenAI metadata.
func ResponseSpanAttrs(a *ResponseAttrs, conv Convention) []attribute.KeyValue {
	var attrs []attribute.KeyValue

	switch conv {
	case ConvOTelGenAI:
		if a.Model != "" {
			attrs = append(attrs, attribute.String(AttrGenAIResponseModel, a.Model))
		}
		if a.ResponseID != "" {
			attrs = append(attrs, attribute.String(AttrGenAIResponseID, a.ResponseID))
		}
		if len(a.FinishReasons) > 0 {
			attrs = append(attrs, attribute.StringSlice(AttrGenAIResponseFinishReasons, a.FinishReasons))
		}
		if a.SystemFingerprint != "" {
			attrs = append(attrs, attribute.String(AttrGenAIResponseSystemFingerprint, a.SystemFingerprint))
		}
		if a.InputTokens > 0 {
			attrs = append(attrs, attribute.Int(AttrGenAIUsageInputTokens, a.InputTokens))
		}
		if a.OutputTokens > 0 {
			attrs = append(attrs, attribute.Int(AttrGenAIUsageOutputTokens, a.OutputTokens))
		}

	case ConvOpenInference:
		if a.Model != "" {
			attrs = append(attrs, attribute.String(AttrLLMModelName, a.Model))
		}
		if a.InputTokens > 0 {
			attrs = append(attrs, attribute.Int(AttrLLMTokenCountPrompt, a.InputTokens))
		}
		if a.OutputTokens > 0 {
			attrs = append(attrs, attribute.Int(AttrLLMTokenCountCompletion, a.OutputTokens))
		}
		total := a.InputTokens + a.OutputTokens
		if total > 0 {
			attrs = append(attrs, attribute.Int(AttrLLMTokenCountTotal, total))
		}

	case ConvBoth:
		attrs = append(attrs, ResponseSpanAttrs(a, ConvOTelGenAI)...)
		attrs = append(attrs, ResponseSpanAttrs(a, ConvOpenInference)...)
	}

	return attrs
}

// GatewaySpanAttrs builds gateway-specific span attributes.
func GatewaySpanAttrs(pool, endpoint, routedModel, routingReason string, attempt int, fallback bool) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrGwPool, pool),
		attribute.String(AttrGwEndpoint, endpoint),
		attribute.Int(AttrGwAttempt, attempt),
		attribute.Bool(AttrGwFallback, fallback),
	}
	if routedModel != "" {
		attrs = append(attrs, attribute.String(AttrGwRoutedModel, routedModel))
	}
	if routingReason != "" {
		attrs = append(attrs, attribute.String(AttrGwRoutingReason, routingReason))
	}
	return attrs
}
