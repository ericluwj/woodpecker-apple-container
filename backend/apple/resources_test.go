package apple

import (
	"context"
	"testing"

	"github.com/urfave/cli/v3"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

func TestServiceResources(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		kind                 types.StepType
		detached             bool
		serviceCPUs          int
		serviceMemory        string
		wantCPUs, wantMemory string
	}{
		{"legacy service", types.StepTypeService, true, 0, "", "8", "8G"},
		{"service overrides", types.StepTypeService, true, 1, "1G", "1", "1G"},
		{"service cpu only", types.StepTypeService, true, 2, "", "2", "8G"},
		{"service memory only", types.StepTypeService, true, 0, "512M", "8", "512M"},
		{"commands unchanged", types.StepTypeCommands, false, 1, "1G", "8", "8G"},
		{"detached commands unchanged", types.StepTypeCommands, true, 1, "1G", "8", "8G"},
		{"clone unchanged", types.StepTypeClone, false, 1, "1G", "8", "8G"},
		{"plugin unchanged", types.StepTypePlugin, false, 1, "1G", "8", "8G"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := New()
			b.config.cpus, b.config.memory = 8, "8G"
			b.config.serviceCPUs, b.config.serviceMemory = tc.serviceCPUs, tc.serviceMemory
			s := testStep("step", "alpine")
			s.Type, s.Detached = tc.kind, tc.detached
			args, err := b.arguments(&workflow{workspace: "/private/workspace"}, &stepState{name: "step"}, s, "alpine")
			if err != nil {
				t.Fatal(err)
			}
			if option(args, "--cpus") != tc.wantCPUs || option(args, "--memory") != tc.wantMemory {
				t.Fatalf("wrong resources: %v", args)
			}
		})
	}
}

func TestResourceValidation(t *testing.T) {
	for _, tc := range []struct {
		cpus      int
		memory    string
		wantError bool
	}{
		{0, "", false}, {1, "1G", false}, {2, "512M", false},
		{-1, "1G", true}, {1, "0G", true}, {1, "-1G", true},
		{1, "1g", true}, {1, "1G --privileged", true},
	} {
		c := defaultConfig()
		c.serviceCPUs, c.serviceMemory = tc.cpus, tc.memory
		if got := c.validateResources(); (got != nil) != tc.wantError {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
	c := defaultConfig()
	c.cpus = 0
	if c.validateResources() == nil {
		t.Fatal("invalid workload CPU accepted")
	}
	c = defaultConfig()
	c.memory = "0G"
	if c.validateResources() == nil {
		t.Fatal("invalid workload memory accepted")
	}
}

func TestServiceResourceFlags(t *testing.T) {
	t.Setenv("WOODPECKER_BACKEND_APPLE_SERVICE_CPUS", "1")
	t.Setenv("WOODPECKER_BACKEND_APPLE_SERVICE_MEMORY", "1G")
	cmd := &cli.Command{Flags: New().Flags(), Action: func(ctx context.Context, cmd *cli.Command) error {
		c := configFromContext(context.WithValue(ctx, types.CliCommand, cmd))
		if c.serviceCPUs != 1 || c.serviceMemory != "1G" {
			t.Fatalf("flags not loaded: %+v", c)
		}
		return c.validateResources()
	}}
	if err := cmd.Run(context.Background(), []string{"agent"}); err != nil {
		t.Fatal(err)
	}
}
