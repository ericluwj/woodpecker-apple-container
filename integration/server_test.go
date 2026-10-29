//go:build integration && test

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"go.woodpecker-ci.org/woodpecker/v3/e2e/setup"
	forgeTypes "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
)

// Uses the pinned release's real gRPC server, scheduler, migrated SQLite store,
// and pipeline compiler. Only the forge is mocked; workloads run in real VMs.
func TestMatchingServerAndAgent(t *testing.T) {
	if os.Getenv("APPLE_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set APPLE_CONTAINER_INTEGRATION=1")
	}
	level := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.WarnLevel)
	defer zerolog.SetGlobalLevel(level)
	binary := filepath.Join(t.TempDir(), "woodpecker-agent-apple")
	build := exec.Command("go", "build", "-o", binary, "../cmd/woodpecker-agent-apple")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name, command string
		status        model.StatusValue
	}{
		{"success", "echo server-to-apple-workload; printf artifact > result", model.StatusSuccess},
		{"failure", "echo failing-workload; exit 23", model.StatusFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := []byte(fmt.Sprintf("skip_clone: true\nlabels:\n  backend: apple-container\nsteps:\n  - name: build\n    image: alpine:3.22\n    commands:\n      - %s\n", tc.command))
			env := setup.StartServer(t.Context(), t, []*forgeTypes.FileMeta{{Name: ".woodpecker.yaml", Data: yaml}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			root := t.TempDir()
			logPath := filepath.Join(root, "agent.log")
			logFile, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer logFile.Close()
			cmd := exec.CommandContext(ctx, binary, "--server", env.GRPCAddr, "--grpc-token", setup.TestAgentToken, "--agent-config", filepath.Join(root, "agent.conf"), "--hostname", "apple-integration", "--single-workflow", "--backend-engine", "apple-container", "--backend-apple-state-dir", filepath.Join(root, "state"))
			cmd.Stdout, cmd.Stderr = logFile, logFile
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("agent did not stop")
				}
			})
			deadline := time.Now().Add(30 * time.Second)
			var agentID int64
			for time.Now().Before(deadline) {
				agents, err := env.Store.AgentList(&model.ListOptionsWithAll{All: true})
				if err != nil {
					t.Fatal(err)
				}
				for _, agent := range agents {
					if agent.Backend == "apple-container" {
						if agent.Version != "3.20.0" {
							t.Fatalf("version %s", agent.Version)
						}
						if agent.Platform != "linux/arm64" {
							t.Fatalf("platform %s", agent.Platform)
						}
						agentID = agent.ID
					}
				}
				if agentID != 0 {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if agentID == 0 {
				data, _ := os.ReadFile(logPath)
				t.Fatalf("agent did not register: %s", data)
			}
			created, err := pipeline.Create(t.Context(), env.Store, env.Fixtures.Repo, env.DummyPipeline(model.EventPush))
			if err != nil {
				t.Fatal(err)
			}
			finished := setup.WaitForPipelineStatus(t, env.Store, created.ID, tc.status, 5*time.Minute)
			workflows, err := env.Store.WorkflowGetTree(finished)
			if err != nil {
				t.Fatal(err)
			}
			if len(workflows) != 1 || workflows[0].AgentID != agentID || workflows[0].State != tc.status {
				t.Fatalf("unexpected workflow result: %+v", workflows)
			}
			wantExit := 0
			if tc.status == model.StatusFailure {
				wantExit = 23
			}
			if len(workflows[0].Children) != 1 || workflows[0].Children[0].ExitCode != wantExit {
				t.Fatalf("unexpected step exit: %+v", workflows[0].Children)
			}
			select {
			case err := <-done:
				if err != nil {
					data, _ := os.ReadFile(logPath)
					t.Fatalf("agent exited: %v %s", err, data)
				}
				done <- nil
			case <-time.After(30 * time.Second):
				t.Fatal("single-workflow agent did not exit")
			}
			entries, err := os.ReadDir(filepath.Join(root, "state"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("workflow state remains: %v, %v", entries, err)
			}
		})
	}
}
