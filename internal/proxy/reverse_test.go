package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aware/gateway/internal/pool"
)

func TestNewReverseProxyHonorsEndpointTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := NewReverseProxy(&pool.Endpoint{
		Name:    "slow-upstream",
		URL:     u,
		Timeout: 50 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	proxy.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("proxy returned after %s, want timeout before upstream sleep completes", elapsed)
	}
}

func TestNewReverseProxyHonorsStreamingBodyTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := NewReverseProxy(&pool.Endpoint{
		Name:    "slow-stream",
		URL:     u,
		Timeout: 50 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	proxy.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if elapsed > 400*time.Millisecond {
		t.Fatalf("proxy returned after %s, want streaming body timeout before upstream sleep completes", elapsed)
	}
}

func TestNewReverseProxyTreatsNonStreamingBodyTimeoutAsError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"partial`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := NewReverseProxy(&pool.Endpoint{
		Name:    "slow-json-body",
		URL:     u,
		Timeout: 50 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	proxy.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("proxy returned after %s, want body timeout before upstream sleep completes", elapsed)
	}
}
