package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func abs(t *testing.T, parts ...string) string {
	t.Helper()
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func TestDefaults(t *testing.T) {
	home := t.TempDir()
	c := Defaults(home)
	if c.MaxActiveDownloads != 5 || c.MaxConnectionsPerTorrent != 50 || c.ListenPort != 6881 ||
		c.DownloadLimit != 0 || c.UploadLimit != 0 || !c.EnableDHT || !c.EnablePEX || !c.ResumeEnabled {
		t.Fatalf("defaults = %+v", c)
	}
	if c.ResumeDir != filepath.Join(home, "resume") || !filepath.IsAbs(c.DownloadDir) {
		t.Fatalf("paths = %q, %q", c.ResumeDir, c.DownloadDir)
	}
	// Missing file: defaults, no error.
	got, err := Load(filepath.Join(home, "nope.yaml"), c)
	if err != nil || got != c {
		t.Fatalf("missing file: %+v, %v", got, err)
	}
}

func TestParse(t *testing.T) {
	u, _ := os.UserHomeDir()
	dl := abs(t, "data", "torrents")
	text := strings.Join([]string{
		"# Denco settings",
		"",
		"download_dir: ~/Downloads/Torrents",
		"max_active_downloads: 3   # trailing comment",
		"max_connections_per_torrent: 80",
		"download_limit: 2M",
		"upload_limit: 500 KB/s",
		"enable_dht: no",
		"enable_pex: off",
		"listen_port: 51413",
		"resume_enabled: off\r", // Windows line ending
		`resume_dir: "` + dl + ` #1"`,
		"   ",
	}, "\n")
	c, err := Parse(text, Defaults(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DownloadDir:              filepath.Join(u, "Downloads", "Torrents"),
		MaxActiveDownloads:       3,
		MaxConnectionsPerTorrent: 80,
		DownloadLimit:            2_000_000,
		UploadLimit:              500_000,
		EnableDHT:                false,
		EnablePEX:                false,
		ListenPort:               51413,
		ResumeEnabled:            false,
		ResumeDir:                dl + " #1",
	}
	if c != want {
		t.Fatalf("got  %+v\nwant %+v", c, want)
	}
}

func TestParseErrors(t *testing.T) {
	base := Defaults(t.TempDir())
	cases := map[string]string{
		"listen_port: 6881\nlisten_prot: 1":       "line 2: unknown setting \"listen_prot\"",
		"listen_port: 99999":                      "line 1: listen_port: 99999 is out of range",
		"max_active_downloads: lots":              "not a whole number",
		"max_active_downloads: 0":                 "out of range",
		"enable_dht: maybe":                       "not true or false",
		"download_limit: fast":                    "not a speed",
		"download_limit: 5 parsecs":               "unknown unit",
		"download_limit: 100B":                    "below the minimum",
		"download_dir: relative/path":             "must be an absolute path",
		"download_dir:":                           "empty path",
		"just some words":                         "expected 'key: value'",
		"listen_port: 1\nlisten_port: 2":          "line 2: listen_port is already set on line 1",
		`download_dir: "/x`:                       "unterminated quote",
		`download_dir: "/x" junk`:                 "unexpected text",
		"# fine\nupload_limit: 1M\nenable_pex: 2": "line 3",
	}
	for text, want := range cases {
		_, err := Parse(text, base)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q): err = %v, want it to contain %q", text, err, want)
		}
	}
	if _, err := Parse("nope: 1", base); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key should wrap ErrUnknownKey: %v", err)
	}
}

