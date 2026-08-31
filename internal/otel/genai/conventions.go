// Package genai defines OpenTelemetry semantic conventions for Generative AI
// operations, compatible with both the OTel GenAI spec (gen_ai.*) and the
// OpenInference spec (llm.*) used by Arize Phoenix and Langfuse.
//
// The gateway uses these conventions to enrich trace spans with structured
// LLM metadata — model, tokens, operation type, finish reason — so that
// observability backends (Jaeger, Tempo, Grafana, Phoenix, Langfuse) can
// render AI-specific views without custom parsing.
//
// References:
//   - OTel GenAI: https://opentelemetry.io/docs/specs/semconv/gen-ai/
//   - OpenInference: https://github.com/Arize-ai/openinference
package genai

// Convention selects which semantic convention namespace to emit.
type Convention string

const (
	// ConvOTelGenAI emits gen_ai.* attributes (OTel standard, recommended).
	ConvOTelGenAI Convention = "gen_ai"
	// ConvOpenInference emits llm.* attributes (Arize/Langfuse compatible).
	ConvOpenInference Convention = "openinference"
	// ConvBoth emits both gen_ai.* and llm.* attributes.
	ConvBoth Convention = "both"
)

// --- OTel GenAI Semantic Conventions (gen_ai.*) ---

const (
	// System identifies the AI system/provider.
	// Values: "openai", "anthropic", "azure_openai", "gcp_gemini",
	// "aws_bedrock", "cohere", "mistralai", "ollama", "vllm", "sglang", etc.
	AttrGenAISystem = "gen_ai.system"

	// Operation is the high-level operation type.
	// Values: "chat", "generate_text", "embeddings", "generate_image",
	// "transcribe", "translate".
	AttrGenAIOperation = "gen_ai.operation"

	// RequestModel is the model name requested by the client.
	AttrGenAIRequestModel = "gen_ai.request.model"

	// RequestMaxTokens is the maximum tokens to generate.
	AttrGenAIRequestMaxTokens = "gen_ai.request.max_tokens"

	// RequestTemperature is the sampling temperature.
	AttrGenAIRequestTemperature = "gen_ai.request.temperature"

	// RequestTopP is the nucleus sampling probability.
	AttrGenAIRequestTopP = "gen_ai.request.top_p"

	// RequestFrequencyPenalty.
	AttrGenAIRequestFreqPenalty = "gen_ai.request.frequency_penalty"

	// RequestPresencePenalty.
	AttrGenAIRequestPresPenalty = "gen_ai.request.presence_penalty"

	// RequestStopSequences is the stop sequence array.
	AttrGenAIRequestStopSequences = "gen_ai.request.stop_sequences"

	// RequestStream indicates streaming mode.
	AttrGenAIRequestStream = "gen_ai.request.stream"

	// ResponseModel is the actual model that served the request (may differ
	// from request.model after routing/aliasing).
	AttrGenAIResponseModel = "gen_ai.response.model"

	// ResponseID is the response/completion ID from the provider.
	AttrGenAIResponseID = "gen_ai.response.id"

	// ResponseFinishReasons is an array of finish reasons (e.g. ["stop"]).
	AttrGenAIResponseFinishReasons = "gen_ai.response.finish_reasons"

	// ResponseSystemFingerprint (OpenAI-specific).
	AttrGenAIResponseSystemFingerprint = "gen_ai.openai.response.system_fingerprint"

	// UsageInputTokens is the prompt token count.
	AttrGenAIUsageInputTokens = "gen_ai.usage.input_tokens"

	// UsageOutputTokens is the completion token count.
	AttrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"
)

// --- OpenInference Semantic Conventions (llm.*) ---

const (
	AttrLLMVendor          = "llm.vendor"
	AttrLLMModelName       = "llm.model_name"
	AttrLLMInvocationParams = "llm.invocation_parameters"
	AttrLLMInputMessages   = "llm.input_messages"
	AttrLLMOutputMessages  = "llm.output_messages"
	AttrLLMTokenCountPrompt    = "llm.token_count.prompt"
	AttrLLMTokenCountCompletion = "llm.token_count.completion"
	AttrLLMTokenCountTotal     = "llm.token_count.total"
	AttrLLMSessionID       = "llm.session_id"
)

// --- Gateway-specific span attributes (custom, not part of either spec) ---
// These are added alongside the standard conventions to provide gateway-
// specific context that the standard specs don't cover.

const (
	AttrGwPool          = "gateway.pool"
	AttrGwEndpoint      = "gateway.endpoint"
	AttrGwAttempt       = "gateway.attempt"
	AttrGwFallback      = "gateway.fallback"
	AttrGwRoutedModel   = "gateway.routed_model"
	AttrGwRoutingReason = "gateway.routing_reason"
	AttrGwTTFT          = "gateway.ttft_seconds"
)
