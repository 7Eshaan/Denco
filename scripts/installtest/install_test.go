// Package installtest tests scripts/install.sh and scripts/install.ps1
// (DECISIONS D67) against a fake GitHub release served on loopback: the
// "latest" redirect, the archive and SHA256SUMS, with a fake denco that
// only records how it was run. Nothing is installed and no network
// beyond loopback is used.
package installtest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const version = "v9.9.9"

// fakeDenco builds the stand-in binary once.
var fakeDenco = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "fakedenco-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "denco")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakedenco")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the fake denco: %v\n%s", err, b)
	}
	return os.ReadFile(out)
})

// release is a fake GitHub release of the fake denco for this machine.
type release struct {
	srv      *httptest.Server
	asset    string
	data     []byte
	sums     string
	requests []string
	mu       sync.Mutex
}

func newRelease(t *testing.T) *release {
	t.Helper()
	bin, err := fakeDenco()
	if err != nil {
		t.Skip(err) // no Go toolchain to build it with
	}
	name := fmt.Sprintf("denco_%s_%s_%s", version, runtime.GOOS, runtime.GOARCH)
	r := &release{}
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		r.asset = name + ".zip"
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(name + "/denco.exe")
		w.Write(bin)
		zw.Close()
	} else {
		r.asset = name + ".tar.gz"
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: name + "/denco", Mode: 0o755, Size: int64(len(bin))})
		tw.Write(bin)
		tw.Close()
		gz.Close()
	}
	r.data = buf.Bytes()
	r.sums = fmt.Sprintf("%x  %s\n%x  some_other_file.zip\n", sha256.Sum256(r.data), r.asset, sha256.Sum256(nil))
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, req.URL.Path)
		data, sums := r.data, r.sums
		r.mu.Unlock()
		switch req.URL.Path {
		case "/releases/latest":
			http.Redirect(w, req, "/releases/tag/"+version, http.StatusFound)
		case "/releases/tag/" + version:
			w.Write([]byte("<html>release page</html>"))
		case "/releases/download/" + version + "/" + r.asset:
			w.Write(data)
		case "/releases/download/" + version + "/SHA256SUMS":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

type result struct {
	code    int
	out     string
	ran     string // the fake denco's arguments; "" if it didn't run
	leftTmp []os.DirEntry
}

// run runs a script command with a temporary HOME/TMPDIR/LOCALAPPDATA.
func (r *release) run(t *testing.T, name string, args []string, env ...string) result {
	t.Helper()
	tmp := t.TempDir()
	scratch := filepath.Join(tmp, "tmp")
	os.MkdirAll(scratch, 0o755)
	log := filepath.Join(tmp, "ran")
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "HOME="+tmp, "USERPROFILE="+tmp, "TMPDIR="+scratch, "TEMP="+scratch, "TMP="+scratch,
		"LOCALAPPDATA="+filepath.Join(tmp, "local"), "DENCO_REPO_URL="+r.srv.URL, "DENCO_FAKE_LOG="+log, "DENCO_VERSION=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	res := result{out: string(out)}
	if ee, ok := err.(*exec.ExitError); ok {
		res.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", name, err)
	}
	if b, err := os.ReadFile(log); err == nil {
		res.ran = string(b)
	}
	res.leftTmp, _ = os.ReadDir(scratch)
	return res
}

// setSums replaces the SHA256SUMS the server sends.
func (r *release) setSums(f func(string) string) {
	r.mu.Lock()
	r.sums = f(r.sums)
	r.mu.Unlock()
}

func (r *release) asked(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.requests {
		if p == path {
			return true
		}
	}
	return false
}

func shells(t *testing.T) []string {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	var out []string
	for _, sh := range []string{"sh", "dash", "bash"} { // dash: Debian/Ubuntu's /bin/sh
		if _, err := exec.LookPath(sh); err == nil {
			out = append(out, sh)
		}
	}
	return out
}

// locales the scripts run under: CI's macOS runner uses UTF-8, where
// macOS's sh once read "$arch" plus an ellipsis as one variable name.
var locales = []string{"C", "en_US.UTF-8"}

func TestInstallSh(t *testing.T) {
	for _, sh := range shells(t) {
		for _, loc := range locales {
			t.Run(sh+"/"+loc, func(t *testing.T) {
				r := newRelease(t)
				res := r.run(t, sh, []string{"../install.sh"}, "LC_ALL="+loc)
				if res.code != 0 || res.ran != "setup" || !strings.Contains(res.out, "Checksum OK") {
					t.Fatalf("exit %d, denco ran with %q:\n%s", res.code, res.ran, res.out)
				}
				if !r.asked("/releases/latest") || !strings.Contains(res.out, version) {
					t.Errorf("the latest version wasn't looked up: %v", r.requests)
				}
				if len(res.leftTmp) != 0 {
					t.Errorf("left in the temp folder: %v", res.leftTmp)
				}
			})
		}
	}
}

// The scripts are plain ASCII (see locales).
func TestScriptsAreASCII(t *testing.T) {
	for _, f := range []string{"../install.sh", "../install.ps1"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range b {
			if c >= 0x80 {
				line := 1 + bytes.Count(b[:i], []byte("\n"))
				t.Errorf("%s:%d: a non-ASCII byte", f, line)
				break
			}
		}
	}
}

// The script as users run it: piped into sh.
func TestInstallShPiped(t *testing.T) {
	shells(t)
	r := newRelease(t)
	script, _ := os.ReadFile("../install.sh")
	tmp := t.TempDir()
	cmd := exec.Command("sh")
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+tmp, "TMPDIR="+tmp, "DENCO_REPO_URL="+r.srv.URL,
		"DENCO_FAKE_LOG="+filepath.Join(tmp, "ran"), "DENCO_VERSION=")
	out, err := cmd.CombinedOutput()
	if b, _ := os.ReadFile(filepath.Join(tmp, "ran")); err != nil || string(b) != "setup" {
		t.Fatalf("%v, ran %q:\n%s", err, b, out)
	}
}

