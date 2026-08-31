// Package audit implements a built-in audit sink plugin for the aware-gateway.
//
// It supports two storage backends:
//   - log: structured JSON via slog (always on when enabled)
//   - sqlite: persistent SQLite store (modernc.org/sqlite, pure Go, no CGO)
//
// Configuration (under plugins.audit in gateway.yaml):
//
//	plugins:
//	  audit:
//	    enabled: true
//	    store: both              # log | sqlite | both
//	    sqlite_path: ./data/audit.db
//	    log_truncate_body: 1024
package audit

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/plugin"
)

type Config struct {
	Enabled         bool   `yaml:"enabled"`
	Store           string `yaml:"store"` // log | sqlite | both
	SQLitePath      string `yaml:"sqlite_path"`
	LogTruncateBody int    `yaml:"log_truncate_body"`
}

type Plugin struct {
	cfg    Config
	logger *slog.Logger
	store  *Store
}

func (p *Plugin) Name() string { return "audit" }

func (p *Plugin) Init(ctx *plugin.Context) error {
	p.logger = ctx.Logger

	cfg, ok := config.PluginConfig[Config](ctx.Config, "audit")
	if !ok {
		// Check top-level audit config for backward compat
		if ctx.Config.Audit.Enabled {
			cfg = Config{
				Enabled:         ctx.Config.Audit.Enabled,
				Store:           ctx.Config.Audit.Store,
				SQLitePath:      ctx.Config.Audit.SQLitePath,
				LogTruncateBody: ctx.Config.Audit.LogTruncateBody,
			}
		} else {
			cfg = Config{Enabled: true, Store: "log"}
		}
	}
	p.cfg = cfg

	if !cfg.Enabled {
		p.logger.Info("audit: disabled")
		return nil
	}

	if cfg.Store == "" {
		cfg.Store = "log"
	}

	if cfg.Store == "sqlite" || cfg.Store == "both" {
		if cfg.SQLitePath == "" {
			cfg.SQLitePath = "data/audit.db"
		}
		if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0755); err != nil {
			p.logger.Warn("audit: dir create failed", "error", err)
		}
		store, err := Open(cfg.SQLitePath)
		if err != nil {
			p.logger.Error("audit: sqlite open failed, falling back to log-only", "error", err)
			cfg.Store = "log"
		} else {
			p.store = store
			p.logger.Info("audit: sqlite store opened", "path", cfg.SQLitePath)
		}
	}

	p.logger.Info("audit initialized", "store", cfg.Store)
	return nil
}

func (p *Plugin) Close() error {
	if p.store != nil {
		return p.store.Close()
	}
	return nil
}

func (p *Plugin) Record(record *plugin.AuditRecord) error {
	if !p.cfg.Enabled {
		return nil
	}

	// Log sink
	if p.cfg.Store == "log" || p.cfg.Store == "both" {
		p.logRecord(record)
	}

	// SQLite sink
	if p.cfg.Store == "sqlite" || p.cfg.Store == "both" {
		if p.store != nil {
			p.store.Record(Record{
				TraceID:      record.TraceID,
				Timestamp:    record.Timestamp,
				Method:       record.Method,
				Path:         record.Path,
				Endpoint:     record.Endpoint,
				Status:       record.Status,
				LatencyMs:    record.LatencyMs,
				Model:        record.Model,
				RoutedModel:  record.RoutedModel,
				Pool:         record.Pool,
				PromptTokens: record.PromptTokens,
				CompTokens:   record.CompTokens,
				TotalTokens:  record.TotalTokens,
				RetryAttempt: record.RetryAttempt,
				Fallback:     record.Fallback,
				UserID:       record.UserID,
				APIKey:       record.APIKey,
				Cost:         record.Cost,
			})
		}
	}

	return nil
}

