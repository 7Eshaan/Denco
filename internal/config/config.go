// Package config reads and writes Denco's configuration file: a flat
// `key: value` file (YAML-like, but parsed here with the standard library;
// DECISIONS D4, D35) at $DENCO_HOME/config.yaml.
//
// A missing file means "all defaults". Every setting is optional, unknown
// keys and bad values are errors (with the line number), so a typo never
// silently does nothing. Command-line flags override the file.
//
// Files:
//
//	config.go – Config, defaults, the list of settings
//	file.go   – parsing, and editing the file in place (keeps comments)
//	rate.go   – speed values like "500K" or "2 MB/s"
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/7Eshaan/Denco/internal/dht"
)

// FileName is the config file's name inside the denco home directory.
const FileName = "config.yaml"

// Config holds every setting. Paths are absolute; rates are bytes per
// second with 0 meaning unlimited.
type Config struct {
	DownloadDir              string
	MaxActiveDownloads       int
	MaxConnectionsPerTorrent int
	DownloadLimit            int64
	UploadLimit              int64
	EnableDHT                bool   // BEP 5 DHT; never for private torrents
	DHTBootstrap             string // comma-separated host:port routers; "" = none
	EnablePEX                bool   // peer exchange (BEP 11); never for private torrents
	ListenPort               int
	PortForwarding           bool // ask the router to forward ListenPort (UPnP / NAT-PMP)
	ResumeEnabled            bool
	ResumeDir                string
	SeedRatio                float64       // stop seeding at this upload/download ratio; 0 = no limit
	SeedTime                 time.Duration // stop seeding after this much seeding; 0 = no limit
	LogFile                  string        // the daemon logs here instead of the terminal; "" = terminal
}

// DefaultDHTBootstrap is the default dht_bootstrap. A variable so tests can
// clear it: no test may contact the public routers.
var DefaultDHTBootstrap = strings.Join(dht.DefaultBootstrap, ",")

// DefaultPortForwarding is port_forwarding's default. A variable so tests
// can turn it off: no test may talk to the real router.
var DefaultPortForwarding = true

// Defaults returns the settings used when the file doesn't set them.
// home is the denco home directory (resume data lives under it).
func Defaults(home string) Config {
	dl := filepath.Join(home, "downloads")
	if u, err := os.UserHomeDir(); err == nil {
		dl = filepath.Join(u, "Downloads", "Denco")
	}
	return Config{
		DownloadDir:              dl,
		MaxActiveDownloads:       5,
		MaxConnectionsPerTorrent: 50,
		EnableDHT:                true,
		DHTBootstrap:             DefaultDHTBootstrap,
		EnablePEX:                true, // like mainstream clients; private torrents never use it
		ListenPort:               6881,
		PortForwarding:           DefaultPortForwarding,
		ResumeEnabled:            true,
		ResumeDir:                filepath.Join(home, "resume"),
	}
}

// Setting describes one key of the file.
type Setting struct {
	Name string
	Help string // one line, shown by `denco config` and written by Init
	// Live settings are applied to a running daemon by `denco config set`;
	// the others take effect when the daemon next starts.
	Live bool
	get  func(*Config) string
	set  func(*Config, string) error
}

