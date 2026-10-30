# Woodpecker agent for Apple containers

A native macOS Woodpecker agent that executes **Linux ARM64** CI workloads in
Apple container VMs. It embeds Woodpecker **3.20.1** and registers the backend
`apple-container` through the upstream custom-agent API.

Each step has its own VM. Steps within a workflow share one workspace and one
custom network. A CoreDNS helper provides workflow-local service names, so two
concurrent workflows can both use `postgres` without a name collision.

## Requirements and build

- Apple silicon Mac, macOS 26 or newer.
- Apple [`container`](https://github.com/apple/container/releases/tag/1.6.1)
  **1.6.1** (recommended). Legacy **1.5.0** remains accepted during upgrades.
  Other versions are rejected because the backend relies on CLI and JSON shapes.
- Go **1.27.0** or newer. Go's automatic toolchain download can supply 1.27.0
  when building with an older Go installation and `GOTOOLCHAIN=auto`.
- A Woodpecker **3.20.1** server accessible over gRPC.

The supported targets are Woodpecker **3.20.1** and Apple container **1.6.1**.
To upgrade an existing agent, drain its workflows, retain the prior binary and
configuration, then rebuild and restart the agent.
Keep the current Apple container runtime during the Woodpecker upgrade. Upgrade
the server and agent together to Woodpecker 3.20.1. No backend state migration or
global resource cleanup is required. For a Woodpecker rollback, drain again and
restore the prior custom agent binary while retaining Apple container 1.6.1
and the host configuration. Server database rollback requires its own
pre-upgrade snapshot; mixed-version operation has not been runtime-tested.

Woodpecker 3.20.1 restores the previous environment substitution behavior,
reverting the pipeline backslash handling change introduced in 3.20.0. Review
workflows adjusted specifically for 3.20.0 when upgrading. See the
[3.20.1 release notes](https://github.com/woodpecker-ci/woodpecker/releases/tag/v3.20.1).

```sh
make build
container system start
```

Start the container service as the same macOS user that runs the agent. Prepare
its kernel and complete any macOS Local Network permission prompts during
installation. The backend only checks the service; it never starts it, changes
global DNS, or installs anything automatically. Confirm the prerequisite with
`container run --rm alpine:3.22 echo ready`.

## Download a release

Published [GitHub Releases](https://github.com/ericluwj/woodpecker-apple-container/releases)
include `woodpecker-agent-apple-darwin-arm64.tar.gz` and `SHA256SUMS`. Download
both into the same directory, then verify and extract:

```sh
shasum -a 256 -c SHA256SUMS
tar -xzf woodpecker-agent-apple-darwin-arm64.tar.gz
./woodpecker-agent-apple --version
```

The archive contains the native macOS ARM64 agent, README and MIT license.
Go is needed only when building from source; the macOS, Apple container and
Woodpecker server requirements above still apply. The binary is not Developer ID
signed or notarized.

The **Publish macOS ARM64 agent** GitHub Actions workflow tests and builds on
macOS ARM64. It uses `GITHUB_TOKEN`; no registry credentials are required.
VM integration tests require a separately configured Apple container host and
are not run by this workflow.

## Versioning policy

Release names identify the exact embedded Woodpecker version and a backend
revision. Use a matching Woodpecker server unless another pairing has been
explicitly tested and documented. List Apple container compatibility separately
in the release requirements. Currently `--version` reports the embedded
Woodpecker version, rather than the backend revision.

| Release tag | Meaning |
| --- | --- |
| `3.19.0-1`, `3.19.0-2` | Immutable backend revisions for Woodpecker 3.19.0 |
| `3.19.0-latest` | Moving alias for the highest successful 3.19.0 revision |
| `3.20.0-1`, `3.20.0-2` | Immutable backend revisions for Woodpecker 3.20.0 |
| `3.20.0-latest` | Moving alias for the highest successful 3.20.0 revision |
| `3.20.1-1`, `3.20.1-2` | Immutable backend revisions for Woodpecker 3.20.1 |
| `3.20.1-latest` | Moving alias for the highest successful 3.20.1 revision |

Start revisions at 1 for each Woodpecker version and increment them for every
new backend release. The workflow checks that the release's Woodpecker version
matches `go.mod`. Numbered tags and their uploaded binaries are never replaced.
Existing numbered releases keep their original tags and assets.

After a numbered release has built, passed tests and published both assets, the
workflow updates that version's `-latest` Git tag to the same commit and copies
the immutable release's archive and checksum to the alias Release. An older
revision finishing late cannot move the alias backward. Each Woodpecker version
has its own alias; GitHub's repository-wide “Latest release” badge is separate.
Alias updates are not atomic across the tag and two files: retry downloads if a
checksum check fails during an update. Pin a numbered revision and verify its
checksum for reproducible deployments.

### Publishing

Run the workflow from `main` with `release_tag` set to a new numbered revision.
Set `source_ref` to the exact source commit or tag; leave it empty to build the
selected workflow ref. For example:

```sh
gh workflow run release.yaml --ref main \
  -f release_tag=3.20.1-1 -f source_ref=SOURCE_COMMIT
```

This also supports the 3.19.0 line: choose a source commit pinned to Woodpecker
3.19.0 and a tag such as `3.19.0-1`. Keep the publishing workflow on `main` when
building historical commits, since their original workflows may predate this
policy. Leave `release_tag` empty for a build-only run with downloadable Actions
artifacts and no Release changes.

Publishing a numbered GitHub Release also triggers a build using its tag when
that commit contains this workflow. Do not publish `-latest` manually: it is
managed by the workflow and its release events are skipped. Reruns can resume
partial uploads only if existing numbered assets are byte-for-byte identical;
otherwise use a new revision. A failed alias update can be retried without
replacing immutable assets.

Rolling download URLs keep the version line in the path, for example:

```text
https://github.com/ericluwj/woodpecker-apple-container/releases/download/3.20.1-latest/woodpecker-agent-apple-darwin-arm64.tar.gz
https://github.com/ericluwj/woodpecker-apple-container/releases/download/3.20.1-latest/SHA256SUMS
```

## Run the agent

```sh
export WOODPECKER_SERVER=ci.example.com:9000
export WOODPECKER_AGENT_SECRET_FILE=/absolute/path/to/agent-secret
export WOODPECKER_GRPC_SECURE=true
export WOODPECKER_GRPC_SKIP_VERIFY=false
export WOODPECKER_AGENT_CONFIG_FILE="$HOME/.config/woodpecker/apple-agent.conf"
export WOODPECKER_BACKEND=apple-container
export WOODPECKER_MAX_WORKFLOWS=2
mkdir -p "$HOME/.config/woodpecker"
./bin/woodpecker-agent-apple
```

Use an agent secret configured by your server administrator. Agent flags,
authentication, TLS, log masking, and scheduler behavior come from Woodpecker.
The default workload platform is `linux/arm64`, even though the agent runs on
macOS. Native macOS/Xcode builds require a different execution environment.

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--backend-apple-binary` | `WOODPECKER_BACKEND_APPLE_BINARY` | `container` on PATH |
| `--backend-apple-state-dir` | `WOODPECKER_BACKEND_APPLE_STATE_DIR` | `<os.UserCacheDir()>/woodpecker-apple` |
| `--backend-apple-cpus` | `WOODPECKER_BACKEND_APPLE_CPUS` | `2` |
| `--backend-apple-memory` | `WOODPECKER_BACKEND_APPLE_MEMORY` | `1G` (1 GiB) |
| `--backend-apple-service-cpus` | `WOODPECKER_BACKEND_APPLE_SERVICE_CPUS` | `0` (inherit workload CPUs) |
| `--backend-apple-service-memory` | `WOODPECKER_BACKEND_APPLE_SERVICE_MEMORY` | unset (inherit workload memory) |
| `--backend-apple-workspace` | `WOODPECKER_BACKEND_APPLE_WORKSPACE` | `bind` |
| `--backend-apple-startup-timeout` | `WOODPECKER_BACKEND_APPLE_STARTUP_TIMEOUT` | `5m` |
| `--backend-apple-registry-mirrors` | `WOODPECKER_BACKEND_APPLE_REGISTRY_MIRRORS` | unset (direct pulls) |

CPU and memory limits apply to **each step VM**, not to the entire workflow.
Service overrides apply only to Woodpecker `service` steps; clone, plugins and
commands (including detached commands) retain workload limits. Leaving both
overrides unset preserves the previous behavior. For an initial two-workflow
trial on a 16 GiB host, start with 4 CPUs / 4G for workloads and 1 CPU / 1G for
services, then measure actual memory pressure and job runtimes. Two workflows
with one active workload and two services each have a combined 12 GiB VM memory
limit, plus DNS helpers and macOS overhead; parallel workload steps add to this.
These are per-VM limits, not a host-wide admission or memory reservation system.
Increase `WOODPECKER_MAX_WORKFLOWS` only after validating representative load.
Each workflow also uses one `coredns/coredns:1.14.7` VM with 1 CPU and 256 MiB.

For a staged I/O trial, `WOODPECKER_BACKEND_APPLE_WORKSPACE=serial-volume`
uses a private 32 GiB sparse ext4 volume only when every stage has at most one
step and no step is a service or detached. Other workflows retain their shared
host bind mount. Previous step cleanup must complete before another step can
attach the volume; unexpected overlap is rejected rather than risking corruption.
Clone/plugin/command steps share that workflow's volume sequentially. Plugin
workdirs are prepared by an isolated Alpine helper. The ownership manifest records
the volume and helper; cleanup verifies labels and deletes only owned resources.
Failed cleanup retains the manifest for retry. Volumes are ephemeral workflow
data, not persistent dependency caches. Revert to `bind` only after draining the
agent; running workflows are never converted or copied. Services/parallel jobs
do not receive an I/O speedup from this mode.

The timeout covers workflow setup, image pulling, and individual step startup;
running steps use Woodpecker's workflow timeout. The state directory must have
mode `0700`; it is created privately when absent. Its path cannot contain comma,
colon, or newline characters because Apple's mount syntax uses those delimiters.

## Registry pull-through caches

Administrators can opt into registry-host mappings without editing workflows:

```sh
export WOODPECKER_BACKEND_APPLE_REGISTRY_MIRRORS='{"docker.io":"docker-cache.example.com","registry.k8s.io":"k8s-cache.example.com","gcr.io":"gcr-cache.example.com","ghcr.io":"ghcr-cache.example.com"}'
```

The value is a JSON object of source registry hosts to cache hosts, optionally
with ports. Schemes, URL paths and embedded credentials are rejected. Apple's
normal HTTPS transport is used; this option does not enable insecure HTTP/TLS.
The cache must expose the same repository paths as its source registry.

The backend maps short Docker Hub names (`alpine:3.22` becomes
`docker-cache.example.com/library/alpine:3.22`), namespaced names and
fully qualified references. Tags and digests are preserved. The same resolution
applies to forced pulls, cached-image startup, clone/plugin/service steps and
the backend's own CoreDNS helper. Workflows keep their logical image references;
unmapped registries and agents without this option keep their existing behavior.
This is separate from Apple Container's default registry and from Colima/Docker
configuration. It does not affect downloads or image builders inside a step VM.

Mappings apply to every repository on the source registry, including private
images. Apple Container looks up Keychain credentials separately for the mirror
or origin host being used; the backend does not copy credentials between hosts
or accept workflow registry credentials. Private-image Keychain access still
needs noninteractive proof.

If a mirror pull fails, the backend **automatically falls back to the original
registry once**, preserving the repository, tag and digest. For short Docker Hub
names, the origin is explicitly qualified as `docker.io/library/...`. Both
attempts share the existing startup timeout; cancellation or an exhausted timeout
stops acquisition without another attempt. Agents must be able to reach the
original registry for fallback to succeed.

Without `step.Pull`, cached Linux ARM64 mirror images are reused; otherwise the
backend pulls from the mirror before launching the VM. After a failed mirror
pull, an available origin image is reused or pulled if absent. With `step.Pull`,
each attempted source is pulled even when cached. Fallback applies to image
acquisition for workload and DNS helper containers, never to VM startup errors or
workload exit failures, so a failed job is not executed twice. Removing the
mapping restores direct-origin behavior after a drained agent reload, but does
not migrate cached images back to their original names. Preserve the prior binary,
service definition, agent identity and secret during rollout; no global prune is
needed. Test a candidate on one idle agent before rolling it out to the others.

## Workflow example

See [examples/basic.yaml](examples/basic.yaml) and
[examples/services.yaml](examples/services.yaml). Normal clone and plugin steps
use OCI images and their default entrypoints. Workspaces customized in YAML are
translated from Woodpecker's generated volume into the workflow's bind mount.

```yaml
labels:
  backend: apple-container

steps:
  - name: build
    image: alpine:3.22
    commands:
      - printf hello > artifact
  - name: verify
    image: alpine:3.22
    commands:
      - test "$(cat artifact)" = hello
```

Services are reachable by their names on the workflow network. DNS visibility
is checked during startup; applications may still need a readiness/retry loop.
External DNS queries are forwarded through the DNS helper's regular resolver.
Administrators can opt out of vmnet's DNS proxy with
`WOODPECKER_BACKEND_APPLE_DNS_SERVERS` (or `--backend-apple-dns-servers`),
a comma-separated list of resolver IP addresses.
Only the CoreDNS helper receives these upstream resolvers; workflow steps keep
using CoreDNS for service aliases. Choose resolvers that preserve your split
DNS zones and verify reachability from the helper network. Unset the option
to retain the default vmnet resolver. This does not change host DNS, TLS, or
application IP fallbacks. Existing workflows retain their current helper;
the setting applies when new workflows are created after an agent restart.
Parallel steps intentionally share a writable workspace, so the workflow must
coordinate conflicting writes.

### NetBird DNS

If the agent host uses NetBird for private DNS, you can configure the workflow
DNS helper to use its resolver:

```sh
export WOODPECKER_BACKEND_APPLE_DNS_SERVERS=100.84.255.254
```

Use the resolver address configured for your NetBird environment and verify that
it is reachable from the workflow network. The resolver must answer the private
zones and public names needed by your workloads. This setting only selects the
helper's upstream resolver; NetBird connectivity and routing must already be
configured on the host. Workflow service aliases continue to resolve through
CoreDNS.

## Compatibility and isolation

Supported: clone, command and OCI plugin steps, services/detached steps,
parallel steps, concurrent workflows, multiline environment values, image pulls,
exit statuses, live stdout/stderr, cancellation, and scoped cleanup.

Images must provide Linux ARM64 manifests. Public images and administrator
credentials configured using `container registry login` are supported.
Woodpecker-supplied registry credentials are rejected and never written into
the shared macOS Keychain. Image content remains cached after workflow cleanup.

The backend rejects privileged mode, devices, arbitrary host volumes, tmpfs,
external networks, host networking, published host ports, custom DNS, extra-host
overrides, and backend options. It accepts only the compiler-generated workspace
mount and workflow network. Step names and network aliases must be valid DNS
names; aliases are case-insensitive and must be unique among active steps.

Isolation is intended for CI workload separation with normal outbound access.
It is not a hostile multi-tenant boundary or an egress firewall. Workloads have
no runtime socket, SSH-agent forwarding, or mounts of the agent's configuration.
Apple's CLI does not expose reliable OOM information, so an exit code is reported
without guessing an OOM cause.

Environment values are passed as explicit CLI arguments so multiline values
retain their contents. Backend errors never print those arguments or values;
as with other host processes, local administrators can inspect process arguments.
Workload output passes through Woodpecker's normal masking. Local spool files
are private, contain raw workload output, and are removed on successful cleanup.

## Tests

```sh
make test
make integration
make server-integration
```

Unit tests use a fake CLI and run under the race detector. The integration suite
requires a running container service, downloads public images, creates VMs and
custom networks, and removes only its own resources. It covers clone → build →
plugin execution, shared files, fast exits and log draining, service names,
external DNS, concurrent workflows, cancellation, and startup timeouts.

The server test starts Woodpecker 3.20.1's real in-process gRPC server, scheduler,
and migrated SQLite store, with a mocked forge. It builds and launches the actual
agent executable and checks registration, platform selection, successful and
failed workloads, and final server-side workflow results. The extra `test` build
tag enables Woodpecker's upstream server test helpers.

To run a subset:

```sh
APPLE_CONTAINER_INTEGRATION=1 go test -race -tags=integration \
  -run TestIntegrationSharedWorkspaceAndFastLogs -v ./backend/apple
```

To exercise Docker Hub cache resolution in real Apple VMs without changing
global registry settings, supply the same opt-in mappings to the dedicated test:

```sh
APPLE_CONTAINER_INTEGRATION=1 \
APPLE_CONTAINER_TEST_REGISTRY_MIRRORS='{"docker.io":"docker-cache.example.com"}' \
go test -race -tags=integration -timeout=10m \
  -run '^TestIntegrationRegistryMirrors$' -v ./backend/apple
```

That test forces a mapped Alpine pull, uses the mapped DNS helper, and checks a
shared workspace across qualified and short image names. It creates/cleans only
its own workflow resources and retains images. Unit tests cover all four mappings,
tags/digests, configuration validation, unchanged direct behavior, credentials,
origin fallback, cache failure and cancellation. A successful real test can still
reuse cached blobs: correlate manifest/blob requests with registry logs to prove cache use,
and test approved public ARM64 images for other mapped registries before rollout.

To test origin fallback for the DNS helper, forced pulls and cached-image startup
against a deliberately failing local mirror:

```sh
APPLE_CONTAINER_INTEGRATION=1 go test -race -tags=integration -timeout=10m \
  -run '^TestIntegrationRegistryMirrorFallback$' -v ./backend/apple
```

## Cleanup and crash recovery

Normal teardown stops workloads with a five-second grace period, force-deletes
remaining step containers, reaps CLI processes, removes the DNS helper and custom
network, and finally removes the workflow directory. Repeated cleanup is safe.
Failed cleanup retains its ownership manifest and can be retried by workflow
teardown. A hard crash requires administrator recovery; there is no global prune
or automatic deletion of resources belonging to another agent.

Each workflow directory contains `ownership.json` with `owner`, `workflow`,
`network`, `dns`, and `containers` identifiers. Read that manifest after stopping
the agent. For **each listed container**, inspect its ownership before removal:

```sh
container inspect CONTAINER_ID
# Check configuration.labels["io.woodpecker.apple.owner"] equals manifest.owner.
container stop --time 5 CONTAINER_ID
container delete --force CONTAINER_ID
```

Include the manifest's `dns` container. After all owned containers are gone:

```sh
container network inspect NETWORK_ID
# Check configuration.labels["io.woodpecker.apple.owner"] equals manifest.owner.
container network delete NETWORK_ID
```

Then remove only that manifest's workflow directory. If the service is unavailable,
restore it before retrying cleanup. Existing stopped resources are handled without
assuming every CLI failure means “not found.”

## License

This project is licensed under the [MIT License](LICENSE).
