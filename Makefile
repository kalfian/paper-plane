.PHONY: run test build tidy vet

# Run the server locally. Reads env from the current shell; export
# ADMIN_PASSWORD (and optionally DATA_DIR/PORT/APP_URL) before running.
run:
	go run ./cmd/paperplane

# Full test suite with race detector and coverage.
test:
	go test ./... -race -cover

# Static, pure-Go build (no CGO) → single binary at ./bin/paperplane.
build:
	CGO_ENABLED=0 go build -o bin/paperplane ./cmd/paperplane

# Tidy module dependencies.
tidy:
	go mod tidy

# Vet all packages.
vet:
	go vet ./...
