package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/tracker"
)

// cmdAnnounce is a developer command: it announces "started" to every
// tracker of a torrent or magnet, prints what came back, then announces
// "stopped" so trackers don't list us as a peer we won't serve.
func cmdAnnounce(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("announce", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Uint("port", 6881, "port to report to trackers")
	showPeers := fs.Int("peers", 10, "max peers to print per tracker")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *port > 65535 {
		fmt.Fprintln(stderr, "usage: denco announce [-port N] [-peers N] <file.torrent | magnet-uri>")
		return 2
	}

	tiers, req, err := announceTarget(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	if len(tiers) == 0 {
		fmt.Fprintln(stderr, "denco: no trackers in this torrent (DHT support comes in Phase 11)")
		return 1
	}
	req.PeerID = peerid.New()
	req.Port = uint16(*port)
	req.Key = rand.Uint32()
	req.Event = tracker.EventStarted

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	list := tracker.NewList(tiers)
	client := &tracker.Client{}
	results := list.AnnounceAll(ctx, client, req)

	ok := printResults(stdout, results, *showPeers)

	// Politely deregister; short timeout, errors ignored.
	req.Event = tracker.EventStopped
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, r := range results {
		if r.Err == nil {
			client.Announce(sctx, r.URL, req)
		}
	}
	if !ok {
		return 1
	}
	return 0
}

// announceTarget turns a CLI argument into tracker tiers and a base request.
func announceTarget(arg string) ([][]string, tracker.Request, error) {
	if magnet.IsMagnet(arg) {
		m, err := magnet.Parse(arg)
		if err != nil {
			return nil, tracker.Request{}, err
		}
		// Size unknown before metadata: report 1 byte left so trackers treat
		// us as a leecher and return seeders.
		return tracker.TiersFromMagnet(m), tracker.Request{InfoHash: m.InfoHash, Left: 1}, nil
	}
	mi, err := metainfo.Load(arg)
	if err != nil {
		return nil, tracker.Request{}, err
	}
	return tracker.TiersFromMetaInfo(mi), tracker.Request{InfoHash: mi.InfoHash, Left: mi.Info.TotalLength}, nil
}

// printResults prints one block per tier; returns true if any tier succeeded.
func printResults(w io.Writer, results []tracker.Result, maxPeers int) bool {
	anyOK := false
	unique := map[string]bool{}
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(w, "✗ tier %d failed:\n    %v\n", r.Tier, indentErr(r.Err))
			continue
		}
		anyOK = true
		resp := r.Response
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "✓ %s\n", r.URL)
		fmt.Fprintf(tw, "    seeders / leechers:\t%s / %s\n", count(resp.Seeders), count(resp.Leechers))
		fmt.Fprintf(tw, "    peers returned:\t%d\n", len(resp.Peers))
		fmt.Fprintf(tw, "    next announce in:\t%s\n", resp.Interval)
		if resp.Warning != "" {
			fmt.Fprintf(tw, "    warning:\t%s\n", resp.Warning)
		}
		tw.Flush()
		for i, p := range resp.Peers {
			unique[p.String()] = true
			if i < maxPeers {
				fmt.Fprintf(w, "      %s\n", p)
			}
		}
		if len(resp.Peers) > maxPeers {
			fmt.Fprintf(w, "      … %d more\n", len(resp.Peers)-maxPeers)
		}
	}
	fmt.Fprintf(w, "\nTotal unique peers: %d\n", len(unique))
	return anyOK
}

func count(n int) string {
	if n < 0 {
		return "?"
	}
	return fmt.Sprint(n)
}

// indentErr puts each joined error on its own indented line.
func indentErr(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", "\n    ")
}
