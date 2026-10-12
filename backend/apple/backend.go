package apple

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

type Backend struct {
	mu        sync.Mutex
	config    config
	runner    runner
	resolve   func(context.Context, string, string, string) error
	workflows map[string]*workflow
}

type workflow struct {
	mu                                                   sync.Mutex
	destroyMu                                            sync.Mutex
	task, owner, dir, workspace, network, dnsName, dnsIP string
	conf                                                 *types.Config
	steps                                                map[string]*stepState
	closing                                              bool
	setupDone                                            chan struct{}
}

type stepState struct {
	name, logPath string
	aliases       []string
	ip            string
	ctx           context.Context
	cancel        context.CancelFunc
	startDone     chan struct{}
	process       *process // published by closing startDone
	spool         *logSpool
	destroyMu     sync.Mutex
	cleaned       bool
}

var _ types.Backend = (*Backend)(nil)

func New() *Backend {
	return &Backend{config: defaultConfig(), resolve: waitDNS, workflows: make(map[string]*workflow)}
}

func resourceID(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "wp-" + hex.EncodeToString(hash[:16])
}

// Ownership manifests contain identifiers only, never environment or secrets.
func (w *workflow) saveManifestLocked() error {
	steps := make([]string, 0, len(w.steps))
	for _, s := range w.steps {
		steps = append(steps, s.name)
	}
	data, err := json.MarshalIndent(struct {
		Owner      string   `json:"owner"`
		Workflow   string   `json:"workflow"`
		Network    string   `json:"network"`
		DNS        string   `json:"dns"`
		Containers []string `json:"containers"`
	}{w.owner, w.task, w.network, w.dnsName, steps}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(w.dir, "ownership.json"), data)
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (b *Backend) getWorkflow(task string) (*workflow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.workflows[task]
	if w == nil {
		return nil, fmt.Errorf("workflow is not set up")
	}
	return w, nil
}

func (b *Backend) SetupWorkflow(ctx context.Context, conf *types.Config, task string) (err error) {
	if conf == nil || conf.Volume == "" || conf.Network == "" || task == "" {
		return fmt.Errorf("workflow requires task, volume, and network identifiers")
	}
	seen := map[string]bool{}
	for _, stage := range conf.Stages {
		if stage == nil {
			return fmt.Errorf("nil workflow stage")
		}
		for _, s := range stage.Steps {
			if err := validateStep(s, conf); err != nil {
				return err
			}
			if seen[s.UUID] {
				return fmt.Errorf("duplicate step UUID")
			}
			seen[s.UUID] = true
		}
	}
	b.mu.Lock()
	if b.runner == nil {
		b.mu.Unlock()
		return fmt.Errorf("backend has not been loaded")
	}
	if b.workflows[task] != nil {
		b.mu.Unlock()
		return fmt.Errorf("workflow is already set up")
	}
	dir, err := os.MkdirTemp(b.config.stateDir, resourceID(task)+"-")
	if err != nil {
		b.mu.Unlock()
		return err
	}
	if strings.ContainsAny(dir, ",:\n\r") {
		b.mu.Unlock()
		os.RemoveAll(dir)
		return fmt.Errorf("state directory cannot contain CLI mount delimiters")
	}
	w := &workflow{task: task, owner: filepath.Base(dir), dir: dir, workspace: filepath.Join(dir, "workspace"), conf: conf,
		steps: make(map[string]*stepState), setupDone: make(chan struct{})}
	w.network = resourceID(w.owner, "network")
	w.dnsName = resourceID(w.owner, "dns")
	b.workflows[task] = w
	b.mu.Unlock()
	defer func() {
		close(w.setupDone)
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			err = errors.Join(err, b.DestroyWorkflow(cleanupCtx, conf, task))
		}
	}()
	for _, p := range []string{w.workspace, filepath.Join(dir, "dns"), filepath.Join(dir, "logs")} {
		if err = os.Mkdir(p, 0700); err != nil {
			return err
		}
	}
	if err = w.saveManifestLocked(); err != nil {
		return err
	}
	startupCtx, cancel := context.WithTimeout(ctx, b.config.startupTimeout)
	defer cancel()
	if _, err = b.runner.run(startupCtx, "network", "create", "--label", ownerLabel+"="+w.owner, w.network); err != nil {
		return err
	}
	if err = b.setupDNS(startupCtx, w); err != nil {
		return err
	}
	return nil
}

