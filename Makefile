BIN := bin/mergeyard

.PHONY: build test lint

build:
	go build -o $(BIN) ./cmd/mergeyard

test:
	go test ./...

lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
