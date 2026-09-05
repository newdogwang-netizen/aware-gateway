package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aware/gateway/internal/pool"
)

// NewReverseProxy creates a proxy that forwards to the selected endpoint.
// FlushInterval=-1 ensures SSE streaming (LLM) is chunk-flushed immediately.
func NewReverseProxy(ep *pool.Endpoint) *httputil.ReverseProxy {
	baseTransport := http.DefaultTransport
	if ep.URL.Scheme == "https" && ep.TLSSkipVerify {
		baseTransport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec
			},
		}
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = ep.URL.Scheme
			req.URL.Host = ep.URL.Host
			req.URL.Path = ep.URL.Path + req.URL.Path
			req.Host = ep.URL.Host

			// Strip internal routing headers before forwarding to upstream
			req.Header.Del("X-Backend-Endpoint")
			req.Header.Del("X-Retry-Attempt")
			req.Header.Del("X-Fallback")
			req.Header.Del("X-Routed-Model")
			req.Header.Del("X-Routing-Reason")
			// Strip task correlation headers — these are gateway-internal,
			// not for upstream consumption (OpenRouter/OpenAI would 400 on unknown headers)
			req.Header.Del("X-Session-ID")
			req.Header.Del("X-Trial-Name")
			req.Header.Del("X-Step-Name")
			req.Header.Del("X-Task-Name")

			// Keep upstream JSON responses parseable for usage/cost accounting.
			// Some clients send Accept-Encoding:gzip; if we pass that through,
			// ModifyResponse sees compressed bytes and cannot extract usage.
			req.Header.Set("Accept-Encoding", "identity")

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
			return parseAndLogTokens(ep.Name, resp)
		},
	}

	if ep.Timeout > 0 {
		rp.Transport = &timeoutTransport{
			base:    baseTransport,
			timeout: ep.Timeout,
		}
	} else if baseTransport != http.DefaultTransport {
		rp.Transport = baseTransport
	}

	return rp
}

type timeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t *timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.Body == nil {
		cancel()
		return resp, nil
	}
	resp.Body = newTimeoutReadCloser(resp.Body, cancel, t.timeout)
	return resp, nil
}

type timeoutReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	timer  *time.Timer
	once   sync.Once
}

func newTimeoutReadCloser(body io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) *timeoutReadCloser {
	rc := &timeoutReadCloser{ReadCloser: body, cancel: cancel}
	rc.timer = time.AfterFunc(timeout, func() {
		rc.once.Do(func() {
			rc.cancel()
			_ = rc.ReadCloser.Close()
		})
	})
	return rc
}

func (r *timeoutReadCloser) Close() error {
	var err error
	r.once.Do(func() {
		if r.timer != nil {
			r.timer.Stop()
		}
		err = r.ReadCloser.Close()
		r.cancel()
	})
	return err
}

const maxTokenParseBytes = 1 << 20

// parseAndLogTokens extracts token usage from non-streaming LLM responses.
// Token data is bridged via X-Gw-* response headers for the audit layer.
func parseAndLogTokens(endpoint string, resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.HasPrefix(ct, "text/event-stream") {
		return nil
	}
	if resp.Body == nil || resp.StatusCode != 200 {
		return nil
	}

	limited := io.LimitReader(resp.Body, maxTokenParseBytes+1)
	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		resp.Body.Close()
		slog.Warn("failed to read upstream response body",
			"endpoint", endpoint,
			"error", err,
		)
		return err
	}

	oversize := len(bodyBytes) > maxTokenParseBytes
	if oversize {
		rest, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyBytes = append(bodyBytes[:maxTokenParseBytes], rest...)
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return nil
	}

	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	var result struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			TotalTokens      int     `json:"total_tokens"`
			Cost             float64 `json:"cost"`
			CostDetails      struct {
				UpstreamInferenceCost float64 `json:"upstream_inference_cost"`
			} `json:"cost_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(bodyBytes, &result) != nil {
		return nil
	}
	if result.Usage.TotalTokens > 0 {
		resp.Header.Set("X-Gw-Prompt-Tokens", strconv.Itoa(result.Usage.PromptTokens))
		resp.Header.Set("X-Gw-Completion-Tokens", strconv.Itoa(result.Usage.CompletionTokens))
		resp.Header.Set("X-Gw-Total-Tokens", strconv.Itoa(result.Usage.TotalTokens))
	}
	cost := result.Usage.Cost
	if result.Usage.CostDetails.UpstreamInferenceCost > 0 {
		cost = result.Usage.CostDetails.UpstreamInferenceCost
	}
	if cost > 0 {
		resp.Header.Set("X-Gw-Cost", strconv.FormatFloat(cost, 'g', -1, 64))
	}
	if len(result.Choices) > 0 && result.Choices[0].FinishReason != "" {
		resp.Header.Set("X-Gw-Finish-Reason", result.Choices[0].FinishReason)
	}
	return nil
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
