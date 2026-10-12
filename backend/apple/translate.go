package apple

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/common"
	"go.woodpecker-ci.org/woodpecker/v3/pipeline/backend/types"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var dnsName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func validAlias(alias string) bool {
	if len(alias) > 253 || !dnsName.MatchString(alias) || strings.EqualFold(alias, "wp-dns-ready.invalid") {
		return false
	}
	for _, label := range strings.Split(alias, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}

func validateStep(s *types.Step, conf *types.Config) error {
	if s == nil || s.UUID == "" || s.Image == "" || strings.HasPrefix(s.Image, "-") || strings.ContainsAny(s.Image, "\x00\r\n") {
		return fmt.Errorf("step requires a UUID and valid OCI image")
	}
	if s.Privileged || len(s.Devices) > 0 || len(s.ExtraHosts) > 0 || s.NetworkMode != "" || len(s.Ports) > 0 || len(s.DNS) > 0 || len(s.DNSSearch) > 0 || len(s.Tmpfs) > 0 || len(s.BackendOptions) > 0 {
		return fmt.Errorf("step %q requests unsupported privileged, mount, network, or backend settings", s.Name)
	}
	if s.AuthConfig.Username != "" || s.AuthConfig.Password != "" {
		return fmt.Errorf("step %q supplies registry credentials; use administrator-preauthenticated registries", s.Name)
	}
	if !path.IsAbs(s.WorkspaceBase) || path.Clean(s.WorkspaceBase) == "/" || strings.ContainsAny(s.WorkspaceBase, ",:\x00\n\r") {
		return fmt.Errorf("step %q has an invalid workspace base", s.Name)
	}
	if len(s.Volumes) != 1 || s.Volumes[0] != conf.Volume+":"+s.WorkspaceBase {
		return fmt.Errorf("step %q must mount only its generated workspace volume", s.Name)
	}
	for _, n := range s.Networks {
		if n.Name != conf.Network {
			return fmt.Errorf("step %q requests an external network", s.Name)
		}
		for _, alias := range n.Aliases {
			if !validAlias(alias) {
				return fmt.Errorf("step %q has an invalid network alias", s.Name)
			}
		}
	}
	if !validAlias(s.Name) {
		return fmt.Errorf("step name must be a valid DNS alias")
	}
	if s.WorkingDir != "" && (!path.IsAbs(s.WorkingDir) || strings.ContainsAny(s.WorkingDir, "\x00\r\n")) {
		return fmt.Errorf("invalid working directory")
	}
	for key, value := range s.Environment {
		if !envName.MatchString(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid environment variable name or NUL value")
		}
	}
	for _, value := range s.Entrypoint {
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid entrypoint")
		}
	}
	if len(s.Entrypoint) > 0 && s.Entrypoint[0] == "" {
		return fmt.Errorf("empty entrypoint executable is unsupported")
	}
	return nil
}

func (b *Backend) arguments(w *workflow, st *stepState, s *types.Step, image string) ([]string, error) {
	args := []string{"run", "--progress", "none", "--platform", "linux/arm64", "--name", st.name,
		"--label", ownerLabel + "=" + w.owner, "--label", "io.woodpecker.apple.workflow=" + w.task,
		"--cpus", fmt.Sprint(b.config.cpus), "--memory", b.config.memory,
		"--network", w.network, "--dns", w.dnsIP,
		"--mount", "type=bind,source=" + w.workspace + ",target=" + s.WorkspaceBase}
	env := make(map[string]string, len(s.Environment)+2)
	for key, value := range s.Environment {
		env[key] = value
	}
	entry, workingDir := s.Entrypoint, s.WorkingDir
	if len(s.Commands) > 0 {
		generated, generatedEntry, err := common.GenerateContainerConf(s.Commands, "linux", s.WorkingDir)
		if err != nil {
			return nil, err
		}
		for key, value := range generated {
			env[key] = value
		}
		if len(entry) == 0 {
			entry = generatedEntry
		}
		workingDir = s.WorkspaceBase
	}
	if workingDir != "" {
		args = append(args, "--workdir", workingDir)
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+env[key])
	}
	if len(entry) > 0 {
		args = append(args, "--entrypoint", entry[0])
	}
	args = append(args, image)
	if len(entry) > 1 {
		args = append(args, entry[1:]...)
	}
	return args, nil
}
