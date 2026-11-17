package version

import "testing"

func stamp(t *testing.T, tag string) {
	t.Helper()
	old := Tag
	t.Cleanup(func() { Tag = old })
	Tag = tag
}

func TestStamped(t *testing.T) {
	stamp(t, "v0.1.0")
	checks := map[string][2]string{
		"String":       {String(), "v0.1.0"},
		"Short":        {Short(), "0.1.0"},
		"Client":       {Client(), "Denco 0.1.0"},
		"UserAgent":    {UserAgent(), "Denco/0.1.0"},
		"PeerIDDigits": {PeerIDDigits(), "0100"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
}

func TestUnstamped(t *testing.T) {
	stamp(t, "")
	if String() == "" || Short() == "" {
		t.Fatal("version is empty without a stamp")
	}
	if d := PeerIDDigits(); len(d) != 4 {
		t.Fatalf("PeerIDDigits() = %q, want 4 characters", d)
	}
}

func TestPeerIDDigits(t *testing.T) {
	tests := map[string]string{
		"v0.1.0":                               "0100",
		"v1.2.3":                               "1230",
		"v0.2.0-rc1":                           "0200",
		"v2.10.35":                             "2AZ0",
		"v0.1.1-0.20261117120000-abcdef123456": "0110", // Go pseudo-version
		"v40.0.0":                              "Z000",
		"dev":                                  "0000",
		"v1.2":                                 "0000",
		"1.2.3":                                "0000",
		"vx.y.z":                               "0000",
	}
	for in, want := range tests {
		if got := peerIDDigits(in); got != want {
			t.Errorf("peerIDDigits(%q) = %q, want %q", in, got, want)
		}
	}
}
