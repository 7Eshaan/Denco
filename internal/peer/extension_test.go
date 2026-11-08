package peer

import (
	"errors"
	"reflect"
	"testing"
)

func TestExtHandshakeRoundTrip(t *testing.T) {
	h := ExtHandshake{
		M:            map[string]int{ExtMetadata: ExtMetadataID, "ut_pex": 2},
		MetadataSize: 31337, Version: "Denco 0.0.1", Port: 6881, ReqQ: 256,
	}
	got, err := ParseExtHandshake(h.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, h) {
		t.Fatalf("got %+v, want %+v", got, h)
	}
	// Zero values are omitted and parse back as zero.
	if got, _ := ParseExtHandshake(ExtHandshake{}.Encode()); len(got.M) != 0 || got.MetadataSize != 0 || got.Version != "" {
		t.Fatalf("empty = %+v", got)
	}
}

func TestParseExtHandshakeIsLenientAboutValues(t *testing.T) {
	// Odd values are skipped rather than failing the whole handshake.
	body := []byte("d1:md11:ut_metadatai3e6:ut_pexi-1e4:junk3:abce13:metadata_sizei-5e1:pi99999e1:v2:qBe")
	h, err := ParseExtHandshake(body)
	if err != nil {
		t.Fatal(err)
	}
	if h.M[ExtMetadata] != 3 || len(h.M) != 1 || h.MetadataSize != 0 || h.Port != 0 || h.Version != "qB" {
		t.Fatalf("h = %+v", h)
	}
	for _, bad := range [][]byte{[]byte("le"), []byte("d1:m"), make([]byte, maxExtHandshake+1)} {
		if _, err := ParseExtHandshake(bad); !errors.Is(err, ErrBadPayload) {
			t.Errorf("ParseExtHandshake(%.10q): err = %v", bad, err)
		}
	}
}
