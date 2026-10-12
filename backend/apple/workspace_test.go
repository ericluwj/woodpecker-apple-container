package apple

import (
	"context"
	"strings"
	"testing"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

func serialConf(steps ...*types.Step) *types.Config {
	c := testConf()
	c.Stages = nil
	for _, s := range steps {
		c.Stages = append(c.Stages, &types.Stage{Steps: []*types.Step{s}})
	}
	return c
}

func TestSerialWorkspaceEligibility(t *testing.T) {
	a, b := testStep("a", "alpine"), testStep("b", "alpine")
	if !serialWorkspace(serialConf(a, b)) {
		t.Fatal("serial steps rejected")
	}
	if serialWorkspace(testConf(a, b)) || serialWorkspace(testConf()) {
		t.Fatal("parallel or empty accepted")
	}
	b.Detached = true
	if serialWorkspace(serialConf(a, b)) {
		t.Fatal("detached step accepted")
	}
	b.Detached = false
	b.Type = types.StepTypeService
	if serialWorkspace(serialConf(a, b)) {
		t.Fatal("service accepted")
	}
	if err := (config{cpus: 1, memory: "1G", workspaceMode: "unsafe"}).validateResources(); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestSerialWorkspaceLifecycle(t *testing.T) {
	b, f := testBackend(t)
	b.config.workspaceMode = "serial-volume"
	a, next := testStep("a", "long"), testStep("next", "alpine")
	a.Commands = []string{"sleep 30"}
	c := serialConf(a, next)
	if err := b.SetupWorkflow(t.Context(), c, "serial"); err != nil {
		t.Fatal(err)
	}
	w, _ := b.getWorkflow("serial")
	if w.workspaceVolume == "" || !strings.HasPrefix(workspaceMount(w, "/woodpecker"), "type=volume,") {
		t.Fatal("volume not selected")
	}
	if err := b.StartStep(t.Context(), a, "serial"); err != nil {
		t.Fatal(err)
	}
	if err := b.StartStep(t.Context(), next, "serial"); err == nil {
		t.Fatal("overlapping step accepted")
	}
	if err := b.DestroyStep(t.Context(), a, "serial"); err != nil {
		t.Fatal(err)
	}
	if err := b.DestroyWorkflow(context.Background(), nil, "serial"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.volumes) != 0 {
		t.Fatal("owned volume leaked")
	}
}

func TestSerialWorkspaceFallback(t *testing.T) {
	b, _ := testBackend(t)
	b.config.workspaceMode = "serial-volume"
	s := testStep("service", "alpine")
	s.Type = types.StepTypeService
	if err := b.SetupWorkflow(t.Context(), testConf(s), "service"); err != nil {
		t.Fatal(err)
	}
	w, _ := b.getWorkflow("service")
	if w.workspaceVolume != "" || !strings.HasPrefix(workspaceMount(w, "/woodpecker"), "type=bind,") {
		t.Fatal("sharing fallback not preserved")
	}
}

func TestWorkspaceVolumeOwnership(t *testing.T) {
	b, f := testBackend(t)
	b.config.workspaceMode = "serial-volume"
	if err := b.SetupWorkflow(t.Context(), testConf(testStep("a", "alpine")), "owner"); err != nil {
		t.Fatal(err)
	}
	w, _ := b.getWorkflow("owner")
	f.mu.Lock()
	f.volumes[w.workspaceVolume] = "other"
	f.mu.Unlock()
	if err := b.removeWorkspaceVolume(t.Context(), w); err == nil {
		t.Fatal("foreign volume accepted")
	}
	f.mu.Lock()
	f.volumes[w.workspaceVolume] = w.owner
	f.mu.Unlock()
}

func TestSerialWorkspaceSetupFailureCleanup(t *testing.T) {
	b, f := testBackend(t)
	b.config.workspaceMode = "serial-volume"
	f.failDNS = true
	if err := b.SetupWorkflow(t.Context(), testConf(testStep("a", "alpine")), "failure"); err == nil {
		t.Fatal("expected setup failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.volumes) != 0 {
		t.Fatal("volume leaked after partial setup")
	}
}
