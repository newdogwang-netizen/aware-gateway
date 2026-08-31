package core

import (
	"net/http"
	"strings"
	"time"
)

// decisionWriter wraps the real ResponseWriter and decides at WriteHeader
// time whether to buffer (for retry) or stream directly (for success).
//
// - Error status (5xx, 429) → buffer mode: captures body, no write to client
// - Success status (2xx, 3xx) → direct mode: headers + status sent immediately
// - Other (4xx not 429) → buffer mode
type decisionWriter struct {
	real       http.ResponseWriter
	retryable  map[int]bool
	code       int
	header     http.Header
	body       []byte
	discard    bool
	directMode bool
	done       bool

	// Token accounting (intercepted from X-Gw-* headers)
	promptTokens int
	compTokens   int
	totalTokens  int
	hasTokens    bool
	finishReason string

	// Streaming support
	streaming   bool
	tail        []byte
	firstWriteAt time.Time
}

func (w *decisionWriter) Header() http.Header {
	return w.header
}

func (w *decisionWriter) WriteHeader(code int) {
	if w.done || w.code != 0 {
		return
	}
	if code == http.StatusContinue {
		return
	}
	w.code = code

	if code >= 200 && code < 400 {
		for _, h := range []string{"X-Prompt-Tokens", "X-Completion-Tokens", "X-Total-Tokens"} {
			w.header.Del(h)
		}
		w.interceptTokenHeaders()
		for k, vv := range w.header {
			for _, v := range vv {
				w.real.Header().Add(k, v)
			}
		}
		w.real.WriteHeader(code)
		w.directMode = true
		w.done = true
	}
}

func (w *decisionWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(200)
	}

	if w.directMode {
		return w.real.Write(p)
	}

	if w.discard {
		return len(p), nil
	}
	if len(w.body)+len(p) > maxRetryBufSize {
		w.discard = true
		return len(p), nil
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *decisionWriter) IsBuffered() bool {
	return !w.directMode
}

func (w *decisionWriter) commit() {
	if w.directMode {
		return
	}

	if w.code == 0 {
		w.code = http.StatusBadGateway
	}

	for _, h := range []string{"X-Prompt-Tokens", "X-Completion-Tokens", "X-Total-Tokens"} {
		w.header.Del(h)
	}

	for k, vv := range w.header {
		for _, v := range vv {
			w.real.Header().Add(k, v)
		}
	}
	w.real.WriteHeader(w.code)
	if len(w.body) > 0 {
		w.real.Write(w.body)
	}
	w.done = true
}

func (w *decisionWriter) Flush() {
	if w.directMode {
		if f, ok := w.real.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (w *decisionWriter) reset() {
	if w.done || w.directMode {
		return
	}
	w.code = 0
	w.body = w.body[:0]
	w.discard = false
}

func (w *decisionWriter) interceptTokenHeaders() {
	if v := w.header.Get("X-Gw-Prompt-Tokens"); v != "" {
		w.promptTokens, _ = parseInt(v)
		w.header.Del("X-Gw-Prompt-Tokens")
	}
	if v := w.header.Get("X-Gw-Completion-Tokens"); v != "" {
		w.compTokens, _ = parseInt(v)
		w.header.Del("X-Gw-Completion-Tokens")
	}
	if v := w.header.Get("X-Gw-Total-Tokens"); v != "" {
		w.totalTokens, _ = parseInt(v)
		w.header.Del("X-Gw-Total-Tokens")
	}
	if v := w.header.Get("X-Gw-Finish-Reason"); v != "" {
		w.finishReason = v
		w.header.Del("X-Gw-Finish-Reason")
	}
	w.hasTokens = w.totalTokens > 0
}

// statusWriter wraps the real ResponseWriter for the audit middleware layer.
// It captures the final status code and intercepts X-Gw-* token headers.
type statusWriter struct {
	http.ResponseWriter
	status       int
	promptTokens int
	compTokens   int
	totalTokens  int
	hasTokens    bool
	finishReason string
	streaming    bool
	tail         []byte
	firstWriteAt time.Time
}

const swMaxTailBytes = 64 << 10

func (w *statusWriter) WriteHeader(code int) {
	if code == http.StatusContinue {
		return
	}
	w.streaming = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	w.interceptTokenHeaders()
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.streaming = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
		w.interceptTokenHeaders()
		w.status = 200
	}
	if w.firstWriteAt.IsZero() && len(b) > 0 {
		w.firstWriteAt = time.Now()
	}
	if w.streaming {
		w.appendTail(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) appendTail(b []byte) {
	if len(b) >= swMaxTailBytes {
		w.tail = append(w.tail[:0], b[len(b)-swMaxTailBytes:]...)
		return
	}
	w.tail = append(w.tail, b...)
	if len(w.tail) > swMaxTailBytes {
		w.tail = w.tail[len(w.tail)-swMaxTailBytes:]
	}
}

func (w *statusWriter) interceptTokenHeaders() {
	if v := w.Header().Get("X-Gw-Prompt-Tokens"); v != "" {
		w.promptTokens, _ = parseInt(v)
		w.Header().Del("X-Gw-Prompt-Tokens")
	}
	if v := w.Header().Get("X-Gw-Completion-Tokens"); v != "" {
		w.compTokens, _ = parseInt(v)
		w.Header().Del("X-Gw-Completion-Tokens")
	}
	if v := w.Header().Get("X-Gw-Total-Tokens"); v != "" {
		w.totalTokens, _ = parseInt(v)
		w.Header().Del("X-Gw-Total-Tokens")
	}
	if v := w.Header().Get("X-Gw-Finish-Reason"); v != "" {
		w.finishReason = v
		w.Header().Del("X-Gw-Finish-Reason")
	}
	w.hasTokens = w.totalTokens > 0
}

func parseInt(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errInvalidInt
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errInvalidInt = &parseError{"invalid integer"}

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }
