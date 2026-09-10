.DEFAULT_GOAL := run

# Start the API on :8080 (override with ADDR, e.g. make run ADDR=:9000).
.PHONY: run
run:
	go run ./cmd/server

.PHONY: test
test:
	go test -race -cover ./...

.PHONY: lint
lint:
	gofmt -l .
	go vet ./...