func TestParseRate(t *testing.T) {
	cases := map[string]int64{
		"0": 0, "0K": 0, "500": 500_000, "500K": 500_000, "500kb": 500_000, "500 KB/s": 500_000,
		"1.5M": 1_500_000, "2MB/s": 2_000_000, "1MiB": 1 << 20, "1 GiB/s": 1 << 30, "1000B": 1000, "1g": 1e9,
	}
	for in, want := range cases {
		if got, err := ParseRate(in); err != nil || got != want {
			t.Errorf("ParseRate(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-5", "K", "1.2.3M", "10 xb", "5TB", "999B"} {
		if _, err := ParseRate(in); err == nil {
			t.Errorf("ParseRate(%q) should fail", in)
		}
	}
	// What FormatRate prints, ParseRate reads back.
	for _, n := range []int64{0, 1000, 500_000, 2_500_000, 3_000_000_000} {
		if got, err := ParseRate(FormatRate(n)); err != nil || got != n {
			t.Errorf("round trip %d → %q → %d, %v", n, FormatRate(n), got, err)
		}
	}
	if DescribeRate(0) != "unlimited" || DescribeRate(2_000_000) != "2.0 MB/s" {
		t.Errorf("DescribeRate: %q %q", DescribeRate(0), DescribeRate(2_000_000))
	}
}

func TestSetInFileKeepsCommentsAndOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	base := Defaults(t.TempDir())
	orig := "# my settings\nlisten_port: 7000   # forwarded on the router\n\n# speed\nupload_limit: 1M\n"
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(orig), 0o644)

	c, err := SetInFile(path, "listen_port", "7001", base)
	if err != nil || c.ListenPort != 7001 || c.UploadLimit != 1_000_000 {
		t.Fatalf("set: %+v, %v", c, err)
	}
	c, err = SetInFile(path, "download_limit", "3M", base) // new key: appended
	if err != nil || c.DownloadLimit != 3_000_000 {
		t.Fatalf("append: %+v, %v", c, err)
	}
	data, _ := os.ReadFile(path)
	want := "# my settings\nlisten_port: 7001  # forwarded on the router\n\n# speed\nupload_limit: 1M\ndownload_limit: 3M\n"
	if string(data) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", data, want)
	}

	// Invalid values and keys are rejected without touching the file.
	if _, err := SetInFile(path, "listen_port", "-1", base); err == nil {
		t.Fatal("bad value accepted")
	}
	if _, err := SetInFile(path, "colour", "blue", base); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != want {
		t.Fatal("rejected set modified the file")
	}

	// A value containing '#' is quoted, and reads back intact.
	dir := abs(t, "media", "tv #2")
	if _, err := SetInFile(path, "download_dir", dir, base); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path, base); err != nil || c.DownloadDir != dir {
		t.Fatalf("quoted path: %q, %v", c.DownloadDir, err)
	}

	if k := KeysInFile(path); !k["download_limit"] || !k["listen_port"] || k["enable_dht"] {
		t.Errorf("KeysInFile = %v", k)
	}

	// Unset drops the line; the default applies again.
	c, err = UnsetInFile(path, "listen_port", base)
	if err != nil || c.ListenPort != 6881 {
		t.Fatalf("unset: %+v, %v", c, err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "listen_port") {
		t.Fatalf("line not removed:\n%s", data)
	}
}

func TestSetInFileCreatesFileAndRefusesBrokenOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	base := Defaults(dir)
	if _, err := SetInFile(path, "enable_pex", "yes", base); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# Denco configuration") || !strings.HasSuffix(string(data), "enable_pex: yes\n") {
		t.Fatalf("new file:\n%s", data)
	}
	os.WriteFile(path, []byte("garbage line\n"), 0o644)
	if _, err := SetInFile(path, "enable_pex", "no", base); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("editing a broken file: %v", err)
	}
}

func TestInitWritesEverySettingAndParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	base := Defaults(dir)
	base.UploadLimit = 250_000
	if err := Init(path, base); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, s := range Settings() {
		if !strings.Contains(string(data), "\n"+s.Name+": ") || !strings.Contains(string(data), s.Help) {
			t.Errorf("init file lacks %s", s.Name)
		}
	}
	if c, err := Load(path, Defaults(dir)); err != nil || c != base {
		t.Fatalf("init file reads back as %+v, %v; want %+v", c, err, base)
	}
	if err := Init(path, base); err == nil {
		t.Fatal("Init overwrote an existing file")
	}
}

func TestGetAndLookup(t *testing.T) {
	c := Defaults(t.TempDir())
	c.DownloadLimit = 2_000_000
	if v, err := c.Get("download_limit"); err != nil || v != "2.0 MB/s" {
		t.Fatalf("get = %q, %v", v, err)
	}
	if v, _ := c.Get("upload_limit"); v != "0" {
		t.Fatalf("unlimited = %q", v)
	}
	if _, err := c.Get("nope"); !errors.Is(err, ErrUnknownKey) {
		t.Fatal(err)
	}
	if s, _ := Lookup("upload_limit"); !s.Live {
		t.Error("limits should be live")
	}
	if s, _ := Lookup("listen_port"); s.Live {
		t.Error("listen_port can't be applied live")
	}
}
