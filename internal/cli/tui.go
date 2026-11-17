package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/tui"
)

// cmdTUI runs the terminal dashboard (internal/tui, DECISIONS D55). It
// connects like every client command (starting the daemon if needed),
// and quitting it leaves the daemon running.
func cmdTUI(e *env, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(e.stderr, "usage: denco tui")
		return 2
	}
	if !tui.IsTerminal(os.Stdout) {
		fmt.Fprintf(e.stderr, "denco: %v\n", tui.ErrNotTerminal)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), tui.QuitSignals()...)
	defer stop()
	// Connect (and maybe start the daemon) before raw mode, so Ctrl-C still
	// works while waiting and the start notice prints normally.
	c, code := connect(ctx, e)
	if c == nil {
		return code
	}
	term, err := tui.Open(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	defer term.Restore() // also when Run panics
	return runTUI(ctx, e, c, term, 0)
}

// runTUI runs the dashboard on term (tests pass a fake terminal).
func runTUI(ctx context.Context, e *env, c *ipc.Client, term tui.Terminal, refresh time.Duration) int {
	cfg := tui.Config{
		Client:  tuiClient{c, e},
		Refresh: refresh,
		Color:   os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb",
		// Find a restarted daemon again, but never start one: if the user
		// stopped it, the dashboard says so instead.
		Reconnect: func(ctx context.Context) (tui.Client, error) {
			c, err := ipc.Connect(ctx, e.daemonFile())
			if err != nil {
				return nil, err
			}
			return tuiClient{c, e}, nil
		},
	}
	if err := tui.Run(ctx, term, cfg); err != nil {
		term.Restore() // so the message isn't lost on the alternate screen
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	return 0
}

// tuiClient is the daemon client with `denco add`'s handling of what the
// user typed: .torrent paths, magnet links and http(s) URLs.
type tuiClient struct {
	*ipc.Client
	e *env
}

func (c tuiClient) AddTorrent(ctx context.Context, arg string) (engine.Torrent, error) {
	// addRequest prints the torrent's summary; the dashboard shows its own.
	quiet := &env{home: c.e.home, stdout: io.Discard, stderr: io.Discard}
	req, _, err := addRequest(ctx, quiet, typedPath(arg))
	if err != nil {
		return engine.Torrent{}, err
	}
	return c.Client.Add(ctx, req)
}

// typedPath tidies a path typed or dropped into the add prompt, where no
// shell has expanded it: surrounding quotes, a leading ~, and the "\ "
// escapes terminals insert when a file is dragged in.
func typedPath(arg string) string {
	arg = strings.TrimSpace(arg)
	if len(arg) >= 2 && (arg[0] == '"' || arg[0] == '\'') && arg[len(arg)-1] == arg[0] {
		arg = arg[1 : len(arg)-1]
	}
	if magnet.IsMagnet(arg) || isURL(arg) {
		return arg
	}
	if arg == "~" || strings.HasPrefix(arg, "~/") || strings.HasPrefix(arg, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			arg = filepath.Join(home, arg[1:])
		}
	}
	if _, err := os.Stat(arg); err != nil && strings.Contains(arg, `\ `) {
		if un := strings.ReplaceAll(arg, `\ `, " "); fileExists(un) {
			arg = un
		}
	}
	return arg
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
