package server

import (
	"io"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/i41-eu/q41eu-raw/wire"
)

// Uploads to the robot's file store (pictures and sounds on its userdata
// partition, about 1.9 MB): the browser posts a file, the server sends it to
// the robot as wire.BinAssetChunk messages, and the robot answers with an
// "asset_saved" {name, bytes, crc} or "asset_error" {name, reason} event.

const (
	maxAssetBytes = 2 << 20
	uploadHeader  = "X-Stackchan-Upload" // a custom header forces a CORS preflight: no cross-site uploads
)

// Relative paths: parts of letters, digits, '.', '_', '-', not starting with '.'.
var assetNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)

func validAssetName(name string) bool {
	return len(name) <= 64 && assetNamePattern.MatchString(name)
}

// handleAssetUpload: POST /api/robots/{id}/assets?name=food/cake.png with the file as the body.
func (s *Server) handleAssetUpload(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	if !s.commandAllowed(session, time.Now()) {
		s.tooMany(w, session, "too many requests")
		return
	}
	name := r.URL.Query().Get("name")
	if r.Header.Get(uploadHeader) != "1" {
		http.Error(w, "missing "+uploadHeader+": 1", http.StatusBadRequest)
		return
	}
	if !validAssetName(name) {
		http.Error(w, "name: up to 64 letters, digits, . _ - and / (no part may start with .)", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	paired := s.sessions[session][id]
	st := s.robots[id]
	var conn *robotConn
	var commands []string
	encrypted := false
	if st != nil {
		conn, commands, encrypted = st.conn, st.caps.Commands, st.labels["e2e"] == "1"
	}
	s.mu.Unlock()
	switch {
	case !paired || st == nil:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	case encrypted:
		http.Error(w, e2eNoBinary, http.StatusConflict)
		return
	case !slices.Contains(commands, "assets"):
		http.Error(w, "this robot has no file store (update its firmware)", http.StatusBadRequest)
		return
	case conn == nil:
		http.Error(w, "robot is offline", http.StatusConflict)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAssetBytes))
	if err != nil {
		http.Error(w, "file too big (max 2 MB)", http.StatusRequestEntityTooLarge)
		return
	}
	chunks := wire.AssetChunks(name, data)
	go s.sendAsset(conn, name, chunks)
	s.log.Info("asset upload", "robot", id, "name", name, "bytes", len(data), "chunks", len(chunks))
	s.commandSent(id, "asset_upload", map[string]any{"name": name, "bytes": len(data)})
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "sending", "chunks": len(chunks)})
}

// sendAsset queues the chunks without crowding out commands: it waits while
// the robot's send queue is more than half full.
func (s *Server) sendAsset(c *robotConn, name string, chunks [][]byte) {
	for _, msg := range chunks {
		deadline := time.Now().Add(20 * time.Second)
		for len(c.send) > cap(c.send)/2 {
			if time.Now().After(deadline) {
				s.log.Warn("asset upload stalled", "robot", c.id, "name", name)
				return
			}
			select {
			case <-c.done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		if !c.enqueueBinary(msg) {
			s.log.Warn("asset upload stopped: robot gone or stuck", "robot", c.id, "name", name)
			return
		}
	}
}
