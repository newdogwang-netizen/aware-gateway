.PHONY: build run dev test vet clean docker bundle static \
       start stop restart docker-stop docker-logs logs logsf smoke

BINARY   = aware-gateway
GO       = go
LDFLAGS  = -s -w
PORT     = 12026
CONFIG   = configs/gateway-openrouter.yaml
PIDFILE  = /tmp/aware-gateway.pid
LOGFILE  = /tmp/aware-gateway.log
ENVFILE  = openrouter.env

# Docker
IMAGE    = aware-gateway
TAG      = latest
DNAME    = aware-gateway
DPORT    = 12026

# ── Build (local binary) ──────────────────────────────

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/gateway/

static:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/gateway/

# ── Dev: build + kill-old + start + health-check ──────

dev: stop build
	@echo "Starting gateway on :$(PORT)..."
	@export GW_OPENROUTER_KEY=$$(cat $(ENVFILE)); \
	 nohup ./$(BINARY) -config $(CONFIG) > $(LOGFILE) 2>&1 & \
	 echo $$! > $(PIDFILE)
	@sleep 2
	@curl -sf http://localhost:$(PORT)/health >/dev/null 2>&1 \
	 && echo "✓ Gateway is UP on :$(PORT)  (pid=$$(cat $(PIDFILE)), log=$(LOGFILE))" \
	 || (echo "✗ Gateway failed to start — check $(LOGFILE)"; tail -20 $(LOGFILE); exit 1)

run: build
	./$(BINARY) -config $(CONFIG)

# ── Docker: build image + run container on :12026 ────
# Usage: make start
#   or:  make start PORT=8080

start: docker-stop
	@echo "Building Docker image $(IMAGE):$(TAG)..."
	@docker build -t $(IMAGE):$(TAG) . 2>&1 | tail -5
	@echo "Starting container $(DNAME) on :$(DPORT)..."
	@docker run -d \
		--name $(DNAME) \
		-p $(DPORT):12026 \
		-v $$(pwd)/$(ENVFILE):/etc/aware-gateway/openrouter.env:ro \
		-e GW_OPENROUTER_KEY=$$(cat $(ENVFILE)) \
		$(IMAGE):$(TAG)
	@sleep 3
	@curl -sf http://localhost:$(DPORT)/health >/dev/null 2>&1 \
	 && echo "✓ Container is UP on :$(DPORT)  (docker logs: make docker-logs)" \
	 || (echo "✗ Container failed — check logs:"; docker logs $(DNAME) 2>&1 | tail -20; exit 1)

restart: docker-stop start

docker-stop:
	-@docker rm -f $(DNAME) 2>/dev/null || true
	@true

docker-logs:
	@docker logs --tail 50 $(DNAME)

docker-logsf:
	@docker logs -f $(DNAME)

# ── Stop local process ────────────────────────────────

stop:
	@if [ -f $(PIDFILE) ]; then \
		kill $$(cat $(PIDFILE)) 2>/dev/null; \
		rm -f $(PIDFILE); \
		echo "Stopped (pid file)"; \
	fi
	-@pkill -f '$(BINARY) -config' 2>/dev/null || true
	@true

# ── Logs ──────────────────────────────────────────────

logs:
	@tail -50 $(LOGFILE)

logsf:
	@tail -f $(LOGFILE)

# ── Smoke tests (requires running gateway) ───────────

smoke:
	@echo "=== /health ==="
	@curl -sf http://localhost:$(PORT)/health | python3 -m json.tool 2>/dev/null | head -20
	@echo ""
	@echo "=== /v1/models (first 5) ==="
	@curl -sf http://localhost:$(PORT)/v1/models | python3 -c \
		"import sys,json;d=json.load(sys.stdin);print(f'total: {len(d[\"data\"])}');[print(f'  {m[\"id\"]}') for m in d['data'][:5]]" 2>/dev/null
	@echo ""
	@echo "=== chat completion ==="
	@curl -sf http://localhost:$(PORT)/v1/chat/completions \
		-H "Content-Type: application/json" \
		-d '{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"Say hi in 5 words"}],"max_tokens":30}' \
		| python3 -c \
		"import sys,json;d=json.load(sys.stdin);c=d['choices'][0]['message'];u=d.get('usage',{});print(f'model: {d[\"model\"]}');print(f'reply: {c.get(\"content\",\"\")}');print(f'tokens: {u}')" 2>/dev/null
	@echo ""
	@echo "=== /metrics (gen_ai) ==="
	@curl -sf http://localhost:$(PORT)/metrics | grep 'gen_ai_' | grep -v bucket | grep -v HELP | grep -v TYPE | head -10

# ── Test & Vet ────────────────────────────────────────

test:
	$(GO) test ./... -v -count=1

test-short:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

# ── Clean ─────────────────────────────────────────────

clean: stop
	rm -f $(BINARY)
	rm -f $(PIDFILE)

# ── Docker / Bundle ───────────────────────────────────

docker:
	docker build -t $(IMAGE):$(TAG) .

bundle: static
	tar czf aware-gateway-bundle.tar.gz $(BINARY) configs/gateway.yaml
