package apple

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (b *Backend) setupDNS(ctx context.Context, w *workflow) error {
	corefile := `.:53 {
    errors
    hosts /config/hosts {
        ttl 1
        reload 1s
        no_reverse
        fallthrough
    }
    forward . /etc/resolv.conf
}
`
	if err := atomicWrite(filepath.Join(w.dir, "dns", "Corefile"), []byte(corefile)); err != nil {
		return err
	}
	if err := b.writeHostsLocked(w); err != nil {
		return err
	}
	args := []string{"run", "--progress", "none", "--platform", "linux/arm64", "--detach", "--name", w.dnsName,
		"--label", ownerLabel + "=" + w.owner, "--network", w.network, "--cpus", "1", "--memory", "256M",
		"--mount", "type=bind,source=" + filepath.Join(w.dir, "dns") + ",target=/config,readonly", dnsImage, "-conf", "/config/Corefile"}
	if _, err := b.runner.run(ctx, args...); err != nil {
		return fmt.Errorf("start workflow DNS helper: %w", err)
	}
	info, err := b.container(ctx, w.dnsName)
	if err != nil {
		return err
	}
	if info == nil || info.Configuration.Labels[ownerLabel] != w.owner || info.Status.State != "running" {
		return fmt.Errorf("workflow DNS helper is not running")
	}
	w.dnsIP = info.IP(w.network)
	if net.ParseIP(w.dnsIP) == nil {
		return fmt.Errorf("workflow DNS helper has no valid network address")
	}
	return b.resolve(ctx, w.dnsIP, "wp-dns-ready.invalid", "127.0.0.1")
}

func (b *Backend) writeHostsLocked(w *workflow) error {
	lines := []string{"127.0.0.1 wp-dns-ready.invalid"}
	for _, s := range w.steps {
		if s.ip == "" {
			continue
		}
		if net.ParseIP(s.ip) == nil {
			return fmt.Errorf("invalid workload IP address")
		}
		for _, alias := range s.aliases {
			lines = append(lines, s.ip+" "+strings.ToLower(alias))
		}
	}
	sort.Strings(lines)
	return atomicWrite(filepath.Join(w.dir, "dns", "hosts"), []byte(strings.Join(lines, "\n")+"\n"))
}

func waitDNS(ctx context.Context, server, alias, want string) error {
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(server, "53"))
	}}
	for {
		queryCtx, cancel := context.WithTimeout(ctx, time.Second)
		ips, err := resolver.LookupIP(queryCtx, "ip4", alias+".")
		cancel()
		if err == nil {
			for _, ip := range ips {
				if ip.String() == want {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("workflow DNS alias %q is unavailable: %w", alias, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
