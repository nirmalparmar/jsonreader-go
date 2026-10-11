export GOTOOLCHAIN := local

.PHONY: dev dev-docker test build image runner-image up

# Local development on macOS/Linux without a sandbox. NEVER expose this.
dev:
	SANDBOX=unsafe ADDR=127.0.0.1:8080 DATA_DIR=./data \
	CHROME_PATH="$${CHROME_PATH:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}" \
	go run ./cmd/server

# Local development with real isolation: one throwaway container per run.
dev-docker: runner-image
	SANDBOX=docker SANDBOX_DOCKER_IMAGE=common-service-runner ADDR=127.0.0.1:8080 DATA_DIR=./data \
	CHROME_PATH="$${CHROME_PATH:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}" \
	go run ./cmd/server

test:
	go vet ./...
	go test -race ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/server ./cmd/server

image:
	docker build -t common-service:latest .

runner-image:
	docker build --target toolchains -t common-service-runner .

up:
	docker compose up -d --build
