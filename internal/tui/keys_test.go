package tui

import (
	"reflect"
	"testing"
)

func TestParseKeys(t *testing.T) {
	r := runeKey
	k := func(c KeyCode) Key { return Key{Code: c} }
	tests := []struct {
		name string
		in   string
		want []Key
	}{
		{"letters", "jk", []Key{r('j'), r('k')}},
		{"arrows", "\x1b[A\x1b[B\x1b[C\x1b[D", []Key{k(KeyUp), k(KeyDown), k(KeyRight), k(KeyLeft)}},
		{"application-mode arrows", "\x1bOA\x1bOB", []Key{k(KeyUp), k(KeyDown)}},
		{"ctrl-up counts as up", "\x1b[1;5A", []Key{k(KeyUp)}},
		{"home end", "\x1b[H\x1b[F\x1b[1~\x1b[4~", []Key{k(KeyHome), k(KeyEnd), k(KeyHome), k(KeyEnd)}},
		{"pages", "\x1b[5~\x1b[6~", []Key{k(KeyPgUp), k(KeyPgDn)}},
		{"delete, backtab", "\x1b[3~\x1b[Z", []Key{k(KeyDelete), k(KeyBackTab)}},
		{"enter (CR, LF, CRLF once)", "\r\n\r", []Key{k(KeyEnter), k(KeyEnter)}},
		{"lone esc", "\x1b", []Key{k(KeyEsc)}},
		{"esc then a key", "\x1bq", []Key{k(KeyEsc), r('q')}},
		{"controls", "\x03\x15\x17\t\x7f\x08", []Key{k(KeyCtrlC), k(KeyCtrlU), k(KeyCtrlW), k(KeyTab), k(KeyBackspace), k(KeyBackspace)}},
		{"utf-8", "é日", []Key{r('é'), r('日')}},
		{"unknown sequence dropped", "\x1b[99~x", []Key{r('x')}},
		{"cut-off sequence dropped", "x\x1b[1;", []Key{r('x')}},
		{"stray control dropped", "\x01a", []Key{r('a')}},
		{"invalid utf-8 dropped", "\xffa", []Key{r('a')}},
		{"pasted magnet", "magnet:?xt", []Key{r('m'), r('a'), r('g'), r('n'), r('e'), r('t'), r(':'), r('?'), r('x'), r('t')}},
	}
	for _, tt := range tests {
		if got := parseKeys([]byte(tt.in)); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: parseKeys(%q) = %v, want %v", tt.name, tt.in, got, tt.want)
		}
	}
}
