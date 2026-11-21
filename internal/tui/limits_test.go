package tui

import (
	"strings"
	"testing"
)

func TestLimitsPrompt(t *testing.T) {
	m := newModel(120, 24, false, true, t0)
	ts := fakeTorrents(2)
	ts[0].DownLimit = 1_500_000
	connected(m, ts)

	// s opens the prompt, filled with the current limits.
	if acts := press(m, runeKey('s')); len(acts) != 0 || m.mode != modeLimits {
		t.Fatalf("s: mode %v, actions %+v", m.mode, acts)
	}
	if got := string(m.input); got != "1.5M 0" {
		t.Fatalf("prefilled %q, want \"1.5M 0\"", got)
	}
	if got := screenText(m); !strings.Contains(got, "Limits for #1 (down up)") {
		t.Errorf("prompt not shown:\n%s", got)
	}
	press(m, Key{Code: KeyCtrlU})
	for _, r := range "500K 2M" {
		press(m, runeKey(r))
	}
	acts := press(m, kEnter)
	if len(acts) != 1 || acts[0].kind != actLimits || acts[0].id != 1 || acts[0].down != 500_000 || acts[0].up != 2_000_000 {
		t.Fatalf("enter: %+v", acts)
	}

	// Bad input is refused here, with the reason.
	for _, bad := range []string{"500K", "fast 0", "10B 0"} {
		press(m, runeKey('s'), Key{Code: KeyCtrlU})
		for _, r := range bad {
			press(m, runeKey(r))
		}
		if acts := press(m, kEnter); len(acts) != 0 {
			t.Errorf("%q: actions %+v", bad, acts)
		}
		if _, isErr := m.message(); !isErr {
			t.Errorf("%q: no error shown", bad)
		}
	}

	// Esc cancels.
	press(m, runeKey('s'))
	if acts := press(m, kEsc); len(acts) != 0 || m.mode != modeNormal {
		t.Errorf("esc: mode %v, %+v", m.mode, acts)
	}

	// Line mode: "s 0 0" in one line.
	lm := newModel(120, 24, true, true, t0)
	connected(lm, fakeTorrents(1))
	if acts := lm.update(lineEvent{"s 0 0"}); len(acts) != 1 || acts[0].kind != actLimits || acts[0].down != 0 {
		t.Errorf("line mode: %+v", acts)
	}
}

func TestLimitsShownAndHelp(t *testing.T) {
	m := newModel(120, 24, false, true, t0)
	ts := fakeTorrents(1)
	ts[0].DownLimit, ts[0].UpLimit = 500_000, 0
	connected(m, ts)
	press(m, kEnter)
	if got := screenText(m); !strings.Contains(got, "↓ 500.0 KB/s · ↑ no limit (its own") {
		t.Errorf("details lack the limits:\n%s", got)
	}
	press(m, kEsc, runeKey('?'))
	got := screenText(m)
	for _, want := range []string{"its own speed limits", "p r d f s K J T B"} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing %q:\n%s", want, got)
		}
	}
	if compactRate(0) != "0" || compactRate(2_000_000) != "2M" || compactRate(64_000) != "64K" || compactRate(1500) != "1.5K" {
		t.Error("compactRate")
	}
}
