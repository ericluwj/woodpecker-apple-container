package apple

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDNSServers(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"100.84.255.254", []string{"100.84.255.254"}},
		{" 100.84.255.254, fd00::53,100.84.255.254 ", []string{"100.84.255.254", "fd00::53"}},
	} {
		got, err := parseDNSServers(tc.raw)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("parseDNSServers(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"dns.example.com", "100.84.255.254:53", "100.84.0.0/16", "100.84.255.254,", "127.0.0.1", "::1", "0.0.0.0", "224.0.0.1", "100.84.255.254\n--privileged"} {
		if _, err := parseDNSServers(raw); err == nil {
			t.Errorf("accepted invalid DNS configuration %q", raw)
		}
	}
}

func TestDNSUpstreamOnlyChangesHelperResolver(t *testing.T) {
	for i, raw := range []string{"", "100.84.255.254,fd00::53"} {
		t.Run([]string{"default", "explicit"}[i], func(t *testing.T) {
			b, f := testBackend(t)
			b.config.dnsServers, _ = parseDNSServers(raw)
			s := testStep("build", "alpine:3.22")
			if err := b.SetupWorkflow(t.Context(), testConf(s), "task"); err != nil {
				t.Fatal(err)
			}
			if err := b.StartStep(t.Context(), s, "task"); err != nil {
				t.Fatal(err)
			}
			var helperDNS, stepDNS []string
			for _, call := range f.calls {
				if call[0] != "run" {
					continue
				}
				var values []string
				for i, arg := range call[:len(call)-1] {
					if arg == "--dns" {
						values = append(values, call[i+1])
					}
				}
				if contains(call, "--detach") {
					helperDNS = values
				} else {
					stepDNS = values
				}
			}
			if !reflect.DeepEqual(helperDNS, b.config.dnsServers) {
				t.Fatalf("helper DNS = %v; want %v", helperDNS, b.config.dnsServers)
			}
			w := b.workflows["task"]
			if !reflect.DeepEqual(stepDNS, []string{w.dnsIP}) {
				t.Fatalf("step bypasses service DNS: %v", stepDNS)
			}
			corefile, err := os.ReadFile(filepath.Join(w.dir, "dns", "Corefile"))
			if err != nil || !strings.Contains(string(corefile), "hosts /config/hosts") || !strings.Contains(string(corefile), "forward . /etc/resolv.conf") {
				t.Fatalf("workflow aliases or forwarding lost: %s, %v", corefile, err)
			}
		})
	}
}
