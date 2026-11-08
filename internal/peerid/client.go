package peerid

import "fmt"

// Azureus-style two-letter client codes ("-qB4630-…"). Deliberately short:
// only codes we're confident about, since a wrong label is worse than "unknown".
var clientCodes = map[string]string{
	"AZ": "Vuze", "BC": "BitComet", "BI": "BiglyBT", "BT": "BitTorrent",
	"DE": "Deluge", "DN": "Denco", "FW": "FrostWire", "KT": "KTorrent",
	"LT": "libtorrent", "lt": "libTorrent (rakshasa)", "qB": "qBittorrent",
	"TR": "Transmission", "UT": "µTorrent", "UM": "µTorrent Mac",
	"WW": "WebTorrent", "WD": "WebTorrent Desktop", "XL": "Xunlei",
}

// ClientName guesses the client software from a peer ID, e.g.
// "-qB4630-…" → "qBittorrent 4.6.3.0". Unknown formats return "unknown".
func ClientName(id ID) string {
	if id[0] == '-' && id[7] == '-' {
		code := string(id[1:3])
		name, ok := clientCodes[code]
		if !ok {
			name = "unknown (" + code + ")"
		}
		return fmt.Sprintf("%s %s", name, versionDigits(id[3:7]))
	}
	return "unknown"
}

// versionDigits renders "4630" as "4.6.3.0". Some clients use letters for
// numbers above 9 (e.g. 'A' = 10); those are shown as-is.
func versionDigits(v []byte) string {
	out := make([]byte, 0, 2*len(v))
	for i, c := range v {
		if i > 0 {
			out = append(out, '.')
		}
		out = append(out, c)
	}
	return string(out)
}
