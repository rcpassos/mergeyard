BIN := bin/mergeyard

.PHONY: build assets test lint

build: assets
	go build -o $(BIN) ./cmd/mergeyard

assets:
	cd web && npm ci && npm run build

test:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_preflight.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p '*_test.py'
	go test -timeout=30m ./...
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s docs/research/codex-m2 -p '*_test.py'

lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
