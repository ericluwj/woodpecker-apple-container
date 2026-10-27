package apple

import "testing"

func TestSupportedCLIVersion(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"container CLI version 1.6.1 (build: release, commit: abc)", true},
		{"container CLI version 1.5.0 (build: release, commit: unspeci)\n", true},
		{"container CLI version 1.6.1\n", true},
		{"container CLI version 1.6.1-rc.1", false},
		{"container CLI version 1.6.10", false},
		{"container CLI version 1.4.0 (build: 1.6.1)", false},
		{"container CLI version 2.0.0", false},
		{"unrecognized output 1.6.1", false},
		{"", false},
	} {
		t.Run(tc.output, func(t *testing.T) {
			if got := supportedCLIVersion(tc.output); got != tc.want {
				t.Fatalf("supportedCLIVersion(%q) = %v; want %v", tc.output, got, tc.want)
			}
		})
	}
}
