package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/utp"
)

// cmdProbe is a developer command that exercises the peer wire protocol
// against real peers: announce, connect to peers in parallel, handshake,
// wait briefly for their bitfield, and print a table of who has what.
func cmdProbe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Uint("port", 6881, "port to report to trackers")
	maxPeers := fs.Int("n", 30, "max peers to try")
	parallel := fs.Int("parallel", 10, "connections attempted at once")
	wait := fs.Duration("wait", 5*time.Second, "how long to wait for each peer's bitfield")
	encFlag := fs.String("encryption", "prefer", "prefer, require or disable (MSE, as the encryption setting)")
	trFlag := fs.String("transport", "both", "both, tcp or utp (as the transport setting)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	enc, encErr := peer.ParseEncryption(*encFlag)
	transport, trErr := peer.ParseTransport(*trFlag)
	if fs.NArg() != 1 || *port > 65535 || *maxPeers < 1 || *parallel < 1 || encErr != nil || trErr != nil {
		fmt.Fprintln(stderr, "usage: denco probe [-n N] [-parallel N] [-wait D] [-encryption prefer|require|disable] [-transport both|tcp|utp] <file.torrent | magnet-uri>")
		return 2
	}

	t, err := loadTarget(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()

	id := peerid.New()
	addrs := append([]netip.AddrPort(nil), t.peers...)
	if len(t.tiers) > 0 {
		results, stopAnnounce := announceStarted(ctx, t, id, uint16(*port))
		defer stopAnnounce()
		for _, r := range results {
			if r.Err == nil {
				addrs = append(addrs, r.Response.Peers...)
			}
		}
	}
	addrs = dedupAddrs(addrs)
	if len(addrs) == 0 {
		fmt.Fprintln(stderr, "denco: no peers found")
		return 1
	}
	if len(addrs) > *maxPeers {
		addrs = addrs[:*maxPeers]
	}
	fmt.Fprintf(stdout, "Probing %d peers…\n\n", len(addrs))

	opts := peer.Options{PeerID: id, Extensions: true, Encryption: enc, Transport: transport}
	if transport != peer.TransportTCP {
		d, closeUTP := probeUTP()
		defer closeUTP()
		opts.UTP = d
	}
	reports := probeAll(ctx, addrs, t, opts, *parallel, *wait)
	printProbe(stdout, reports, t.numPieces)
	return 0
}

type probeReport struct {
	addr   netip.AddrPort
	err    error
	client string
	ext    bool
	dht    bool
	have   int // pieces from bitfield/have messages; -1 if no bitfield seen
	rtt    time.Duration
	// encrypted: MSE with RC4 (D63).
	encrypted bool
	utp       bool // over uTP (D64)
}

func probeAll(ctx context.Context, addrs []netip.AddrPort, t *target, opts peer.Options, parallel int, wait time.Duration) []probeReport {
	reports := make([]probeReport, len(addrs))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func(i int, a netip.AddrPort) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				reports[i] = probeReport{addr: a, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			reports[i] = probeOne(ctx, a, t, opts, wait)
		}(i, a)
	}
	wg.Wait()
	return reports
}

func probeOne(ctx context.Context, a netip.AddrPort, t *target, opts peer.Options, wait time.Duration) probeReport {
	r := probeReport{addr: a, have: -1}
	start := time.Now()
	c, err := peer.Dial(ctx, a, t.req.InfoHash, opts)
	if err != nil {
		r.err = err
		return r
	}
	defer c.Close()
	r.rtt = time.Since(start)
	r.client = peerid.ClientName(c.Peer.PeerID)
	r.encrypted = c.Encrypted
	r.utp = c.UTP
	r.ext = c.Peer.SupportsExtensions()
	r.dht = c.Peer.SupportsDHT()

	// Peers normally send a bitfield right after the handshake; peers with
	// nothing (or using BEP 6 "have none") may send only have messages, so
	// count those too until the bitfield arrives or the wait runs out.
	n := t.numPieces
	if n == 0 {
		n = -1 // magnet: piece count unknown until metadata arrives
	}
	var haves map[uint32]bool
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-c.Incoming():
			if !ok {
				r.err = c.Err()
				return r
			}
			switch m.ID {
			case peer.MsgBitfield:
				if n < 0 {
					n = len(m.Payload) * 8
				}
				bf, err := bitfield.FromBytes(m.Payload, n)
				if err != nil {
					r.err = err
					return r
				}
				r.have = bf.Count()
				return r // the bitfield is what we came for
			case peer.MsgHave:
				if idx, err := peer.ParseHave(m); err == nil {
					if haves == nil {
						haves = map[uint32]bool{}
					}
					haves[idx] = true
					r.have = len(haves)
				}
			}
		case <-timer.C:
			return r
		case <-ctx.Done():
			return r
		}
	}
}

func printProbe(w io.Writer, reports []probeReport, numPieces int) {
	sort.SliceStable(reports, func(i, j int) bool {
		return (reports[i].err == nil) && (reports[j].err != nil)
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tCLIENT\tHAS\tEXT\tDHT\tVIA\tENC\tCONNECT\tSTATUS")
	ok, encrypted, overUTP := 0, 0, 0
	for _, r := range reports {
		if r.err != nil {
			fmt.Fprintf(tw, "%s\t\t\t\t\t\t\t\t✗ %v\n", r.addr, shortErr(r.err))
			continue
		}
		ok++
		via := "tcp"
		if r.utp {
			via, overUTP = "utp", overUTP+1
		}
		enc := "no"
		if r.encrypted {
			enc, encrypted = "rc4", encrypted+1
		}
		has := "no bitfield"
		switch {
		case r.have >= 0 && numPieces > 0:
			has = fmt.Sprintf("%s (%d/%d)", format.Percent(int64(r.have), int64(numPieces)), r.have, numPieces)
		case r.have >= 0:
			has = fmt.Sprintf("%d pieces", r.have)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t✓\n", r.addr, r.client, has, yesNo(r.ext), yesNo(r.dht), via, enc, r.rtt.Round(time.Millisecond))
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d of %d peers handshook successfully (%d encrypted, %d over uTP).\n", ok, len(reports), encrypted, overUTP)
}

func dedupAddrs(in []netip.AddrPort) []netip.AddrPort {
	seen := make(map[netip.AddrPort]bool, len(in))
	out := in[:0]
	for _, a := range in {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// shortErr keeps table rows readable: just the innermost error text.
func shortErr(err error) string {
	s := err.Error()
	if len(s) > 70 {
		s = s[:67] + "..."
	}
	return s
}

// probeUTP opens uTP sockets on random UDP ports, IPv4 and (if this
// machine has it) IPv6, for probing over uTP (D64).
func probeUTP() (*utp.Dialer, func()) {
	d := &utp.Dialer{}
	if pc, err := net.ListenUDP("udp4", nil); err == nil {
		d.V4 = utp.NewSocket(pc, utp.Config{})
	}
	if pc, err := net.ListenUDP("udp6", nil); err == nil {
		d.V6 = utp.NewSocket(pc, utp.Config{})
	}
	return d, func() {
		for _, s := range []*utp.Socket{d.V4, d.V6} {
			if s != nil {
				s.Close()
			}
		}
	}
}