func TestInstallShRefuses(t *testing.T) {
	shells(t)
	for _, loc := range locales {
		t.Run(loc, func(t *testing.T) { installShRefuses(t, "LC_ALL="+loc) })
	}
}

func installShRefuses(t *testing.T, locale string) {
	t.Run("checksum mismatch", func(t *testing.T) {
		r := newRelease(t)
		r.setSums(func(s string) string { return "00000000" + s[8:] })
		res := r.run(t, "sh", []string{"../install.sh"}, locale)
		if res.code == 0 || res.ran != "" || !strings.Contains(res.out, "checksum mismatch") {
			t.Fatalf("exit %d, ran %q:\n%s", res.code, res.ran, res.out)
		}
		if len(res.leftTmp) != 0 {
			t.Errorf("left in the temp folder: %v", res.leftTmp)
		}
	})
	t.Run("not in SHA256SUMS", func(t *testing.T) {
		r := newRelease(t)
		r.setSums(func(string) string { return "abc  other.tar.gz\n" })
		res := r.run(t, "sh", []string{"../install.sh"}, locale)
		if res.code == 0 || res.ran != "" || !strings.Contains(res.out, "isn't listed") {
			t.Fatalf("exit %d, ran %q:\n%s", res.code, res.ran, res.out)
		}
	})
	t.Run("setup fails", func(t *testing.T) {
		r := newRelease(t)
		res := r.run(t, "sh", []string{"../install.sh"}, locale, "DENCO_FAKE_EXIT=2")
		if res.code == 0 || !strings.Contains(res.out, "denco setup failed") {
			t.Fatalf("exit %d:\n%s", res.code, res.out)
		}
	})
	t.Run("no such version", func(t *testing.T) {
		r := newRelease(t)
		res := r.run(t, "sh", []string{"../install.sh"}, locale, "DENCO_VERSION=v0.0.1")
		if res.code == 0 || res.ran != "" || !strings.Contains(res.out, "download failed") || r.asked("/releases/latest") {
			t.Fatalf("exit %d, ran %q:\n%s", res.code, res.ran, res.out)
		}
	})
}

