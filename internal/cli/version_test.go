package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v1.2.3"

	var out, errOut bytes.Buffer
	e := &env{home: t.TempDir(), stdout: &out, stderr: &errOut}
	for _, cmd := range []string{"version", "--version"} {
		out.Reset()
		if code := runEnv(e, []string{cmd}); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", cmd, code, errOut.String())
		}
		want := "denco v1.2.3 (" + runtime.GOOS + "/" + runtime.GOARCH
		if !strings.HasPrefix(out.String(), want) {
			t.Errorf("%s printed %q, want prefix %q", cmd, out.String(), want)
		}
	}
	if code := runEnv(e, []string{"version", "extra"}); code != 2 {
		t.Errorf("version with an argument: exit %d, want 2", code)
	}
}

func TestVersionWithoutStamp(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = ""
	if v := version(); v == "" {
		t.Error("version() is empty without a stamp")
	}
}
