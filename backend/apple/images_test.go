package apple

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

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

type mirrorPullFailure struct {
	runner
	pulls  [][]string
	cancel context.CancelFunc
}

func (r *mirrorPullFailure) run(ctx context.Context, args ...string) ([]byte, error) {
	if args[0] == "image" && args[1] == "pull" {
		r.pulls = append(r.pulls, append([]string(nil), args...))
		if r.cancel != nil {
			r.cancel()
			return nil, ctx.Err()
		}
		return nil, errors.New("cache unavailable")
	}
	return r.runner.run(ctx, args...)
}

func TestRegistryMirrorFailureDoesNotFallback(t *testing.T) {
	for _, cancelPull := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancellation"}[cancelPull], func(t *testing.T) {
			b, f := testBackend(t)
			b.config.registryMirrors, _ = parseRegistryMirrors(testMirrorsJSON)
			s := testStep("build", "alpine:3.22")
			s.Pull = true
			if err := b.SetupWorkflow(t.Context(), testConf(s), "task"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := &mirrorPullFailure{runner: f}
			if cancelPull {
				r.cancel = cancel
			}
			b.runner = r
			err := b.StartStep(ctx, s, "task")
			if err == nil || (cancelPull && !errors.Is(err, context.Canceled)) {
				t.Fatalf("expected failure/cancellation, got %v", err)
			}
			if len(r.pulls) != 1 || r.pulls[0][len(r.pulls[0])-1] != "registry-docker-io.pi-xcels.com/library/alpine:3.22" {
				t.Fatal("cache failure retried or fell back to origin")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, call := range f.calls {
				if call[0] == "run" && contains(call, "registry-docker-io.pi-xcels.com/library/alpine:3.22") {
					t.Fatal("started workload after failed cache pull")
				}
			}
		})
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
