.PHONY: run test build tidy vet

# Run the server locally. Loads .env automatically (optional APP_URL/DATA_DIR/
# PORT); the admin password is set on first run at /_app/setup.
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
