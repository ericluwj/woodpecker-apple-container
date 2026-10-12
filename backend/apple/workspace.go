package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"time"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

// ext4 has a single VM writer. Never change service or parallel sharing semantics.
func serialWorkspace(conf *types.Config) bool {
	steps := 0
	for _, stage := range conf.Stages {
		if len(stage.Steps) > 1 {
			return false
		}
		for _, s := range stage.Steps {
			if s.Detached || s.Type == types.StepTypeService {
				return false
			}
			steps++
		}
	}
	return steps > 0
}

func workspaceMount(w *workflow, target string) string {
	if w.workspaceVolume != "" {
		return "type=volume,source=" + w.workspaceVolume + ",target=" + target
	}
	return "type=bind,source=" + w.workspace + ",target=" + target
}

// Plugin workdirs must exist before their entrypoint starts. This helper mounts
// only the owned volume, never host directories; workload-created symlinks cannot
// redirect mkdir into the macOS filesystem. All arguments remain argv elements.
func (b *Backend) initVolumeWorkdir(ctx context.Context, w *workflow, rel string) error {
	ctx, cancel := context.WithTimeout(ctx, b.config.startupTimeout)
	defer cancel()
	image, err := b.prepareImage(ctx, "alpine:3.22", false)
	if err != nil {
		return err
	}
	_, err = b.runner.run(ctx, "run", "--rm", "--progress", "none", "--name", w.workspaceInit,
		"--label", ownerLabel+"="+w.owner, "--network", "none", "--cpus", "1", "--memory", "256M",
		"--mount", workspaceMount(w, "/workspace"), "--entrypoint", "/bin/mkdir", image, "-p", "--", path.Join("/workspace", rel))
	// On interruption a helper can outlive its CLI; retain the ownership manifest
	// and let workflow cleanup retry instead of deleting a potentially mounted volume.
	return err
}

func (b *Backend) removeWorkspaceVolume(ctx context.Context, w *workflow) error {
	out, err := b.runner.run(ctx, "volume", "list", "--format", "json")
	if err != nil {
		return err
	}
	var volumes []struct {
		ID            string `json:"id"`
		Configuration struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(out, &volumes); err != nil {
		return fmt.Errorf("invalid volume list JSON")
	}
	for _, v := range volumes {
		if v.ID == w.workspaceVolume || v.Configuration.Name == w.workspaceVolume {
			if v.Configuration.Labels[ownerLabel] != w.owner {
				return fmt.Errorf("workspace volume ownership mismatch")
			}
			cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, err = b.runner.run(cleanupCtx, "volume", "delete", w.workspaceVolume)
			return err
		}
	}
	return nil
}
