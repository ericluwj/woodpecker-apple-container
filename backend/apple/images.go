package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

// Mappings change only the registry host, never repository, tag or digest.
// Apple Container selects credentials for the resulting host; no origin
// credentials are read, copied or supplied by this backend.
func parseRegistryMirrors(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var values map[string]string
	if json.Unmarshal([]byte(raw), &values) != nil || values == nil {
		return nil, fmt.Errorf("registry mirrors must be a JSON object of registry hosts")
	}
	mirrors := make(map[string]string, len(values))
	for source, target := range values {
		source, target = strings.ToLower(source), strings.ToLower(target)
		if !validRegistryHost(source) || !validRegistryHost(target) {
			// Configuration may mistakenly contain credentials. Never echo it.
			return nil, fmt.Errorf("registry mirrors require host[:port] values without schemes, credentials or paths")
		}
		if source == "index.docker.io" {
			source = "docker.io"
		}
		if _, exists := mirrors[source]; exists {
			return nil, fmt.Errorf("registry mirrors contain duplicate normalized source hosts")
		}
		mirrors[source] = target
	}
	return mirrors, nil
}

func validRegistryHost(host string) bool {
	if host == "" || strings.ContainsAny(host, "/@?# \t\r\n\x00") {
		return false
	}
	named, err := reference.ParseNormalizedNamed(host + "/mirror-probe")
	return err == nil && (reference.Domain(named) == host || host == "index.docker.io")
}

func (b *Backend) resolveImage(image string) (string, error) {
	if len(b.config.registryMirrors) == 0 {
		return image, nil // Preserve the pre-migration CLI behavior exactly.
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid image reference for registry cache resolution")
	}
	domain := reference.Domain(named)
	mirror, mapped := b.config.registryMirrors[domain]
	if !mapped {
		return image, nil
	}
	// ParseNormalizedNamed also expands alpine -> docker.io/library/alpine.
	return mirror + strings.TrimPrefix(named.String(), domain), nil
}

// Resolve and fetch images before launching any workload. This limits fallback
// to image acquisition failures, so a failed job can never be executed twice.
func (b *Backend) prepareImage(ctx context.Context, image string, forcePull bool) (string, error) {
	resolved, err := b.resolveImage(image)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if resolved == image {
		if forcePull {
			err = b.pullImage(ctx, resolved)
		}
		return resolved, err
	}
	// Fully qualify short Docker Hub names so Apple's default registry cannot
	// redirect the origin attempt to a different host.
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid image reference for registry fallback")
	}
	origin := named.String()
	if !forcePull && b.cachedImage(ctx, resolved) {
		return resolved, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mirrorErr := b.pullImage(ctx, resolved)
	if mirrorErr == nil {
		return resolved, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if resolved == origin || errors.Is(mirrorErr, context.Canceled) || errors.Is(mirrorErr, context.DeadlineExceeded) {
		return "", mirrorErr
	}
	if !forcePull && b.cachedImage(ctx, origin) {
		return origin, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := b.pullImage(ctx, origin); err != nil {
		return "", errors.Join(fmt.Errorf("mirror image pull failed: %w", mirrorErr), fmt.Errorf("origin image pull failed: %w", err))
	}
	return origin, nil
}

func (b *Backend) pullImage(ctx context.Context, image string) error {
	_, err := b.runner.run(ctx, "image", "pull", "--progress", "none", "--platform", "linux/arm64", image)
	return err
}

func (b *Backend) cachedImage(ctx context.Context, image string) bool {
	out, err := b.runner.run(ctx, "image", "inspect", image)
	if err != nil {
		return false
	}
	var images []struct {
		Variants []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"variants"`
	}
	if json.Unmarshal(out, &images) != nil {
		return false
	}
	for _, image := range images {
		for _, variant := range image.Variants {
			if variant.Platform.OS == "linux" && variant.Platform.Architecture == "arm64" {
				return true
			}
		}
	}
	return false
}
