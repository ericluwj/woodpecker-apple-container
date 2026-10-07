// Package apple implements Woodpecker's backend using Apple's container CLI.
package apple

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

const Name = "apple-container"
const dnsImage = "coredns/coredns:1.14.7"
const ownerLabel = "io.woodpecker.apple.owner"

type config struct {
	binary, stateDir, memory string
	cpus                     int
	startupTimeout           time.Duration
}

func defaultConfig() config {
	root, err := os.UserCacheDir()
	if err != nil {
		root = os.TempDir()
	}
	return config{binary: "container", stateDir: filepath.Join(root, "woodpecker-apple"), memory: "1G", cpus: 2, startupTimeout: 5 * time.Minute}
}

func (*Backend) Name() string { return Name }

func (*Backend) Flags() []cli.Flag {
	c := defaultConfig()
	return []cli.Flag{
		&cli.StringFlag{Name: "backend-apple-binary", Value: c.binary, Sources: cli.EnvVars("WOODPECKER_BACKEND_APPLE_BINARY")},
		&cli.StringFlag{Name: "backend-apple-state-dir", Value: c.stateDir, Sources: cli.EnvVars("WOODPECKER_BACKEND_APPLE_STATE_DIR")},
		&cli.IntFlag{Name: "backend-apple-cpus", Value: c.cpus, Sources: cli.EnvVars("WOODPECKER_BACKEND_APPLE_CPUS")},
		&cli.StringFlag{Name: "backend-apple-memory", Value: c.memory, Sources: cli.EnvVars("WOODPECKER_BACKEND_APPLE_MEMORY")},
		&cli.DurationFlag{Name: "backend-apple-startup-timeout", Value: c.startupTimeout, Sources: cli.EnvVars("WOODPECKER_BACKEND_APPLE_STARTUP_TIMEOUT")},
	}
}

func configFromContext(ctx context.Context) config {
	c := defaultConfig()
	if cmd, ok := ctx.Value(types.CliCommand).(*cli.Command); ok {
		c.binary = cmd.String("backend-apple-binary")
		c.stateDir = cmd.String("backend-apple-state-dir")
		c.cpus = cmd.Int("backend-apple-cpus")
		c.memory = cmd.String("backend-apple-memory")
		c.startupTimeout = cmd.Duration("backend-apple-startup-timeout")
	}
	return c
}

func checkPlatform(ctx context.Context) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("Apple containers require macOS on Apple silicon")
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return fmt.Errorf("read macOS version: %w", err)
	}
	major, err := strconv.Atoi(strings.Split(strings.TrimSpace(string(out)), ".")[0])
	if err != nil || major < 26 {
		return fmt.Errorf("macOS 26 or newer is required")
	}
	return nil
}

func checkCLI(ctx context.Context, c config) (runner, error) {
	path, err := exec.LookPath(c.binary)
	if err != nil {
		return nil, fmt.Errorf("container executable unavailable: %w", err)
	}
	r := &execRunner{binary: path}
	out, err := r.run(ctx, "--version")
	if err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`\b1\.5\.0\b`).Match(out) {
		return nil, fmt.Errorf("this release requires container CLI 1.5.0")
	}
	if _, err := r.run(ctx, "list", "--all", "--format", "json"); err != nil {
		return nil, fmt.Errorf("container service unavailable; run container system start as the agent user: %w", err)
	}
	return r, nil
}

func (*Backend) IsAvailable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if checkPlatform(ctx) != nil {
		return false
	}
	_, err := checkCLI(ctx, configFromContext(ctx))
	return err == nil
}

func (b *Backend) Load(ctx context.Context) (*types.BackendInfo, error) {
	if err := checkPlatform(ctx); err != nil {
		return nil, err
	}
	c := configFromContext(ctx)
	if c.cpus < 1 || c.startupTimeout <= 0 || !regexp.MustCompile(`^[1-9][0-9]*[KMGTP]?$`).MatchString(c.memory) {
		return nil, fmt.Errorf("invalid CPU, memory, or startup timeout configuration")
	}
	root, err := filepath.Abs(c.stateDir)
	if err != nil {
		return nil, err
	}
	c.stateDir = root
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("state directory must be a private directory (mode 0700)")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := checkCLI(probeCtx, c)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.workflows) != 0 {
		return nil, fmt.Errorf("cannot reload backend with active workflows")
	}
	b.config, b.runner = c, r
	return &types.BackendInfo{Platform: "linux/arm64"}, nil
}
