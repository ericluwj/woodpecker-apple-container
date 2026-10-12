package apple

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

const testMirrorsJSON = `{"docker.io":"registry-docker-io.pi-xcels.com","registry.k8s.io":"registry-k8s-io.pi-xcels.com","gcr.io":"registry-gcr-io.pi-xcels.com","ghcr.io":"registry-ghcr-io.pi-xcels.com"}`

func TestRegistryMirrorResolution(t *testing.T) {
	b := New()
	var err error
	b.config.registryMirrors, err = parseRegistryMirrors(testMirrorsJSON)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ image, want string }{
		{"alpine", "registry-docker-io.pi-xcels.com/library/alpine"},
		{"alpine:3.22", "registry-docker-io.pi-xcels.com/library/alpine:3.22"},
		{"library/alpine:3.22", "registry-docker-io.pi-xcels.com/library/alpine:3.22"},
		{"docker.io/alpine:3.22", "registry-docker-io.pi-xcels.com/library/alpine:3.22"},
		{"index.docker.io/library/alpine:3.22", "registry-docker-io.pi-xcels.com/library/alpine:3.22"},
		{"woodpeckerci/plugin-git:2.10.0", "registry-docker-io.pi-xcels.com/woodpeckerci/plugin-git:2.10.0"},
		{"docker.io/woodpeckerci/plugin-git:2.10.0", "registry-docker-io.pi-xcels.com/woodpeckerci/plugin-git:2.10.0"},
		{"registry.k8s.io/pause:3.10", "registry-k8s-io.pi-xcels.com/pause:3.10"},
		{"gcr.io/project/image:tag", "registry-gcr-io.pi-xcels.com/project/image:tag"},
		{"ghcr.io/org/image:tag", "registry-ghcr-io.pi-xcels.com/org/image:tag"},
		{"alpine@" + digest, "registry-docker-io.pi-xcels.com/library/alpine@" + digest},
		{"docker.io/library/alpine:3.22@" + digest, "registry-docker-io.pi-xcels.com/library/alpine:3.22@" + digest},
		{"quay.io/woodpeckerci/plugin-git:2.10.0", "quay.io/woodpeckerci/plugin-git:2.10.0"},
		{"registry.pi-xcels.com/private/image:tag", "registry.pi-xcels.com/private/image:tag"},
		{"localhost:5000/image:tag", "localhost:5000/image:tag"},
		{"registry-docker-io.pi-xcels.com/library/alpine:3.22", "registry-docker-io.pi-xcels.com/library/alpine:3.22"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			got, err := b.resolveImage(tc.image)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, image := range []string{"https://user:DO-NOT-LOG-ME@docker.io/alpine", "-bad", "alpine\nsecret"} {
		_, err := b.resolveImage(image)
		if err == nil || strings.Contains(err.Error(), "DO-NOT-LOG-ME") {
			t.Fatal("invalid reference accepted or sensitive input disclosed")
		}
	}
	// No opt-in means no normalization or validation change to existing users.
	b.config.registryMirrors = nil
	for _, image := range []string{"alpine", "index.docker.io/library/alpine", "legacy-reference"} {
		got, err := b.resolveImage(image)
		if err != nil || got != image {
			t.Fatal("unconfigured behavior changed")
		}
	}
}

func TestRegistryMirrorConfig(t *testing.T) {
	for _, raw := range []string{"", " ", "{}"} {
		got, err := parseRegistryMirrors(raw)
		if err != nil || len(got) != 0 {
			t.Fatalf("empty config: %v, %v", got, err)
		}
	}
	got, err := parseRegistryMirrors(`{"INDEX.DOCKER.IO":"cache.example:443"}`)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"docker.io": "cache.example:443"}) {
		t.Fatalf("alias/port config: %v, %v", got, err)
	}
	for _, raw := range []string{
		`null`, `[]`, `true`, `{"docker.io":42}`, `invalid-json`,
		`{"docker.io":"https://cache.example"}`,
		`{"docker.io":"http://cache.example"}`,
		`{"docker.io":"user:DO-NOT-LOG-ME@cache.example"}`,
		`{"docker.io":"cache.example/path"}`,
		`{"docker.io":"cache.example?secret=DO-NOT-LOG-ME"}`,
		`{"docker.io":"cache.example#secret"}`,
		`{"docker.io":" cache.example"}`,
		`{"docker.io":"cache.example\n"}`,
		`{"docker.io":""}`, `{"docker.io":"cache"}`,
		`{"docker.io/path":"cache.example"}`,
		`{"docker.io":"cache.example","index.docker.io":"other.example"}`,
	} {
		_, err := parseRegistryMirrors(raw)
		if err == nil || strings.Contains(err.Error(), "DO-NOT-LOG-ME") {
			t.Fatal("invalid mapping accepted or sensitive input disclosed")
		}
	}
}

