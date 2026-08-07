package talos

import "testing"

func TestVersionFromImageID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://factory.talos.dev/image/b0f2a8b5/v1.13.7/nocloud-amd64.iso", "1.13.7"},
		{"https://factory.talos.dev/image/abc/v1.14.0-beta.1/metal-arm64.iso", "1.14.0-beta.1"},
		{"", ""},
		{"not-a-url", ""},
		{"https://example.com/no/version/here.iso", ""},
	}
	for _, c := range cases {
		if got := VersionFromImageID(c.in); got != c.want {
			t.Errorf("VersionFromImageID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVersionFromOSImage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Talos (v1.13.7)", "1.13.7"},
		{"Talos (v1.12.7)", "1.12.7"},
		{"Ubuntu 24.04.1 LTS", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := VersionFromOSImage(c.in); got != c.want {
			t.Errorf("VersionFromOSImage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVersionFromEnforcement(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"All nodes run Talos v1.12.7", "1.12.7"},
		{"all nodes run talos v1.13.7", "1.13.7"},
		{"Upgrading node t-x-ctp0 to v1.13.7", ""}, // mid-upgrade shape: no false positive
		{"", ""},
	}
	for _, c := range cases {
		if got := VersionFromEnforcement(c.in); got != c.want {
			t.Errorf("VersionFromEnforcement(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJoinVersions(t *testing.T) {
	if got := JoinVersions(nil); got != "" {
		t.Errorf("JoinVersions(nil) = %q, want empty", got)
	}
	one := map[string]struct{}{"1.13.7": {}}
	if got := JoinVersions(one); got != "1.13.7" {
		t.Errorf("JoinVersions(one) = %q", got)
	}
	mixed := map[string]struct{}{"1.13.7": {}, "1.13.6": {}}
	if got := JoinVersions(mixed); got != "1.13.6,1.13.7" {
		t.Errorf("JoinVersions(mixed) = %q", got)
	}
}
