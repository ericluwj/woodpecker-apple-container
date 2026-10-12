package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

type fakeRunner struct {
	mu                  sync.Mutex
	calls               [][]string
	containers          map[string]containerInfo
	networks            map[string]string
	volumes             map[string]string
	failDNS, failDelete bool
}

func option(args []string, key string) string {
	for i, a := range args {
		if a == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func label(args []string) string {
	for i, a := range args {
		if a == "--label" && i+1 < len(args) && strings.HasPrefix(args[i+1], ownerLabel+"=") {
			return strings.TrimPrefix(args[i+1], ownerLabel+"=")
		}
	}
	return ""
}
func fakeInfo(args []string) containerInfo {
	c := containerInfo{ID: option(args, "--name")}
	c.Configuration.Labels = map[string]string{ownerLabel: label(args)}
	c.Status.State = "running"
	date := json.RawMessage(`"2026-10-07T00:00:00Z"`)
	c.Status.StartedDate = &date
	c.Status.Networks = append(c.Status.Networks, struct {
		Network     string `json:"network"`
		IPv4Address string `json:"ipv4Address"`
	}{option(args, "--network"), "192.168.65.2/24"})
	return c
}

func (f *fakeRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "volume":
		if f.volumes == nil {
			f.volumes = make(map[string]string)
		}
		switch args[1] {
		case "create":
			f.volumes[args[len(args)-1]] = label(args)
		case "delete":
			delete(f.volumes, args[len(args)-1])
		case "list":
			result := []map[string]any{}
			for name, owner := range f.volumes {
				result = append(result, map[string]any{"id": name, "configuration": map[string]any{"name": name, "labels": map[string]string{ownerLabel: owner}}})
			}
			return json.Marshal(result)
		}
	case "image":
		if args[1] == "inspect" {
			return []byte(`[{"variants":[{"platform":{"os":"linux","architecture":"arm64"}}]}]`), nil
		}
	case "network":
		switch args[1] {
		case "create":
			f.networks[args[len(args)-1]] = label(args)
		case "delete":
			delete(f.networks, args[len(args)-1])
		case "list":
			var result []map[string]any
			for name, owner := range f.networks {
				result = append(result, map[string]any{"id": name, "configuration": map[string]any{"name": name, "labels": map[string]string{ownerLabel: owner}}})
			}
			return json.Marshal(result)
		}
	case "run":
		if f.failDNS {
			return nil, errors.New("DNS startup failed")
		}
		c := fakeInfo(args)
		f.containers[c.ID] = c
	case "list":
		infos := make([]containerInfo, 0, len(f.containers))
		for _, c := range f.containers {
			infos = append(infos, c)
		}
		return json.Marshal(infos)
	case "stop":
		name := args[len(args)-1]
		c := f.containers[name]
		c.Status.State = "stopped"
		f.containers[name] = c
	case "delete":
		if f.failDelete {
			return nil, errors.New("delete failed")
		}
		delete(f.containers, args[len(args)-1])
	}
	return []byte("ok"), nil
}

func (f *fakeRunner) start(ctx context.Context, out io.Writer, args ...string) (*process, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	c := fakeInfo(args)
	mode := "normal"
	for _, a := range args {
		if a == "fast" || a == "long" || a == "fail-start" {
			mode = a
		}
	}
	if mode != "fail-start" {
		if mode == "fast" {
			c.Status.State = "stopped"
		}
		f.containers[c.ID] = c
	}
	f.mu.Unlock()
	p := &process{done: make(chan struct{})}
	go func() {
		if mode == "fail-start" {
			p.code = 1
			close(p.done)
			return
		}
		_, _ = out.Write([]byte("early stdout\n"))
		_, _ = out.Write([]byte("early stderr\n"))
		if mode == "long" {
			<-ctx.Done()
			p.err = ctx.Err()
		} else {
			select {
			case <-ctx.Done():
				p.err = ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			_, _ = out.Write([]byte("final output\n"))
			if mode == "fast" {
				p.code = 23
			}
		}
		f.mu.Lock()
		if c, ok := f.containers[c.ID]; ok {
			c.Status.State = "stopped"
			f.containers[c.ID] = c
		}
		f.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func testBackend(t *testing.T) (*Backend, *fakeRunner) {
	t.Helper()
	f := &fakeRunner{containers: map[string]containerInfo{}, networks: map[string]string{}}
	b := New()
	b.config.stateDir = t.TempDir()
	b.config.startupTimeout = time.Second
	b.runner = f
	b.resolve = func(context.Context, string, string, string) error { return nil }
	t.Cleanup(func() {
		b.mu.Lock()
		var tasks []string
		for task := range b.workflows {
			tasks = append(tasks, task)
		}
		b.mu.Unlock()
		for _, task := range tasks {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := b.DestroyWorkflow(ctx, nil, task)
			cancel()
			if err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	return b, f
}

func testStep(name, image string) *types.Step {
	return &types.Step{Name: name, UUID: name, Image: image, WorkspaceBase: "/woodpecker", WorkingDir: "/woodpecker/src", Volumes: []string{"volume:/woodpecker"}, Networks: []types.Conn{{Name: "network", Aliases: []string{name}}}, Environment: map[string]string{"EMPTY": "", "SECRET": "first\nsecond $(touch /tmp/unwanted)"}}
}

func testConf(steps ...*types.Step) *types.Config {
	return &types.Config{Volume: "volume", Network: "network", Stages: []*types.Stage{{Steps: steps}}}
}

func TestTranslationAndPull(t *testing.T) {
	for _, entry := range [][]string{nil, {"/custom", "--flag"}} {
		t.Run(fmt.Sprint(entry), func(t *testing.T) {
			b, f := testBackend(t)
			s := testStep("build", "alpine")
			s.Commands = []string{"printf hello"}
			s.Entrypoint = entry
			s.Pull = true
			if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
				t.Fatal(err)
			}
			if err := b.StartStep(context.Background(), s, "task"); err != nil {
				t.Fatal(err)
			}
			w, _ := b.getWorkflow("task")
			f.mu.Lock()
			calls := append([][]string(nil), f.calls...)
			f.mu.Unlock()
			var args []string
			pulls := 0
			for _, call := range calls {
				if call[0] == "image" {
					pulls++
				}
				if call[0] == "run" && option(call, "--detach") == "" && option(call, "--entrypoint") != "" {
					args = call
				}
			}
			if pulls != 1 {
				t.Fatalf("pull count %d", pulls)
			}
			wantEntry := "/bin/sh"
			if len(entry) > 0 {
				wantEntry = entry[0]
			}
			if option(args, "--entrypoint") != wantEntry {
				t.Fatal(args)
			}
			if option(args, "--mount") != "type=bind,source="+w.workspace+",target=/woodpecker" || option(args, "--dns") != w.dnsIP || option(args, "--platform") != "linux/arm64" {
				t.Fatal(args)
			}
			if !contains(args, "SECRET="+s.Environment["SECRET"]) || !contains(args, "EMPTY=") || !containsPrefix(args, "CI_SCRIPT=") {
				t.Fatal("lost environment or generated script")
			}
			if option(args, "--workdir") != "/woodpecker" {
				t.Fatal("command working directory must be workspace base")
			}
		})
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
func containsPrefix(ss []string, want string) bool {
	for _, s := range ss {
		if strings.HasPrefix(s, want) {
			return true
		}
	}
	return false
}

func TestDefaultEntrypointAndCachedImage(t *testing.T) {
	b, f := testBackend(t)
	s := testStep("plugin", "plugin-image")
	if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), s, "task"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, args := range f.calls {
		if args[0] == "image" {
			t.Fatal("unexpected forced pull")
		}
		if args[0] == "run" && contains(args, "plugin-image") && contains(args, "--entrypoint") {
			t.Fatal("overrode image entrypoint")
		}
	}
}

func TestLogsDrainBeforeWaitAndNonzeroExit(t *testing.T) {
	b, _ := testBackend(t)
	s := testStep("fast", "fast")
	if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), s, "task"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := b.TailStep(ctx, s, "task")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "early stdout\nearly stderr\nfinal output\n" {
		t.Fatalf("logs %q", out)
	}
	state, err := b.WaitStep(ctx, s, "task")
	if err != nil || state.ExitCode != 23 || !state.Exited || state.OOMKilled {
		t.Fatalf("state %+v, %v", state, err)
	}
}

func TestEarlyLogCloseDoesNotBlockWorkload(t *testing.T) {
	b, _ := testBackend(t)
	s := testStep("build", "normal")
	if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), s, "task"); err != nil {
		t.Fatal(err)
	}
	r, err := b.TailStep(context.Background(), s, "task")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := b.WaitStep(ctx, s, "task"); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWorkflowsAndSteps(t *testing.T) {
	b, f := testBackend(t)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			task := fmt.Sprint(i)
			a := testStep("postgres", "long")
			d := testStep("worker", "long")
			if err := b.SetupWorkflow(context.Background(), testConf(a, d), task); err != nil {
				t.Error(err)
				return
			}
			var starts sync.WaitGroup
			for _, s := range []*types.Step{a, d} {
				starts.Go(func() {
					if err := b.StartStep(context.Background(), s, task); err != nil {
						t.Error(err)
					}
				})
			}
			starts.Wait()
			w, _ := b.getWorkflow(task)
			data, err := os.ReadFile(filepath.Join(w.dir, "dns", "hosts"))
			if err != nil {
				t.Error(err)
			}
			if !strings.Contains(string(data), "postgres") || !strings.Contains(string(data), "worker") {
				t.Error("missing service records")
			}
			if err := b.DestroyWorkflow(context.Background(), nil, task); err != nil {
				t.Error(err)
			}
			if err := b.DestroyWorkflow(context.Background(), nil, task); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.containers) != 0 || len(f.networks) != 0 {
		t.Fatal("leaked resources")
	}
}

func TestCancellationAndIdempotentCleanup(t *testing.T) {
	b, f := testBackend(t)
	s := testStep("build", "long")
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.SetupWorkflow(ctx, testConf(s), "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(ctx, s, "task"); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if _, err := b.WaitStep(waitCtx, s, "task"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.DestroyStep(waitCtx, s, "task"); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.DestroyWorkflow(waitCtx, nil, "task"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.containers) > 0 || len(f.networks) > 0 {
		t.Fatal("leak")
	}
}

func TestSetupRollbackAndStartupFailure(t *testing.T) {
	t.Run("partial setup", func(t *testing.T) {
		b, f := testBackend(t)
		f.failDNS = true
		if err := b.SetupWorkflow(context.Background(), testConf(), "task"); err == nil {
			t.Fatal("expected error")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.networks) != 0 {
			t.Fatal("network leaked")
		}
	})
	t.Run("workload bootstrap", func(t *testing.T) {
		b, _ := testBackend(t)
		s := testStep("bad", "fail-start")
		if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
			t.Fatal(err)
		}
		if err := b.StartStep(context.Background(), s, "task"); err == nil || !strings.Contains(err.Error(), "before startup") {
			t.Fatal(err)
		}
	})
	t.Run("startup timeout", func(t *testing.T) {
		b, _ := testBackend(t)
		s := testStep("service", "long")
		s.Detached = true
		if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
			t.Fatal(err)
		}
		b.config.startupTimeout = 30 * time.Millisecond
		b.resolve = func(ctx context.Context, _, _, _ string) error { <-ctx.Done(); return ctx.Err() }
		if err := b.StartStep(context.Background(), s, "task"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if err := b.DestroyWorkflow(context.Background(), nil, "task"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFailedCleanupPreservesOwnership(t *testing.T) {
	b, f := testBackend(t)
	s := testStep("build", "long")
	if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), s, "task"); err != nil {
		t.Fatal(err)
	}
	w, _ := b.getWorkflow("task")
	f.mu.Lock()
	f.failDelete = true
	f.mu.Unlock()
	if err := b.DestroyWorkflow(context.Background(), nil, "task"); err == nil {
		t.Fatal("expected cleanup failure")
	}
	if _, err := os.Stat(filepath.Join(w.dir, "ownership.json")); err != nil {
		t.Fatal("lost ownership manifest")
	}
	f.mu.Lock()
	f.failDelete = false
	f.mu.Unlock()
	if err := b.DestroyWorkflow(context.Background(), nil, "task"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRefusesForeignResources(t *testing.T) {
	b, f := testBackend(t)
	if err := b.SetupWorkflow(context.Background(), testConf(), "task"); err != nil {
		t.Fatal(err)
	}
	w, _ := b.getWorkflow("task")
	f.mu.Lock()
	c := f.containers[w.dnsName]
	c.Configuration.Labels = map[string]string{ownerLabel: "foreign"}
	f.containers[w.dnsName] = c
	f.mu.Unlock()
	if err := b.DestroyWorkflow(context.Background(), nil, "task"); err == nil {
		t.Fatal("removed foreign helper")
	}
	f.mu.Lock()
	c.Configuration.Labels = map[string]string{ownerLabel: w.owner}
	f.containers[w.dnsName] = c
	f.mu.Unlock()
}

func TestUnsupportedSettingsRejectedBeforeSetup(t *testing.T) {
	cases := map[string]func(*types.Step){
		"privileged": func(s *types.Step) { s.Privileged = true }, "device": func(s *types.Step) { s.Devices = []string{"/dev/foo"} },
		"mount": func(s *types.Step) { s.Volumes = append(s.Volumes, "/Users:/host") }, "network": func(s *types.Step) { s.Networks = append(s.Networks, types.Conn{Name: "external"}) },
		"host network": func(s *types.Step) { s.NetworkMode = "host" }, "DNS": func(s *types.Step) { s.DNS = []string{"8.8.8.8"} },
		"extra hosts": func(s *types.Step) { s.ExtraHosts = []types.HostAlias{{Name: "host", IP: "1.2.3.4"}} },
		"port":        func(s *types.Step) { s.Ports = []types.Port{{}} }, "options": func(s *types.Step) { s.BackendOptions = map[string]any{"user": "root"} },
		"registry": func(s *types.Step) { s.AuthConfig.Password = "DO-NOT-LOG-ME" }, "environment": func(s *types.Step) { s.Environment["BAD=SECRET"] = "hidden" },
		"root workspace": func(s *types.Step) { s.WorkspaceBase = "/"; s.Volumes = []string{"volume:/"} }, "NUL": func(s *types.Step) { s.Environment["SECRET"] = "hidden\x00" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b, f := testBackend(t)
			s := testStep("build", "alpine")
			mutate(s)
			err := b.SetupWorkflow(context.Background(), testConf(s), "task")
			if err == nil {
				t.Fatal("accepted unsupported setting")
			}
			if strings.Contains(err.Error(), "DO-NOT-LOG-ME") || strings.Contains(err.Error(), "hidden") {
				t.Fatal("secret in error")
			}
			if len(f.calls) != 0 {
				t.Fatal("created resources before validation")
			}
		})
	}
}

func TestDNSHostsUpdates(t *testing.T) {
	b, _ := testBackend(t)
	s := testStep("postgres", "long")
	s.Detached = true
	s.Networks[0].Aliases = append(s.Networks[0].Aliases, "database")
	if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), s, "task"); err != nil {
		t.Fatal(err)
	}
	w, _ := b.getWorkflow("task")
	read := func() string {
		data, err := os.ReadFile(filepath.Join(w.dir, "dns", "hosts"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if !strings.Contains(read(), "database") {
		t.Fatal("alias missing")
	}
	if err := b.DestroyStep(context.Background(), s, "task"); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, "127.0.0.1 wp-dns-ready.invalid\n") {
		t.Fatalf("stale records: %s", got)
	}
}

func TestPluginWorkdirCannotFollowHostEscapingSymlink(t *testing.T) {
	b, _ := testBackend(t)
	s := testStep("plugin", "alpine")
	s.WorkingDir = "/woodpecker/escape/new-directory"
	if err := b.SetupWorkflow(context.Background(), testConf(s), "task"); err != nil {
		t.Fatal(err)
	}
	w, _ := b.getWorkflow("task")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(w.workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), s, "task"); err == nil {
		t.Fatal("accepted a workdir escaping the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "new-directory")); !os.IsNotExist(err) {
		t.Fatal("modified a host path outside the workspace")
	}
}

func TestAliasValidationAndCaseInsensitiveCollisions(t *testing.T) {
	for _, alias := range []string{"a..b", "a.", "a.-b", "a.b-", strings.Repeat("a", 64), "wp-dns-ready.invalid"} {
		s := testStep("build", "alpine")
		s.Networks[0].Aliases = []string{alias}
		if err := validateStep(s, testConf(s)); err == nil {
			t.Errorf("accepted invalid alias %q", alias)
		}
	}
	b, _ := testBackend(t)
	a := testStep("postgres", "long")
	c := testStep("POSTGRES", "long")
	if err := b.SetupWorkflow(context.Background(), testConf(a, c), "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), a, "task"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(context.Background(), c, "task"); err == nil {
		t.Fatal("accepted duplicate DNS alias with different case")
	}
}
