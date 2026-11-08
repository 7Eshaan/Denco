package format

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	tests := map[int64]string{
		0:             "0 B",
		999:           "999 B",
		1000:          "1.0 KB",
		1500:          "1.5 KB",
		999_949:       "999.9 KB",
		999_950:       "1.0 MB", // no "1000.0 KB"
		850_000_000:   "850.0 MB",
		5_200_000_000: "5.2 GB",
		-2000:         "-2.0 KB",
	}
	for in, want := range tests {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
	if got := Speed(120_000); got != "120.0 KB/s" {
		t.Errorf("Speed = %q", got)
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		done, total int64
		want        string
	}{
		{0, 0, "0.0%"},
		{654, 1000, "65.4%"},
		{9999, 10000, "99.9%"},
		{999_999, 1_000_000, "99.9%"}, // not rounded up to 100
		{10, 10, "100%"},
	}
	for _, tc := range tests {
		if got := Percent(tc.done, tc.total); got != tc.want {
			t.Errorf("Percent(%d,%d) = %q, want %q", tc.done, tc.total, got, tc.want)
		}
	}
}

func TestDuration(t *testing.T) {
	tests := map[time.Duration]string{
		-1:                                    "∞",
		0:                                     "0s",
		42 * time.Second:                      "42s",
		7*time.Minute + 42*time.Second:        "7m 42s",
		3*time.Hour + 5*time.Minute:           "3h 5m",
		52*time.Hour + 10*time.Minute:         "2d 4h",
		59*time.Second + 600*time.Millisecond: "1m 0s",
	}
	for in, want := range tests {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestETA(t *testing.T) {
	if ETA(0, 0) != 0 {
		t.Error("finished should be 0")
	}
	if ETA(100, 0) != -1 {
		t.Error("stalled should be -1")
	}
	if got := ETA(1000, 100); got != 10*time.Second {
		t.Errorf("ETA = %v", got)
	}
}
