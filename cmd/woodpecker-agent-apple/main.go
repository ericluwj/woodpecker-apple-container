package main

import (
	"context"

	"go.woodpecker-ci.org/woodpecker/v3/cmd/agent/core"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
	"go.woodpecker-ci.org/woodpecker/v3/version"

	"woodpecker-macos-container/backend/apple"
)

func main() {
	if version.Version == "" {
		version.Version = "3.20.0"
	}
	core.RunAgent(context.Background(), []types.Backend{apple.New()})
}
