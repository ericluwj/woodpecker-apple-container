//go:build integration

package apple

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

func integrationBackend(t *testing.T) *Backend {
	t.Helper()
	if os.Getenv("APPLE_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set APPLE_CONTAINER_INTEGRATION=1 to run VM tests")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("requires Apple silicon macOS")
	}
	b := New()
	root := filepath.Join(t.TempDir(), "state")
	app := &cli.Command{Flags: b.Flags(), Action: func(ctx context.Context, c *cli.Command) error {
		_, err := b.Load(context.WithValue(ctx, types.CliCommand, c))
		return err
	}}
	if err := app.Run(t.Context(), []string{"integration", "--backend-apple-state-dir", root}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.mu.Lock()
		var tasks []string
		for task := range b.workflows {
			tasks = append(tasks, task)
		}
		b.mu.Unlock()
		for _, task := range tasks {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			err := b.DestroyWorkflow(ctx, nil, task)
			cancel()
			if err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	return b
}

func integrationRun(t *testing.T, b *Backend, task string, s *types.Step) (string, *types.State) {
	t.Helper()
	if err := b.StartStep(t.Context(), s, task); err != nil {
		t.Fatal(err)
	}
	r, err := b.TailStep(t.Context(), s, task)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	state, err := b.WaitStep(t.Context(), s, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DestroyStep(t.Context(), s, task); err != nil {
		t.Fatal(err)
	}
	return string(data), state
}

func TestIntegrationSharedWorkspaceAndFastLogs(t *testing.T) {
	b := integrationBackend(t)
	t.Setenv("APPLE_TEST_HOST_ONLY", "must-not-reach-workload")
	a := testStep("write", "alpine:3.22")
	a.Commands = []string{"printf shared > artifact", "printf 'stdout-marker\\n'", "printf 'stderr-marker\\n' >&2", `test -z "${APPLE_TEST_HOST_ONLY+x}"`, `test "$SECRET" = "$(printf 'first\nsecond $(touch /tmp/unwanted)')"`, "test ! -e /tmp/unwanted"}
	d := testStep("read", "alpine:3.22")
	d.Commands = []string{"test \"$(cat artifact)\" = shared", "exit 23"}
	if err := b.SetupWorkflow(t.Context(), testConf(a, d), t.Name()); err != nil {
		t.Fatal(err)
	}
	out, state := integrationRun(t, b, t.Name(), a)
	if state.ExitCode != 0 || !strings.Contains(out, "stdout-marker") || !strings.Contains(out, "stderr-marker") {
		t.Fatalf("state %+v output %s", state, out)
	}
	_, state = integrationRun(t, b, t.Name(), d)
	if state.ExitCode != 23 {
		t.Fatalf("exit %+v", state)
	}
	if err := b.DestroyWorkflow(t.Context(), nil, t.Name()); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationLiveLogs(t *testing.T) {
	b := integrationBackend(t)
	s := testStep("live", "alpine:3.22")
	s.Commands = []string{"printf 'live-start\\n'", "sleep 3", "printf 'live-end\\n'"}
	if err := b.SetupWorkflow(t.Context(), testConf(s), t.Name()); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(t.Context(), s, t.Name()); err != nil {
		t.Fatal(err)
	}
	r, err := b.TailStep(t.Context(), s, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "live-start\n" {
			break
		}
	}
	_, st, err := b.step(t.Name(), s)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.process.done:
		t.Fatal("logs were not delivered until completion")
	default:
	}
	data, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(data), "live-end") {
		t.Fatalf("remaining logs: %s, %v", data, err)
	}
	state, err := b.WaitStep(t.Context(), s, t.Name())
	if err != nil || state.ExitCode != 0 {
		t.Fatalf("state %+v, %v", state, err)
	}
}

func TestIntegrationServicesAndConcurrentWorkflows(t *testing.T) {
	b := integrationBackend(t)
	var wg sync.WaitGroup
	for _, task := range []string{"services-a", "services-b"} {
		wg.Go(func() {
			service := testStep("web", "busybox:1.37")
			service.Detached = true
			service.Commands = []string{"mkdir -p /www", "echo " + task + " > /www/index.html", "httpd -f -p 8080 -h /www"}
			client := testStep("client", "alpine:3.22")
			client.Commands = []string{"for i in 1 2 3 4 5; do wget -q -O /tmp/result http://web:8080 && break; sleep 1; done", "test \"$(cat /tmp/result)\" = " + task, "nslookup github.com", "test ! -e /host"}
			if err := b.SetupWorkflow(t.Context(), testConf(service, client), task); err != nil {
				t.Error(err)
				return
			}
			if err := b.StartStep(t.Context(), service, task); err != nil {
				t.Error(err)
				return
			}
			// Use a subtest for fatal failures without terminating this goroutine.
			t.Run(task, func(t *testing.T) {
				out, state := integrationRun(t, b, task, client)
				if state.ExitCode != 0 {
					t.Fatalf("state %+v logs %s", state, out)
				}
			})
			if err := b.DestroyWorkflow(t.Context(), nil, task); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestIntegrationCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "startup-timeout"}[timeout], func(t *testing.T) {
			b := integrationBackend(t)
			s := testStep("sleep", "alpine:3.22")
			s.Commands = []string{"sleep 600"}
			if err := b.SetupWorkflow(t.Context(), testConf(s), t.Name()); err != nil {
				t.Fatal(err)
			}
			w, _ := b.getWorkflow(t.Name())
			dir := w.dir
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if timeout {
				b.config.startupTimeout = time.Nanosecond
				if err := b.StartStep(ctx, s, t.Name()); err == nil {
					t.Fatal("expected startup timeout")
				}
			} else {
				if err := b.StartStep(ctx, s, t.Name()); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cleanupCancel()
			if err := b.DestroyWorkflow(cleanupCtx, nil, t.Name()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("workflow directory remains")
			}
			if info, err := b.container(cleanupCtx, w.dnsName); err != nil || info != nil {
				t.Fatal("DNS helper remains", err)
			}
		})
	}
}

func TestIntegrationCloneBuildPlugin(t *testing.T) {
	b := integrationBackend(t)
	// A public fixture avoids depending on deployment credentials.
	clone := testStep("clone", "woodpeckerci/plugin-git:2.10.1")
	clone.Type = types.StepTypeClone
	clone.Environment = map[string]string{"PLUGIN_REMOTE": "https://github.com/octocat/Hello-World.git", "PLUGIN_REF": "refs/heads/master", "PLUGIN_BRANCH": "master", "PLUGIN_DEPTH": "1", "PLUGIN_PATH": "/woodpecker/src"}
	build := testStep("build", "alpine:3.22")
	build.Commands = []string{"test -f README", "wc -c README > artifact"}
	plugin := testStep("plugin", "alpine:3.22")
	plugin.Type = types.StepTypePlugin
	plugin.Entrypoint = []string{"/bin/sh", "-c", "test -s artifact && printf 'plugin-output\\n'"}
	if err := b.SetupWorkflow(t.Context(), testConf(clone, build, plugin), t.Name()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*types.Step{clone, build, plugin} {
		out, state := integrationRun(t, b, t.Name(), s)
		if state.ExitCode != 0 {
			t.Fatalf("%s: state %+v logs %s", s.Name, state, out)
		}
	}
	w, _ := b.getWorkflow(t.Name())
	if _, err := os.Stat(filepath.Join(w.workspace, "src", "artifact")); err != nil {
		t.Fatal(err)
	}
}
