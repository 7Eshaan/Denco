package bencode

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeValid(t *testing.T) {
	tests := []struct {
		in   string
		want any
	}{
		{"i0e", int64(0)},
		{"i42e", int64(42)},
		{"i-7e", int64(-7)},
		{"i9223372036854775807e", int64(9223372036854775807)},
		{"0:", ""},
		{"4:spam", "spam"},
		{"3:\x00\xff\x01", "\x00\xff\x01"}, // binary-safe
		{"le", []any{}},
		{"l4:spami3ee", []any{"spam", int64(3)}},
		{"de", map[string]any{}},
		{"d3:cow3:moo4:spaml1:a1:bee", map[string]any{
			"cow":  "moo",
			"spam": []any{"a", "b"},
		}},
		// Unsorted keys are accepted (D7).
		{"d1:bi1e1:ai2ee", map[string]any{"a": int64(2), "b": int64(1)}},
		{"lli1eee", []any{[]any{int64(1)}}},
	}
	for _, tc := range tests {
		got, err := Decode([]byte(tc.in))
		if err != nil {
			t.Errorf("Decode(%q) error: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Decode(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestDecodeInvalid(t *testing.T) {
	tests := []string{
		"",
		"i",
		"ie",
		"i-e",
		"i-0e",  // negative zero
		"i03e",  // leading zero
		"i1.5e", // not an integer
		"i99999999999999999999e",
		"4:spa",   // too short
		"04:spam", // leading zero length
		"-1:a",
		"5",
		"l",
		"li1e",
		"d",
		"d3:cow",
		"di1ei2ee", // non-string key
		"x",
		"i1ei2e", // trailing data
		"4:spamX",
	}
	for _, in := range tests {
		if _, err := Decode([]byte(in)); err == nil {
			t.Errorf("Decode(%q) succeeded, want error", in)
		} else if !errors.Is(err, ErrSyntax) {
			t.Errorf("Decode(%q) error %v does not wrap ErrSyntax", in, err)
		}
	}
}

func TestDecodeDepthLimit(t *testing.T) {
	deep := strings.Repeat("l", maxDepth+10) + strings.Repeat("e", maxDepth+10)
	if _, err := Decode([]byte(deep)); err == nil {
		t.Fatal("expected nesting error")
	}
	ok := strings.Repeat("l", 100) + strings.Repeat("e", 100)
	if _, err := Decode([]byte(ok)); err != nil {
		t.Fatalf("depth 100 should be fine: %v", err)
	}
}

func TestDecodePrefix(t *testing.T) {
	in := []byte("d8:msg_typei1ee" + "RAWPIECEDATA")
	v, n, err := DecodePrefix(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(in[n:]) != "RAWPIECEDATA" {
		t.Fatalf("remaining = %q", in[n:])
	}
	if m := v.(map[string]any); m["msg_type"] != int64(1) {
		t.Fatalf("got %#v", v)
	}
}

func TestDictSpans(t *testing.T) {
	// Non-canonical inner dict (keys unsorted) — span must be the exact bytes.
	info := "d4:name1:x1:ai1ee"
	in := []byte("d8:announce3:url4:info" + info + "e")
	spans, err := DictSpans(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(spans["info"]) != info {
		t.Fatalf("info span = %q, want %q", spans["info"], info)
	}
	if string(spans["announce"]) != "3:url" {
		t.Fatalf("announce span = %q", spans["announce"])
	}
	for _, bad := range []string{"", "le", "d4:infoi1e", "d4:infoi1eeX"} {
		if _, err := DictSpans([]byte(bad)); err == nil {
			t.Errorf("DictSpans(%q) succeeded, want error", bad)
		}
	}
}

func TestEncode(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{42, "i42e"},
		{int64(-3), "i-3e"},
		{uint32(7), "i7e"},
		{"spam", "4:spam"},
		{[]byte{0, 1}, "2:\x00\x01"},
		{[]string{"a", "bc"}, "l1:a2:bce"},
		{[]any{1, "x"}, "li1e1:xe"},
		{map[string]any{"b": 1, "a": "x"}, "d1:a1:x1:bi1ee"}, // keys sorted
		{map[string]string{"z": "1", "y": "2"}, "d1:y1:21:z1:1e"},
		{Raw("i5e"), "i5e"},
	}
	for _, tc := range tests {
		got, err := Encode(tc.in)
		if err != nil {
			t.Errorf("Encode(%#v) error: %v", tc.in, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("Encode(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := Encode(3.14); err == nil {
		t.Error("Encode(float) should fail")
	}
	if _, err := Encode(map[string]any{"k": struct{}{}}); err == nil {
		t.Error("Encode(nested unsupported) should fail")
	}
}

func TestRoundTrip(t *testing.T) {
	in := []byte("d4:infod6:lengthi1024e4:name8:file.bin12:piece lengthi16384ee4:listl1:a1:be3:numi-5ee")
	v, err := Decode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Fatalf("round trip mismatch:\n in  %q\n out %q", in, out)
	}
}

func TestGetters(t *testing.T) {
	d := map[string]any{"s": "x", "i": int64(1), "l": []any{}, "d": map[string]any{}}
	if v, ok := GetString(d, "s"); !ok || v != "x" {
		t.Error("GetString")
	}
	if v, ok := GetInt(d, "i"); !ok || v != 1 {
		t.Error("GetInt")
	}
	if _, ok := GetList(d, "l"); !ok {
		t.Error("GetList")
	}
	if _, ok := GetDict(d, "d"); !ok {
		t.Error("GetDict")
	}
	if _, ok := GetInt(d, "s"); ok {
		t.Error("GetInt on string should fail")
	}
	if _, ok := GetString(d, "missing"); ok {
		t.Error("GetString on missing should fail")
	}
}

// FuzzDecode checks the decoder never panics and that anything it accepts
// re-encodes to something that decodes to the same value.
func FuzzDecode(f *testing.F) {
	for _, seed := range []string{"i42e", "4:spam", "l4:spami3ee", "d3:cow3:mooe", "d1:ad1:bl1:ceee"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Decode(data)
		if err != nil {
			return
		}
		enc, err := Encode(v)
		if err != nil {
			t.Fatalf("decoded value failed to encode: %v", err)
		}
		v2, err := Decode(enc)
		if err != nil {
			t.Fatalf("re-encoded value failed to decode: %v", err)
		}
		if !reflect.DeepEqual(v, v2) {
			t.Fatalf("round trip changed value: %#v vs %#v", v, v2)
		}
	})
}