var settings = []Setting{
	{Name: "download_dir", Help: "where new torrents are downloaded (~ = your home folder)",
		get: func(c *Config) string { return c.DownloadDir },
		set: func(c *Config, v string) error { return setPath(&c.DownloadDir, v) }},
	{Name: "max_active_downloads", Help: "torrents downloading at once; more wait in a queue (seeding doesn't count)",
		get: func(c *Config) string { return strconv.Itoa(c.MaxActiveDownloads) },
		set: func(c *Config, v string) error { return setInt(&c.MaxActiveDownloads, v, 1, 1000) }},
	{Name: "max_connections_per_torrent", Help: "peer connections per torrent",
		get: func(c *Config) string { return strconv.Itoa(c.MaxConnectionsPerTorrent) },
		set: func(c *Config, v string) error { return setInt(&c.MaxConnectionsPerTorrent, v, 1, 1000) }},
	{Name: "download_limit", Help: "total download speed limit, e.g. 2M or 500K (KB/s if no unit); 0 = unlimited", Live: true,
		get: func(c *Config) string { return FormatRate(c.DownloadLimit) },
		set: func(c *Config, v string) error { return setRate(&c.DownloadLimit, v) }},
	{Name: "upload_limit", Help: "total upload speed limit, same format; 0 = unlimited", Live: true,
		get: func(c *Config) string { return FormatRate(c.UploadLimit) },
		set: func(c *Config, v string) error { return setRate(&c.UploadLimit, v) }},
	{Name: "seed_ratio", Help: "stop seeding a torrent at this ratio (uploaded ÷ downloaded), e.g. 2.0; 0 = no limit", Live: true,
		get: func(c *Config) string { return strconv.FormatFloat(c.SeedRatio, 'f', -1, 64) },
		set: func(c *Config, v string) error { return setRatio(&c.SeedRatio, v) }},
	{Name: "seed_time", Help: "stop seeding a torrent after seeding this long, e.g. 24h, 2d, 1d12h; 0 = no limit", Live: true,
		get: func(c *Config) string { return FormatSeedTime(c.SeedTime) },
		set: func(c *Config, v string) error { return setSeedTime(&c.SeedTime, v) }},
	{Name: "enable_dht", Help: "find peers without trackers (DHT); never used for private torrents",
		get: func(c *Config) string { return strconv.FormatBool(c.EnableDHT) },
		set: func(c *Config, v string) error { return setBool(&c.EnableDHT, v) }},
	{Name: "dht_bootstrap", Help: "DHT routers used to join (host:port, comma-separated); none = only remembered nodes",
		get: func(c *Config) string {
			if c.DHTBootstrap == "" {
				return "none"
			}
			return c.DHTBootstrap
		},
		set: func(c *Config, v string) error { return setHostPorts(&c.DHTBootstrap, v) }},
	{Name: "enable_pex", Help: "exchange peer lists with other peers (PEX); never used for private torrents",
		get: func(c *Config) string { return strconv.FormatBool(c.EnablePEX) },
		set: func(c *Config, v string) error { return setBool(&c.EnablePEX, v) }},
	{Name: "listen_port", Help: "TCP port for incoming peers (0 = random); a random one is used if it's taken",
		get: func(c *Config) string { return strconv.Itoa(c.ListenPort) },
		set: func(c *Config, v string) error { return setInt(&c.ListenPort, v, 0, 65535) }},
	{Name: "port_forwarding", Help: "ask your router to forward listen_port (UPnP / NAT-PMP), so peers can connect to you",
		get: func(c *Config) string { return strconv.FormatBool(c.PortForwarding) },
		set: func(c *Config, v string) error { return setBool(&c.PortForwarding, v) }},
	{Name: "resume_enabled", Help: "remember torrents and progress across daemon restarts",
		get: func(c *Config) string { return strconv.FormatBool(c.ResumeEnabled) },
		set: func(c *Config, v string) error { return setBool(&c.ResumeEnabled, v) }},
	{Name: "resume_dir", Help: "where resume data is kept",
		get: func(c *Config) string { return c.ResumeDir },
		set: func(c *Config, v string) error { return setPath(&c.ResumeDir, v) }},
	{Name: "log_file", Help: "the daemon writes its log here instead of the terminal (kept under ~20 MB); none = terminal",
		get: func(c *Config) string {
			if c.LogFile == "" {
				return "none"
			}
			return c.LogFile
		},
		set: func(c *Config, v string) error {
			if strings.EqualFold(v, "none") {
				c.LogFile = ""
				return nil
			}
			return setPath(&c.LogFile, v)
		}},
}

// Settings lists every setting, in file order.
func Settings() []Setting { return append([]Setting(nil), settings...) }

// ErrUnknownKey is returned for a key that isn't a setting.
var ErrUnknownKey = errors.New("unknown setting")

// Lookup finds a setting by name.
func Lookup(name string) (Setting, error) {
	for _, s := range settings {
		if s.Name == name {
			return s, nil
		}
	}
	return Setting{}, fmt.Errorf("%w %q (see 'denco config' for the list)", ErrUnknownKey, name)
}

// Get returns a setting's value as text (the form the file accepts).
func (c *Config) Get(name string) (string, error) {
	s, err := Lookup(name)
	if err != nil {
		return "", err
	}
	return s.get(c), nil
}