func (b *Backend) StartStep(ctx context.Context, step *types.Step, task string) (err error) {
	w, err := b.getWorkflow(task)
	if err != nil {
		return err
	}
	select {
	case <-w.setupDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err = validateStep(step, w.conf); err != nil {
		return err
	}
	w.mu.Lock()
	if w.closing || w.steps[step.UUID] != nil {
		w.mu.Unlock()
		return fmt.Errorf("workflow is closing or step has already started")
	}
	lifetime, cancel := context.WithCancel(ctx)
	s := &stepState{name: resourceID(w.owner, step.UUID), ctx: lifetime, cancel: cancel, startDone: make(chan struct{})}
	s.logPath = filepath.Join(w.dir, "logs", s.name+".log")
	s.aliases = append(s.aliases, step.Name)
	for _, n := range step.Networks {
		s.aliases = append(s.aliases, n.Aliases...)
	}
	// Resolve alias ambiguity before creating any step resource.
	for _, existing := range w.steps {
		for _, a := range existing.aliases {
			for _, other := range s.aliases {
				if strings.EqualFold(a, other) && !existing.cleaned {
					w.mu.Unlock()
					cancel()
					return fmt.Errorf("duplicate workflow DNS alias %q", a)
				}
			}
		}
	}
	w.steps[step.UUID] = s
	err = w.saveManifestLocked()
	w.mu.Unlock()
	defer func() {
		if err != nil {
			cancel()
		}
		close(s.startDone)
	}()
	go func() {
		<-lifetime.Done()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Failed automatic cleanup remains retryable via DestroyWorkflow and
		// leaves its ownership manifest for administrator recovery.
		_ = b.destroyStep(cleanupCtx, w, s)
	}()
	if err != nil {
		return err
	}
	// Create plugin workdirs inside the workspace without following symlinks
	// to host paths left behind by an earlier workload.
	if len(step.Commands) == 0 && step.WorkingDir != "" {
		rel, relErr := filepath.Rel(step.WorkspaceBase, path.Clean(step.WorkingDir))
		if relErr != nil {
			return relErr
		}
		if rel != ".." && !strings.HasPrefix(rel, "../") {
			root, rootErr := os.OpenRoot(w.workspace)
			if rootErr != nil {
				return rootErr
			}
			err = root.MkdirAll(rel, 0755)
			root.Close()
			if err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	s.spool = &logSpool{file: f}
	startupCtx, startupCancel := context.WithTimeout(lifetime, b.config.startupTimeout)
	defer startupCancel()
	image, err := b.prepareImage(startupCtx, step.Image, step.Pull)
	if err != nil {
		s.spool.close()
		return err
	}
	args, err := b.arguments(w, s, step, image)
	if err != nil {
		s.spool.close()
		return err
	}
	s.process, err = b.runner.start(lifetime, s.spool, args...)
	if err != nil {
		s.spool.close()
		return err
	}
	go func() { <-s.process.done; _ = s.spool.close() }()
	for {
		info, inspectErr := b.container(startupCtx, s.name)
		if inspectErr != nil {
			return inspectErr
		}
		if info != nil && info.Configuration.Labels[ownerLabel] != w.owner {
			return fmt.Errorf("container ownership mismatch")
		}
		if info != nil && info.Status.State == "running" {
			ip := info.IP(w.network)
			if ip == "" {
				return fmt.Errorf("running workload has no workflow network address")
			}
			w.mu.Lock()
			s.ip = ip
			err = b.writeHostsLocked(w)
			w.mu.Unlock()
			if err != nil {
				return err
			}
			if step.Detached || step.Type == types.StepTypeService {
				for _, alias := range s.aliases {
					if err = b.resolve(startupCtx, w.dnsIP, alias, ip); err != nil {
						return err
					}
				}
			}
			return nil
		}
		select {
		case <-s.process.done:
			if s.process.err != nil {
				return s.process.err
			}
			// Apple's run command removes the container on bootstrap errors,
			// but keeps it after a genuine workload exit, including nonzero.
			info, err = b.container(startupCtx, s.name)
			if err != nil {
				return err
			}
			if info == nil {
				return fmt.Errorf("workload failed before startup (CLI exit %d)", s.process.code)
			}
			return nil
		case <-startupCtx.Done():
			return startupCtx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (b *Backend) step(task string, step *types.Step) (*workflow, *stepState, error) {
	if step == nil {
		return nil, nil, fmt.Errorf("nil step")
	}
	w, err := b.getWorkflow(task)
	if err != nil {
		return nil, nil, err
	}
	w.mu.Lock()
	s := w.steps[step.UUID]
	w.mu.Unlock()
	if s == nil {
		return w, nil, fmt.Errorf("step has not started")
	}
	return w, s, nil
}

func (b *Backend) TailStep(ctx context.Context, step *types.Step, task string) (io.ReadCloser, error) {
	_, s, err := b.step(task, step)
	if err != nil {
		return nil, err
	}
	select {
	case <-s.startDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.process == nil {
		return nil, fmt.Errorf("step process unavailable")
	}
	f, err := os.Open(s.logPath)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithCancel(ctx)
	return &logReader{file: f, done: s.process.done, ctx: readCtx, cancel: cancel}, nil
}

func (b *Backend) WaitStep(ctx context.Context, step *types.Step, task string) (*types.State, error) {
	_, s, err := b.step(task, step)
	if err != nil {
		return nil, err
	}
	select {
	case <-s.startDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.process == nil {
		return nil, fmt.Errorf("step process unavailable")
	}
	select {
	case <-s.process.done:
		return &types.State{Exited: true, ExitCode: s.process.code}, s.process.err
	case <-ctx.Done():
		s.cancel()
		return nil, ctx.Err()
	}
}

func (b *Backend) DestroyStep(ctx context.Context, step *types.Step, task string) error {
	w, s, err := b.step(task, step)
	if s == nil {
		return nil
	} // safe before setup/start and after cleanup
	if err != nil {
		return err
	}
	return b.destroyStep(ctx, w, s)
}

func (b *Backend) destroyStep(ctx context.Context, w *workflow, s *stepState) error {
	s.cancel()
	select {
	case <-s.startDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.destroyMu.Lock()
	defer s.destroyMu.Unlock()
	w.mu.Lock()
	cleaned := s.cleaned
	w.mu.Unlock()
	if cleaned {
		return nil
	}
	if err := b.removeContainer(ctx, w, s.name); err != nil {
		return err
	}
	if s.process != nil {
		select {
		case <-s.process.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s.ip = ""
	if err := b.writeHostsLocked(w); err != nil {
		return err
	}
	s.cleaned = true
	return nil
}

func (b *Backend) DestroyWorkflow(ctx context.Context, _ *types.Config, task string) error {
	w, err := b.getWorkflow(task)
	if err != nil {
		return nil
	}
	select {
	case <-w.setupDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	w.destroyMu.Lock()
	defer w.destroyMu.Unlock()
	w.mu.Lock()
	w.closing = true
	steps := make([]*stepState, 0, len(w.steps))
	for _, s := range w.steps {
		s.cancel()
		steps = append(steps, s)
	}
	w.mu.Unlock()
	var errs []error
	for _, s := range steps {
		if err = b.destroyStep(ctx, w, s); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err = b.removeContainer(ctx, w, w.dnsName); err != nil {
		return err
	}
	if err = b.removeNetwork(ctx, w); err != nil {
		return err
	}
	if err = os.RemoveAll(w.dir); err != nil {
		return err
	}
	b.mu.Lock()
	if b.workflows[task] == w {
		delete(b.workflows, task)
	}
	b.mu.Unlock()
	return nil
}

type containerInfo struct {
	ID            string `json:"id"`
	Configuration struct {
		Labels map[string]string `json:"labels"`
	} `json:"configuration"`
	Status struct {
		State       string           `json:"state"`
		StartedDate *json.RawMessage `json:"startedDate"`
		Networks    []struct {
			Network     string `json:"network"`
			IPv4Address string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
}

func (c *containerInfo) IP(network string) string {
	for _, n := range c.Status.Networks {
		if n.Network == network {
			ip, _, _ := strings.Cut(n.IPv4Address, "/")
			return ip
		}
	}
	return ""
}

func (b *Backend) container(ctx context.Context, name string) (*containerInfo, error) {
	out, err := b.runner.run(ctx, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var infos []containerInfo
	if err = json.Unmarshal(out, &infos); err != nil {
		return nil, fmt.Errorf("invalid container list JSON: %w", err)
	}
	for _, info := range infos {
		if info.ID == name {
			return &info, nil
		}
	}
	return nil, nil
}

func (b *Backend) removeContainer(ctx context.Context, w *workflow, name string) error {
	info, err := b.container(ctx, name)
	if err != nil || info == nil {
		return err
	}
	if info.Configuration.Labels[ownerLabel] != w.owner {
		return fmt.Errorf("refusing to remove a container with different ownership")
	}
	if info.Status.State == "running" {
		_, _ = b.runner.run(ctx, "stop", "--time", "5", name)
	}
	if _, err = b.runner.run(ctx, "delete", "--force", name); err != nil {
		info, checkErr := b.container(ctx, name)
		if checkErr != nil || info != nil {
			return errors.Join(err, checkErr)
		}
	}
	return nil
}

func (b *Backend) removeNetwork(ctx context.Context, w *workflow) error {
	out, err := b.runner.run(ctx, "network", "list", "--format", "json")
	if err != nil {
		return err
	}
	var networks []struct {
		ID            string `json:"id"`
		Configuration struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"configuration"`
	}
	if err = json.Unmarshal(out, &networks); err != nil {
		return fmt.Errorf("invalid network list JSON: %w", err)
	}
	for _, n := range networks {
		if n.ID == w.network || n.Configuration.Name == w.network {
			if n.Configuration.Labels[ownerLabel] != w.owner {
				return fmt.Errorf("refusing to remove a network with different ownership")
			}
			_, err = b.runner.run(ctx, "network", "delete", w.network)
			return err
		}
	}
	return nil
}
