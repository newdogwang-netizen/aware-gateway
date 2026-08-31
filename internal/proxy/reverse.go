package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/aware/gateway/internal/pool"
)

// NewReverseProxy creates a proxy that forwards to the selected endpoint.
// FlushInterval=-1 ensures SSE streaming (LLM) is chunk-flushed immediately.
func NewReverseProxy(ep *pool.Endpoint) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = ep.URL.Scheme
			req.URL.Host = ep.URL.Host
			req.URL.Path = ep.URL.Path + req.URL.Path
			req.Host = ep.URL.Host

			// Strip internal headers before forwarding to upstream
			req.Header.Del("X-Backend-Endpoint")
			req.Header.Del("X-Retry-Attempt")
			req.Header.Del("X-Fallback")
			req.Header.Del("X-Routed-Model")
			req.Header.Del("X-Routing-Reason")

			// Inject gateway-managed auth token when configured
			if ep.AuthToken != "" {
				req.Header.Set("Authorization", "Bearer "+ep.AuthToken)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("proxy error",
				"endpoint", ep.Name,
				"path", r.URL.Path,
				"error", err,
			)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			slog.Debug("upstream response",
				"endpoint", ep.Name,
				"status", resp.StatusCode,
			)
			parseAndLogTokens(ep.Name, resp)
			return nil
		},
	}

	if ep.URL.Scheme == "https" && ep.TLSSkipVerify {
		rp.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec
			},
		}
	}

	return rp
}

const maxTokenParseBytes = 1 << 20

// parseAndLogTokens extracts token usage from non-streaming LLM responses.
// Token data is bridged via X-Gw-* response headers for the audit layer.
func parseAndLogTokens(endpoint string, resp *http.Response) {
	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.HasPrefix(ct, "text/event-stream") {
		return
	}
	if resp.Body == nil || resp.StatusCode != 200 {
		return
	}

	limited := io.LimitReader(resp.Body, maxTokenParseBytes+1)
	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return
	}

	oversize := len(bodyBytes) > maxTokenParseBytes
	if oversize {
		rest, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyBytes = append(bodyBytes[:maxTokenParseBytes], rest...)
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return
	}

	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	var result struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(bodyBytes, &result) != nil {
		return
	}
	if result.Usage.TotalTokens > 0 {
		resp.Header.Set("X-Gw-Prompt-Tokens", strconv.Itoa(result.Usage.PromptTokens))
		resp.Header.Set("X-Gw-Completion-Tokens", strconv.Itoa(result.Usage.CompletionTokens))
		resp.Header.Set("X-Gw-Total-Tokens", strconv.Itoa(result.Usage.TotalTokens))
	}
	if len(result.Choices) > 0 && result.Choices[0].FinishReason != "" {
		resp.Header.Set("X-Gw-Finish-Reason", result.Choices[0].FinishReason)
	}
}

// TrackLatency wraps a handler to log request latency.
func TrackLatency(name string, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request completed",
			"pool", name,
			"method", r.Method,
			"path", r.URL.Path,
			"latency_ms", time.Since(start).Milliseconds(),
		)
	}
}
