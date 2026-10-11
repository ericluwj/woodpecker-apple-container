package apple

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

// Mappings change only the registry host, never repository, tag or digest.
// Apple Container selects credentials for the resulting host; no origin
// credentials are read, copied or supplied by this backend. Cache failure is
// fail-closed: there is deliberately no implicit origin fallback.
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
