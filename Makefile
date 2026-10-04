BIN := bin/mergeyard

.PHONY: build assets test lint

build: assets
	go build -o $(BIN) ./cmd/mergeyard

assets:
	cd web && npm ci && npm run build

test:
	go test ./...

lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
