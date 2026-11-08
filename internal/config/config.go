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
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	EnableDHT                bool // accepted; DHT arrives in Phase 11
	EnablePEX                bool // peer exchange (BEP 11); never for private torrents
	ListenPort               int
	ResumeEnabled            bool
	ResumeDir                string
}

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
		ListenPort:               6881,
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
	{Name: "enable_dht", Help: "find peers without trackers (DHT; not available until Phase 11)",
		get: func(c *Config) string { return strconv.FormatBool(c.EnableDHT) },
		set: func(c *Config, v string) error { return setBool(&c.EnableDHT, v) }},
	{Name: "enable_pex", Help: "exchange peer lists with other peers (PEX); never used for private torrents",
		get: func(c *Config) string { return strconv.FormatBool(c.EnablePEX) },
		set: func(c *Config, v string) error { return setBool(&c.EnablePEX, v) }},
	{Name: "listen_port", Help: "TCP port for incoming peers (0 = random); a random one is used if it's taken",
		get: func(c *Config) string { return strconv.Itoa(c.ListenPort) },
		set: func(c *Config, v string) error { return setInt(&c.ListenPort, v, 0, 65535) }},
	{Name: "resume_enabled", Help: "remember torrents and progress across daemon restarts",
		get: func(c *Config) string { return strconv.FormatBool(c.ResumeEnabled) },
		set: func(c *Config, v string) error { return setBool(&c.ResumeEnabled, v) }},
	{Name: "resume_dir", Help: "where resume data is kept",
		get: func(c *Config) string { return c.ResumeDir },
		set: func(c *Config, v string) error { return setPath(&c.ResumeDir, v) }},
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
