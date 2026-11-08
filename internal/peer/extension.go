package peer

import (
	"fmt"

	"github.com/7Eshaan/Denco/internal/bencode"
)

// BEP 10 extension protocol. After the BitTorrent handshake, peers that set
// the extension bit exchange an "extended handshake" (extended message 0):
// a bencoded dict whose "m" maps extension names to the message IDs the
// sender wants to *receive* them on. Later extended messages carry the
// receiver's ID for that extension. https://www.bittorrent.org/beps/bep_0010.html

// Extension names and the IDs Denco asks peers to use when sending them to
// us. Phase 10b adds ut_pex here.
const (
	ExtMetadata   = "ut_metadata" // BEP 9
	ExtMetadataID = 1
)

// maxExtHandshake bounds the handshake we accept (real ones are < 1 KB).
const maxExtHandshake = 64 << 10

// ExtHandshake is the content of an extended handshake. Unknown keys are
// ignored; zero values are omitted when encoding.
type ExtHandshake struct {
	// M maps extension name → message ID; 0 means "disabled".
	M            map[string]int
	MetadataSize int    // BEP 9: size of the info dict, if the sender has it
	Version      string // "v": client name and version
	Port         int    // "p": the sender's listening port
	ReqQ         int    // "reqq": how many outstanding requests it accepts
}

// Encode returns the bencoded handshake body (without the extended-message
// header byte).
func (h ExtHandshake) Encode() []byte {
	m := map[string]any{}
	for name, id := range h.M {
		m[name] = id
	}
	d := map[string]any{"m": m}
	if h.MetadataSize > 0 {
		d["metadata_size"] = h.MetadataSize
	}
	if h.Version != "" {
		d["v"] = h.Version
	}
	if h.Port > 0 {
		d["p"] = h.Port
	}
	if h.ReqQ > 0 {
		d["reqq"] = h.ReqQ
	}
	b, _ := bencode.Encode(d) // only supported types above
	return b
}

// ParseExtHandshake decodes a handshake body. It is lenient about odd
// values (skipped), strict about the shape (must be a dict).
func ParseExtHandshake(b []byte) (ExtHandshake, error) {
	var h ExtHandshake
	if len(b) > maxExtHandshake {
		return h, fmt.Errorf("%w: extension handshake of %d bytes", ErrBadPayload, len(b))
	}
	v, err := bencode.Decode(b)
	if err != nil {
		return h, fmt.Errorf("%w: extension handshake: %v", ErrBadPayload, err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return h, fmt.Errorf("%w: extension handshake is not a dict", ErrBadPayload)
	}
	h.M = map[string]int{}
	if m, ok := bencode.GetDict(d, "m"); ok {
		for name, idv := range m {
			if id, ok := idv.(int64); ok && id >= 0 && id <= 255 {
				h.M[name] = int(id)
			}
		}
	}
	if n, ok := bencode.GetInt(d, "metadata_size"); ok && n > 0 && n < 1<<31 {
		h.MetadataSize = int(n)
	}
	h.Version, _ = bencode.GetString(d, "v")
	if n, ok := bencode.GetInt(d, "p"); ok && n > 0 && n <= 65535 {
		h.Port = int(n)
	}
	if n, ok := bencode.GetInt(d, "reqq"); ok && n > 0 && n < 1<<20 {
		h.ReqQ = int(n)
	}
	return h, nil
}
