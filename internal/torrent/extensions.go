package torrent

import (
	"github.com/7Eshaan/Denco/internal/metadata"
	"github.com/7Eshaan/Denco/internal/peer"
)

// BEP 10 extension messages. A session advertises ut_metadata (BEP 9) and
// serves its info dict to peers that ask — that is how peers who only have
// a magnet link get the torrent. Phase 10b adds ut_pex here.

// sendExtHandshake tells p which extensions we support, if both sides
// set the extension bit.
func (s *Session) sendExtHandshake(p *peerState) {
	if !s.cfg.PeerOpts.Extensions || !p.conn.Peer.SupportsExtensions() {
		return
	}
	hs := peer.ExtHandshake{
		M:            map[string]int{peer.ExtMetadata: peer.ExtMetadataID},
		MetadataSize: len(s.mi.InfoBytes),
		Version:      "Denco 0.0.1",
		Port:         int(s.cfg.Port),
		ReqQ:         maxQueuedUploads,
	}
	s.sendTo(p, peer.Extended(0, hs.Encode()))
}

func (s *Session) onExtended(p *peerState, m *peer.Message) {
	id, body, err := peer.ParseExtended(m)
	if err != nil {
		s.drop(p, "bad extended message")
		return
	}
	switch id {
	case 0:
		hs, err := peer.ParseExtHandshake(body)
		if err != nil {
			s.drop(p, "bad extension handshake")
			return
		}
		p.extMetadata = hs.M[peer.ExtMetadata]
	case peer.ExtMetadataID:
		s.onMetadataMessage(p, body)
	}
	// Other IDs: extensions we didn't advertise; peers shouldn't send them.
}

// onMetadataMessage answers ut_metadata requests from the info dict we
// hold. Data and reject messages are ignored: we already have the metadata.
func (s *Session) onMetadataMessage(p *peerState, body []byte) {
	msg, err := metadata.Decode(body)
	if err != nil {
		s.drop(p, "bad ut_metadata message")
		return
	}
	if msg.Type != metadata.Request || p.extMetadata == 0 {
		return
	}
	reply := metadata.Message{Type: metadata.Reject, Piece: msg.Piece}
	if piece, ok := metadata.Piece(s.mi.InfoBytes, msg.Piece); ok {
		reply = metadata.Message{Type: metadata.Data, Piece: msg.Piece, TotalSize: len(s.mi.InfoBytes), Data: piece}
	}
	s.sendTo(p, peer.Extended(uint8(p.extMetadata), reply.Encode()))
}