// install.ps1 runs under every PowerShell found — Windows PowerShell 5.1
// and PowerShell 7 on Windows CI. The test process inherits CI's
// PSModulePath (PowerShell 7's), the condition that broke Get-FileHash
// in 5.1; the script now needs no module cmdlets for its real work.
func TestInstallPs1(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 is for Windows")
	}
	found := false
	for _, ps := range []string{"powershell", "pwsh"} {
		if _, err := exec.LookPath(ps); err != nil {
			continue
		}
		found = true
		t.Run(ps, func(t *testing.T) { installPs1(t, ps) })
	}
	if !found {
		t.Skip("no PowerShell")
	}
}

func installPs1(t *testing.T, ps string) {
	base := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}
	t.Run("file", func(t *testing.T) {
		r := newRelease(t)
		res := r.run(t, ps, append(base, "-File", `..\install.ps1`))
		if res.code != 0 || res.ran != "setup" || !strings.Contains(res.out, "Checksum OK") || !r.asked("/releases/latest") {
			t.Fatalf("exit %d, ran %q:\n%s", res.code, res.ran, res.out)
		}
		if len(res.leftTmp) != 0 {
			t.Errorf("left in the temp folder: %v", res.leftTmp)
		}
	})
	t.Run("piped into iex", func(t *testing.T) {
		r := newRelease(t)
		res := r.run(t, ps, append(base, "-Command", `Get-Content -Raw ..\install.ps1 | Invoke-Expression`))
		if res.code != 0 || res.ran != "setup" {
			t.Fatalf("exit %d, ran %q:\n%s", res.code, res.ran, res.out)
		}
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		r := newRelease(t)
		r.setSums(func(s string) string { return "00000000" + s[8:] })
		res := r.run(t, ps, append(base, "-File", `..\install.ps1`))
		if res.code == 0 || res.ran != "" || !strings.Contains(res.out, "checksum mismatch") {
			t.Fatalf("exit %d, ran %q:\n%s", res.code, res.ran, res.out)
		}
	})
	t.Run("setup fails", func(t *testing.T) {
		r := newRelease(t)
		res := r.run(t, ps, append(base, "-File", `..\install.ps1`), "DENCO_FAKE_EXIT=2")
		if res.code == 0 || !strings.Contains(res.out, "denco setup failed") {
			t.Fatalf("exit %d:\n%s", res.code, res.out)
		}
	})
}

// An unexpected error after the cleanup trap is set still exits non-zero.
// (macOS's bash 3.2 exits 0 after an unbound variable, $? included; CI
// reported such failures as "exit 0".)
func TestInstallShErrorsExitNonZero(t *testing.T) {
	for _, sh := range shells(t) {
		t.Run(sh, func(t *testing.T) {
			b, err := os.ReadFile("../install.sh")
			if err != nil {
				t.Fatal(err)
			}
			const after = "trap 'exit 1' INT TERM\n"
			if !bytes.Contains(b, []byte(after)) {
				t.Fatal("the trap line moved; update this test")
			}
			broken := bytes.Replace(b, []byte(after), []byte(after+": \"$no_such_variable\"\n"), 1)
			f := filepath.Join(t.TempDir(), "broken.sh")
			os.WriteFile(f, broken, 0o644)
			r := newRelease(t)
			res := r.run(t, sh, []string{f}, "LC_ALL=en_US.UTF-8")
			if res.code == 0 || res.ran != "" {
				t.Fatalf("exit %d, ran %q:\n%s", res.code, res.ran, res.out)
			}
		})
	}
}
