package installtest

// Tests for scripts/brew-formula.sh (DECISIONS D68), which writes the
// Homebrew formula from a release's SHA256SUMS. Nothing is downloaded or
// installed: the formula is only generated and, if Ruby is here, parsed.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// brewSums writes a SHA256SUMS like release-build.sh's for tag, with a
// distinct made-up checksum per archive.
func brewSums(t *testing.T, tag string, skip string) (path string, sums map[string]string) {
	t.Helper()
	sums = map[string]string{}
	var b strings.Builder
	for _, a := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64", "windows_arm64"} {
		ext := ".tar.gz"
		if strings.HasPrefix(a, "windows") {
			ext = ".zip"
		}
		name := fmt.Sprintf("denco_%s_%s%s", tag, a, ext)
		if name == skip {
			continue
		}
		sums[a] = fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
		fmt.Fprintf(&b, "%s  %s\n", sums[a], name)
	}
	path = filepath.Join(t.TempDir(), "SHA256SUMS")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, sums
}

func brewFormula(t *testing.T, sh string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(sh, append([]string{"../brew-formula.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "DENCO_REPO_URL=https://example.test/me/Denco", "LC_ALL=en_US.UTF-8")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func TestBrewFormula(t *testing.T) {
	for _, sh := range shells(t) {
		t.Run(sh, func(t *testing.T) {
			path, sums := brewSums(t, "v1.2.3", "")
			f, errOut, err := brewFormula(t, sh, "v1.2.3", path)
			if err != nil {
				t.Fatalf("%v: %s", err, errOut)
			}
			for _, want := range []string{
				"class Denco < Formula",
				`version "1.2.3"`,
				`homepage "https://example.test/me/Denco"`,
				`bin.install "denco"`,
				`assert_match "denco v1.2.3"`,
			} {
				if !strings.Contains(f, want) {
					t.Errorf("no %q in:\n%s", want, f)
				}
			}
			// Each platform's block has its own archive and that archive's
			// checksum, in that order.
			for _, p := range []struct{ block, inner, arch string }{
				{"on_macos", "on_arm", "darwin_arm64"},
				{"on_macos", "on_intel", "darwin_amd64"},
				{"on_linux", "on_arm", "linux_arm64"},
				{"on_linux", "on_intel", "linux_amd64"},
			} {
				re := regexp.MustCompile(`(?s)` + p.block + ` do\n.*?` + p.inner + ` do\n\s*url "([^"]+)"\n\s*sha256 "([0-9a-f]+)"`)
				m := re.FindStringSubmatch(f)
				wantURL := "https://example.test/me/Denco/releases/download/v1.2.3/denco_v1.2.3_" + p.arch + ".tar.gz"
				if m == nil || m[1] != wantURL || m[2] != sums[p.arch] {
					t.Errorf("%s %s: got %q, want %s with %s", p.block, p.inner, m, wantURL, sums[p.arch])
				}
			}
			if strings.Contains(f, "windows") || strings.Contains(f, sums["windows_amd64"]) {
				t.Error("Windows archives in the formula")
			}
			if ruby, err := exec.LookPath("ruby"); err == nil {
				rb := filepath.Join(t.TempDir(), "denco.rb")
				os.WriteFile(rb, []byte(f), 0o644)
				if out, err := exec.Command(ruby, "-c", rb).CombinedOutput(); err != nil {
					t.Errorf("not valid Ruby: %v\n%s", err, out)
				}
			}
		})
	}
}

// Bad input writes no formula (stdout is what gets pushed to the tap).
func TestBrewFormulaRefuses(t *testing.T) {
	for _, sh := range shells(t) {
		good, _ := brewSums(t, "v1.2.3", "")
		missing, _ := brewSums(t, "v1.2.3", "denco_v1.2.3_linux_arm64.tar.gz")
		short := filepath.Join(t.TempDir(), "SHA256SUMS")
		os.WriteFile(short, []byte("abcdef  denco_v1.2.3_darwin_arm64.tar.gz\n"), 0o644)
		for _, c := range []struct {
			args []string
			why  string
		}{
			{[]string{"v1.2.3"}, "usage"},
			{[]string{"1.2.3", good}, "isn't a version tag"},
			{[]string{"v1.2", good}, "isn't a version tag"},
			{[]string{"v1.2.3-rc1", good}, "pre-release"},
			{[]string{"v1.2.3", filepath.Join(t.TempDir(), "nope")}, "can't read"},
			{[]string{"v1.2.3", missing}, "denco_v1.2.3_linux_arm64.tar.gz isn't listed"},
			{[]string{"v1.2.3", short}, "malformed checksum"},
		} {
			out, errOut, err := brewFormula(t, sh, c.args...)
			if err == nil || out != "" || !strings.Contains(errOut, c.why) {
				t.Errorf("%s %v: err %v, stdout %q, stderr %q; want a failure saying %q", sh, c.args, err, out, errOut, c.why)
			}
		}
	}
}

// The formula's archive names are the ones release-build.sh makes.
func TestBrewFormulaMatchesReleaseBuild(t *testing.T) {
	b, err := os.ReadFile("../release-build.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`name="denco_${version}_${goos}_${goarch}"`, `tar -czf "$name.tar.gz" "$name"`, "darwin/arm64", "darwin/amd64", "linux/arm64", "linux/amd64"} {
		if !strings.Contains(s, want) {
			t.Errorf("release-build.sh no longer has %q: update brew-formula.sh to match", want)
		}
	}
}
