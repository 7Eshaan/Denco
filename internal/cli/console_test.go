package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

// Double-clicked (Windows, a console of its own) with no arguments, Denco
// opens the dashboard instead of flashing its usage; when that fails the
// window waits for Enter so the message can be read.
func TestDoubleClickOpensDashboard(t *testing.T) {
	t.Setenv("DENCO_HOME", t.TempDir())
	oldDC, oldIn, oldNI := doubleClicked, consoleIn, newInstaller
	defer func() { doubleClicked, consoleIn, newInstaller = oldDC, oldIn, oldNI }()
	doubleClicked = func() bool { return true }
	// Already installed: no setup offer (TestDoubleClickOffersSetup covers it).
	inst, _, _ := testInstaller(t, runtime.GOOS)
	inst.exe = inst.dest()
	newInstaller = func(*env) (*installer, error) { return inst, nil }
	in := strings.NewReader("\n")
	consoleIn = in

	var out, errOut bytes.Buffer
	// Under `go test` stdout isn't a console, so the dashboard refuses —
	// which shows the failure path.
	code := Main(nil, &out, &errOut)
	if code == 0 {
		t.Fatal("the dashboard started without a terminal?")
	}
	s := errOut.String()
	if !strings.Contains(s, "terminal") || !strings.Contains(s, "Press Enter to close this window") || strings.Contains(s, "A daemon-based") {
		t.Fatalf("stderr:\n%s", s)
	}
	if in.Len() != 0 {
		t.Fatal("it didn't wait for Enter")
	}

	// From a shell: the usage, as before, and no waiting.
	doubleClicked = func() bool { return false }
	errOut.Reset()
	if code := Main(nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "A daemon-based") || strings.Contains(errOut.String(), "Press Enter") {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut.String())
	}
	// Arguments always run the command, double-clicked or not.
	doubleClicked = func() bool { return true }
	errOut.Reset()
	if code := Main([]string{"version"}, &out, &errOut); code != 0 || strings.Contains(errOut.String(), "Press Enter") {
		t.Fatalf("version: exit %d, %s", code, errOut.String())
	}
}