func (p *Plugin) logRecord(record *plugin.AuditRecord) {
	data, err := json.Marshal(record)
	if err != nil {
		slog.Error("audit: marshal failed", "error", err)
		return
	}
	if record.Status >= 400 {
		slog.Error("audit: request failed", "record", string(data))
	} else {
		slog.Info("audit: request completed", "record", string(data))
	}
}

// --- SQLite Store ---

type Record struct {
	TraceID      string    `json:"trace_id"`
	Timestamp    time.Time `json:"timestamp"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Endpoint     string    `json:"endpoint"`
	Status       int       `json:"status"`
	LatencyMs    int64     `json:"latency_ms"`
	Model        string    `json:"model"`
	RoutedModel  string    `json:"routed_model"`
	Pool         string    `json:"pool"`
	PromptTokens int       `json:"prompt_tokens"`
	CompTokens   int       `json:"completion_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	RetryAttempt int       `json:"retry_attempt"`
	Fallback     string    `json:"fallback"`
	UserID       string    `json:"user_id"`
	APIKey       string    `json:"api_key"`
	Cost         float64   `json:"cost"`
}

type Store struct {
	db            *sql.DB
	mu            sync.Mutex
	buf           []Record
	flushInterval time.Duration
	stopCh        chan struct{}
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// Create table
	schema := `
	CREATE TABLE IF NOT EXISTS audit (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id TEXT,
		timestamp TEXT,
		method TEXT,
		path TEXT,
		endpoint TEXT,
		status INTEGER,
		latency_ms INTEGER,
		model TEXT,
		routed_model TEXT,
		pool TEXT,
		prompt_tokens INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		retry_attempt INTEGER DEFAULT 0,
		fallback TEXT,
		user_id TEXT,
		api_key TEXT,
		cost REAL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_audit_trace ON audit(trace_id);
	CREATE INDEX IF NOT EXISTS idx_audit_time ON audit(timestamp);
	CREATE INDEX IF NOT EXISTS idx_audit_pool ON audit(pool);
	CREATE INDEX IF NOT EXISTS idx_audit_model ON audit(model);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	s := &Store{
		db:            db,
		buf:           make([]Record, 0, 100),
		flushInterval: 5 * time.Second,
		stopCh:        make(chan struct{}),
	}
	go s.flushLoop()
	return s, nil
}

func (s *Store) Record(r Record) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.buf = append(s.buf, r)
	shouldFlush := len(s.buf) >= 50
	s.mu.Unlock()

	if shouldFlush {
		go s.Flush()
	}
}

func (s *Store) Flush() {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.buf
	s.buf = make([]Record, 0, 100)
	s.mu.Unlock()

	s.flush(batch)
}

func (s *Store) flush(records []Record) {
	tx, err := s.db.Begin()
	if err != nil {
		slog.Error("audit: begin tx failed", "error", err)
		return
	}
	stmt, err := tx.Prepare(`INSERT INTO audit
		(trace_id, timestamp, method, path, endpoint, status, latency_ms, model, routed_model, pool,
		 prompt_tokens, completion_tokens, total_tokens, retry_attempt, fallback, user_id, api_key, cost)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		slog.Error("audit: prepare failed", "error", err)
		tx.Rollback()
		return
	}
	defer stmt.Close()

	for _, r := range records {
		_, err := stmt.Exec(
			r.TraceID, r.Timestamp.Format(time.RFC3339Nano), r.Method, r.Path, r.Endpoint,
			r.Status, r.LatencyMs, r.Model, r.RoutedModel, r.Pool,
			r.PromptTokens, r.CompTokens, r.TotalTokens, r.RetryAttempt, r.Fallback,
			r.UserID, r.APIKey, r.Cost,
		)
		if err != nil {
			slog.Error("audit: insert failed", "error", err)
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Error("audit: commit failed", "error", err)
	}
}

func (s *Store) flushLoop() {
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Flush()
		case <-s.stopCh:
			s.Flush()
			return
		}
	}
}

func (s *Store) Close() error {
	close(s.stopCh)
	return s.db.Close()
}
