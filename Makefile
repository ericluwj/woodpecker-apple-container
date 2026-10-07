.PHONY: build test integration server-integration

build:
	go build -trimpath -o bin/woodpecker-agent-apple ./cmd/woodpecker-agent-apple

test:
	go test -race ./...

integration:
	APPLE_CONTAINER_INTEGRATION=1 go test -race -tags=integration -timeout=20m -v ./backend/apple

server-integration:
	APPLE_CONTAINER_INTEGRATION=1 go test -race -tags='integration test' -timeout=20m -v ./integration