func TestRegistryMirrorFlagAndEnvironment(t *testing.T) {
	for _, useFlag := range []bool{false, true} {
		t.Run(map[bool]string{false: "environment", true: "flag"}[useFlag], func(t *testing.T) {
			t.Setenv("WOODPECKER_BACKEND_APPLE_REGISTRY_MIRRORS", testMirrorsJSON)
			b := New()
			args := []string{"test"}
			if useFlag {
				args = append(args, "--backend-apple-registry-mirrors", `{"docker.io":"override.example"}`)
			}
			app := &cli.Command{Flags: b.Flags(), Action: func(ctx context.Context, cmd *cli.Command) error {
				c := configFromContext(context.WithValue(ctx, types.CliCommand, cmd))
				mirrors, err := parseRegistryMirrors(c.registryMirrorsJSON)
				if err != nil {
					return err
				}
				want := "registry-docker-io.pi-xcels.com"
				if useFlag {
					want = "override.example"
				}
				if mirrors["docker.io"] != want {
					t.Fatalf("mapping not loaded: %v", mirrors)
				}
				return nil
			}}
			if err := app.Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRegistryMirrorsCoverPullRunAndDNS(t *testing.T) {
	for _, pull := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached", true: "pull"}[pull], func(t *testing.T) {
			b, f := testBackend(t)
			b.config.registryMirrors, _ = parseRegistryMirrors(testMirrorsJSON)
			s := testStep("clone", "docker.io/woodpeckerci/plugin-git:2.10.0")
			s.Pull = pull
			s.Commands = []string{"echo ready"}
			original := s.Image
			if err := b.SetupWorkflow(t.Context(), testConf(s), "task"); err != nil {
				t.Fatal(err)
			}
			if err := b.StartStep(t.Context(), s, "task"); err != nil {
				t.Fatal(err)
			}
			if s.Image != original {
				t.Fatal("mutated the workflow's logical image")
			}
			f.mu.Lock()
			calls := append([][]string(nil), f.calls...)
			f.mu.Unlock()
			var dnsRuns, stepRuns, pulls int
			for _, call := range calls {
				if call[0] == "image" && call[1] == "pull" {
					pulls++
					if call[len(call)-1] != "registry-docker-io.pi-xcels.com/woodpeckerci/plugin-git:2.10.0" {
						t.Fatal("explicit pull bypassed mirror")
					}
				}
				if call[0] == "run" {
					if contains(call, "--detach") {
						dnsRuns++
						if !contains(call, "registry-docker-io.pi-xcels.com/coredns/coredns:1.14.7") {
							t.Fatal("DNS helper bypassed mirror")
						}
					} else {
						stepRuns++
						if !contains(call, "registry-docker-io.pi-xcels.com/woodpeckerci/plugin-git:2.10.0") {
							t.Fatal("step run bypassed mirror")
						}
					}
				}
			}
			if dnsRuns != 1 || stepRuns != 1 || pulls != map[bool]int{false: 0, true: 1}[pull] {
				t.Fatalf("DNS=%d steps=%d pulls=%d", dnsRuns, stepRuns, pulls)
			}
		})
	}
}

type mirrorImageRunner struct {
	runner
	imageCalls [][]string
	cached     map[string]string // image -> cached architecture
	failures   map[string]error
	onPull     func(context.Context) error
}

func (r *mirrorImageRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	if args[0] != "image" {
		return r.runner.run(ctx, args...)
	}
	r.imageCalls = append(r.imageCalls, append([]string(nil), args...))
	image := args[len(args)-1]
	if args[1] == "inspect" {
		if arch := r.cached[image]; arch != "" {
			return []byte(`[{"variants":[{"platform":{"os":"linux","architecture":"` + arch + `"}}]}]`), nil
		}
		return nil, errors.New("image not cached")
	}
	if r.onPull != nil {
		if err := r.onPull(ctx); err != nil {
			return nil, err
		}
	}
	return nil, r.failures[image]
}

func TestRegistryMirrorFallback(t *testing.T) {
	const mirror = "registry-docker-io.pi-xcels.com/library/alpine:3.22"
	const origin = "docker.io/library/alpine:3.22"
	for _, tc := range []struct {
		name                                string
		force, originFails, cancel, timeout bool
		mirrorArch, originArch              string
		wantImage                           string
		wantPulls                           []string
	}{
		{name: "forced", force: true, mirrorArch: "arm64", originArch: "arm64", wantImage: origin, wantPulls: []string{mirror, origin}},
		{name: "automatic", wantImage: origin, wantPulls: []string{mirror, origin}},
		{name: "cached mirror", mirrorArch: "arm64", wantImage: mirror},
		{name: "cached origin", originArch: "arm64", wantImage: origin, wantPulls: []string{mirror}},
		{name: "wrong cached platform", mirrorArch: "amd64", wantImage: origin, wantPulls: []string{mirror, origin}},
		{name: "both fail", force: true, originFails: true, wantPulls: []string{mirror, origin}},
		{name: "canceled", force: true, cancel: true, wantPulls: []string{mirror}},
		{name: "timeout", force: true, timeout: true, wantPulls: []string{mirror}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f := testBackend(t)
			b.config.registryMirrors, _ = parseRegistryMirrors(testMirrorsJSON)
			s := testStep("build", "alpine:3.22")
			s.Pull = tc.force
			if err := b.SetupWorkflow(t.Context(), testConf(s), "task"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := &mirrorImageRunner{runner: f, cached: map[string]string{mirror: tc.mirrorArch, origin: tc.originArch}, failures: map[string]error{mirror: errors.New("mirror unavailable")}}
			if tc.originFails {
				r.failures[origin] = errors.New("origin unavailable")
			}
			if tc.cancel {
				r.onPull = func(context.Context) error { cancel(); return context.Canceled }
			}
			if tc.timeout {
				b.config.startupTimeout = 20 * time.Millisecond
				r.onPull = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
			}
			b.runner = r
			err := b.StartStep(ctx, s, "task")
			if (err == nil) != (tc.wantImage != "") {
				t.Fatalf("startup error: %v", err)
			}
			if tc.cancel && !errors.Is(err, context.Canceled) || tc.timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost cancellation/timeout: %v", err)
			}
			if tc.originFails && (!strings.Contains(err.Error(), "mirror unavailable") || !strings.Contains(err.Error(), "origin unavailable")) {
				t.Fatalf("lost pull failure information: %v", err)
			}
			var pulls []string
			for _, call := range r.imageCalls {
				if call[1] == "pull" {
					if option(call, "--platform") != "linux/arm64" {
						t.Fatal("pull lost workload platform")
					}
					pulls = append(pulls, call[len(call)-1])
				}
			}
			if !reflect.DeepEqual(pulls, tc.wantPulls) {
				t.Fatalf("pulls %v; want %v", pulls, tc.wantPulls)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			runs := 0
			for _, call := range f.calls {
				if call[0] == "run" && !contains(call, "--detach") {
					runs++
					if !contains(call, tc.wantImage) {
						t.Fatalf("ran wrong image: want %q", tc.wantImage)
					}
				}
			}
			if runs != map[bool]int{true: 1, false: 0}[tc.wantImage != ""] || s.Image != "alpine:3.22" {
				t.Fatal("wrong workload run count or mutated logical image")
			}
		})
	}
}

func TestRegistryMirrorDNSFallback(t *testing.T) {
	b, f := testBackend(t)
	b.config.registryMirrors, _ = parseRegistryMirrors(testMirrorsJSON)
	mirror := "registry-docker-io.pi-xcels.com/coredns/coredns:1.14.7"
	origin := "docker.io/coredns/coredns:1.14.7"
	r := &mirrorImageRunner{runner: f, failures: map[string]error{mirror: errors.New("mirror unavailable")}}
	b.runner = r
	if err := b.SetupWorkflow(t.Context(), testConf(), "task"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call[0] == "run" && contains(call, "--detach") && contains(call, origin) {
			return
		}
	}
	t.Fatal("DNS helper did not run the origin image")
}

func TestRegistryMirrorDoesNotRetryWorkloadFailures(t *testing.T) {
	for _, mode := range []string{"fast", "fail-start"} {
		t.Run(mode, func(t *testing.T) {
			b, f := testBackend(t)
			b.config.registryMirrors, _ = parseRegistryMirrors(testMirrorsJSON)
			s := testStep("build", "alpine:3.22")
			s.Entrypoint = []string{mode}
			if err := b.SetupWorkflow(t.Context(), testConf(s), "task"); err != nil {
				t.Fatal(err)
			}
			err := b.StartStep(t.Context(), s, "task")
			if mode == "fail-start" {
				if err == nil {
					t.Fatal("expected startup failure")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				state, err := b.WaitStep(t.Context(), s, "task")
				if err != nil || state.ExitCode != 23 {
					t.Fatalf("lost workload exit: %+v, %v", state, err)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			runs := 0
			for _, call := range f.calls {
				if call[0] == "image" && call[1] == "pull" {
					t.Fatal("pulled another image after workload failure")
				}
				if call[0] == "run" && !contains(call, "--detach") {
					runs++
				}
			}
			if runs != 1 {
				t.Fatalf("workload executed %d times", runs)
			}
		})
	}
}

func TestRegistryMirrorFallbackPreservesDigest(t *testing.T) {
	b, f := testBackend(t)
	b.config.registryMirrors, _ = parseRegistryMirrors(testMirrorsJSON)
	digest := "sha256:" + strings.Repeat("a", 64)
	mirror := "registry-docker-io.pi-xcels.com/library/alpine:3.22@" + digest
	origin := "docker.io/library/alpine:3.22@" + digest
	r := &mirrorImageRunner{runner: f, failures: map[string]error{mirror: errors.New("mirror unavailable")}}
	b.runner = r
	image, err := b.prepareImage(t.Context(), "alpine:3.22@"+digest, true)
	if err != nil || image != origin || r.imageCalls[len(r.imageCalls)-1][len(r.imageCalls[len(r.imageCalls)-1])-1] != origin {
		t.Fatalf("fallback changed tag/digest: %q, %v", image, err)
	}
}

func TestRegistryMirrorsDoNotAcceptWorkflowCredentials(t *testing.T) {
	b, f := testBackend(t)
	b.config.registryMirrors, _ = parseRegistryMirrors(testMirrorsJSON)
	s := testStep("private", "ghcr.io/org/private:tag")
	s.AuthConfig.Username, s.AuthConfig.Password = "user", "DO-NOT-LOG-ME"
	err := b.SetupWorkflow(t.Context(), testConf(s), "task")
	if err == nil || strings.Contains(err.Error(), s.AuthConfig.Password) {
		t.Fatal("accepted workflow credentials or disclosed secret")
	}
	if len(f.calls) != 0 {
		t.Fatal("CLI used before rejecting workflow credentials")
	}
}