// Set parses and stores a value, or returns why it is invalid.
func (c *Config) Set(name, value string) error {
	s, err := Lookup(name)
	if err != nil {
		return err
	}
	if err := s.set(c, strings.TrimSpace(value)); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// --- value parsers ---------------------------------------------------------------------

func setInt(dst *int, v string, lo, hi int) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%q is not a whole number", v)
	}
	if n < lo || n > hi {
		return fmt.Errorf("%d is out of range (%d–%d)", n, lo, hi)
	}
	*dst = n
	return nil
}

func setBool(dst *bool, v string) error {
	switch strings.ToLower(v) {
	case "true", "yes", "on", "1":
		*dst = true
	case "false", "no", "off", "0":
		*dst = false
	default:
		return fmt.Errorf("%q is not true or false", v)
	}
	return nil
}

// setHostPorts accepts "none" or a comma-separated list of host:port.
func setHostPorts(dst *string, v string) error {
	if strings.EqualFold(v, "none") {
		*dst = ""
		return nil
	}
	var out []string
	for _, hp := range strings.Split(v, ",") {
		hp = strings.TrimSpace(hp)
		host, port, err := net.SplitHostPort(hp)
		n, perr := strconv.Atoi(port)
		if err != nil || host == "" || perr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q is not host:port (use 'none' for no routers)", hp)
		}
		out = append(out, hp)
	}
	*dst = strings.Join(out, ",")
	return nil
}

// DHTRouters returns DHTBootstrap as a list.
func (c *Config) DHTRouters() []string {
	if c.DHTBootstrap == "" {
		return nil
	}
	return strings.Split(c.DHTBootstrap, ",")
}

func setRatio(dst *float64, v string) error {
	r, err := strconv.ParseFloat(v, 64)
	if err != nil || r < 0 || r > 1000 || r != r {
		return fmt.Errorf("%q is not a ratio between 0 and 1000 (e.g. 2.0; 0 = no limit)", v)
	}
	*dst = r
	return nil
}

func setSeedTime(dst *time.Duration, v string) error {
	d, err := ParseSeedTime(v)
	if err != nil {
		return err
	}
	*dst = d
	return nil
}

// ParseSeedTime reads a seeding time: "0" (no limit), a Go duration
// ("36h", "90m"), or days with an optional rest ("2d", "1d12h").
// At least one minute; at most ten years.
func ParseSeedTime(v string) (time.Duration, error) {
	bad := fmt.Errorf("%q is not a time (e.g. 24h, 90m, 2d, 1d12h; 0 = no limit)", v)
	s := strings.ToLower(strings.ReplaceAll(v, " ", ""))
	if s == "0" {
		return 0, nil
	}
	var d time.Duration
	if days, rest, ok := strings.Cut(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 || n > 3650 {
			return 0, bad
		}
		d, s = time.Duration(n)*24*time.Hour, rest
	}
	if s != "" {
		r, err := time.ParseDuration(s)
		if err != nil || r < 0 {
			return 0, bad
		}
		d += r
	}
	if d < time.Minute || d > 3650*24*time.Hour {
		return 0, fmt.Errorf("%q: use at least 1m and at most 3650d (0 = no limit)", v)
	}
	return d, nil
}

// FormatSeedTime renders a seeding time the way ParseSeedTime reads it.
func FormatSeedTime(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d <= 0:
		return "0"
	case d%day == 0:
		return fmt.Sprintf("%dd", d/day)
	case d > day:
		return fmt.Sprintf("%dd%s", d/day, shortDuration(d%day))
	default:
		return shortDuration(d)
	}
}

// shortDuration drops Go's trailing zero units: "12h0m0s" → "12h".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func setRate(dst *int64, v string) error {
	n, err := ParseRate(v)
	if err != nil {
		return err
	}
	*dst = n
	return nil
}

// setPath expands a leading ~ and requires an absolute path, so the
// meaning doesn't depend on the directory the daemon happens to start in.
func setPath(dst *string, v string) error {
	p, err := ExpandPath(v)
	if err != nil {
		return err
	}
	*dst = p
	return nil
}

// ExpandPath turns "~" / "~/x" into the user's home directory and checks
// the result is absolute.
func ExpandPath(v string) (string, error) {
	if v == "" {
		return "", errors.New("empty path")
	}
	if v == "~" || strings.HasPrefix(v, "~/") || strings.HasPrefix(v, `~\`) {
		u, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("can't expand ~: %w", err)
		}
		v = filepath.Join(u, v[1:])
	}
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("%q must be an absolute path (or start with ~)", v)
	}
	return filepath.Clean(v), nil
}
