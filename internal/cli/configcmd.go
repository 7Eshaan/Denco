package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/7Eshaan/Denco/internal/config"
	"github.com/7Eshaan/Denco/internal/ipc"
)

const configUsage = `usage:
  denco config [list] [-v]          show every setting (-v: with descriptions)
  denco config get <key>            print one setting
  denco config set <key> <value>    change a setting (validated, written to the file)
  denco config unset <key>          go back to the default
  denco config init                 write a file listing every setting
  denco config path                 print the config file's location`

// cmdConfig views and edits the config file. It works without a daemon;
// speed limits are also applied to a running daemon at once.
func cmdConfig(e *env, args []string) int {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "-v":
		if sub == "-v" {
			args = append(args, "-v")
		}
		return configList(e, args)
	case "get":
		return configGet(e, args)
	case "set":
		return configSet(e, args)
	case "unset":
		return configUnset(e, args)
	case "init":
		return configInit(e, args)
	case "path":
		fmt.Fprintln(e.stdout, e.configFile())
		return 0
	default:
		fmt.Fprintln(e.stderr, configUsage)
		return 2
	}
}

func configList(e *env, args []string) int {
	verbose := len(args) == 1 && args[0] == "-v"
	if len(args) > 1 || (len(args) == 1 && !verbose) {
		fmt.Fprintln(e.stderr, configUsage)
		return 2
	}
	cfg, err := e.loadConfig()
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	inFile := config.KeysInFile(e.configFile())
	// Source before value: values (paths) can be long.
	tw := newTable(e.stdout)
	fmt.Fprintln(tw, "SETTING\tFROM\tVALUE")
	for _, s := range config.Settings() {
		v, _ := cfg.Get(s.Name)
		from := "default"
		if inFile[s.Name] {
			from = "file"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, from, humanValue(s.Name, v))
		if verbose {
			fmt.Fprintf(tw, "\t\t  ↳ %s\n", s.Help)
		}
	}
	tw.Flush()
	if _, err := os.Stat(e.configFile()); err != nil {
		fmt.Fprintf(e.stdout, "\nNo config file yet (%s); these are the defaults.\n", e.configFile())
	} else {
		fmt.Fprintf(e.stdout, "\nFile: %s\n", e.configFile())
	}
	return 0
}

func configGet(e *env, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(e.stderr, "usage: denco config get <key>")
		return 2
	}
	cfg, err := e.loadConfig()
	if err == nil {
		var v string
		if v, err = cfg.Get(args[0]); err == nil {
			fmt.Fprintln(e.stdout, v)
			return 0
		}
	}
	fmt.Fprintf(e.stderr, "denco: %v\n", err)
	return 1
}

func configSet(e *env, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(e.stderr, "usage: denco config set <key> <value>")
		return 2
	}
	key, value := args[0], strings.Join(args[1:], " ") // allows: set upload_limit 500 KB/s
	cfg, err := config.SetInFile(e.configFile(), key, value, config.Defaults(e.home))
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	v, _ := cfg.Get(key)
	fmt.Fprintf(e.stdout, "%s = %s\n", key, humanValue(key, v))
	return applyToDaemon(e, key, cfg)
}

// humanValue explains values whose plain form is cryptic.
func humanValue(key, v string) string {
	if (key == "download_limit" || key == "upload_limit") && v == "0" {
		return "0 (unlimited)"
	}
	if (key == "seed_ratio" || key == "seed_time") && v == "0" {
		return "0 (no limit)"
	}
	return v
}

func configUnset(e *env, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(e.stderr, "usage: denco config unset <key>")
		return 2
	}
	cfg, err := config.UnsetInFile(e.configFile(), args[0], config.Defaults(e.home))
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	v, _ := cfg.Get(args[0])
	fmt.Fprintf(e.stdout, "%s = %s (default)\n", args[0], humanValue(args[0], v))
	return applyToDaemon(e, args[0], cfg)
}

func configInit(e *env, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(e.stderr, "usage: denco config init")
		return 2
	}
	if err := config.Init(e.configFile(), config.Defaults(e.home)); err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "Wrote %s\n", e.configFile())
	return 0
}

// applyToDaemon tells a running daemon about a changed setting: speed
// limits apply at once, anything else needs a restart.
func applyToDaemon(e *env, key string, cfg config.Config) int {
	ctx := context.Background()
	c, err := ipc.Connect(ctx, e.daemonFile())
	if errors.Is(err, ipc.ErrNoDaemon) {
		return 0 // used at the next start
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: saved, but couldn't reach the daemon: %v\n", err)
		return 1
	}
	if s, _ := config.Lookup(key); !s.Live {
		fmt.Fprintln(e.stdout, "Saved. Restart the daemon to apply it: denco stop, then denco daemon.")
		return 0
	}
	// Change only this limit: the other may have been set by a daemon flag.
	st, err := c.Status(ctx)
	if err == nil {
		switch key {
		case "download_limit", "upload_limit":
			down, up := st.DownLimit, st.UpLimit
			if key == "download_limit" {
				down = cfg.DownloadLimit
			} else {
				up = cfg.UploadLimit
			}
			_, err = c.SetLimits(ctx, down, up)
		case "seed_ratio", "seed_time":
			ratio, d := st.SeedRatio, st.SeedTime
			if key == "seed_ratio" {
				ratio = cfg.SeedRatio
			} else {
				d = cfg.SeedTime
			}
			_, err = c.SetSeedLimits(ctx, ratio, d)
		default:
			err = fmt.Errorf("%s is marked live but the daemon has no way to apply it", key)
		}
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: saved, but applying it to the daemon failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(e.stdout, "Applied to the running daemon.")
	return 0
}
