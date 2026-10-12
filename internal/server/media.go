package server

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/e2e"
	"github.com/i41-eu/q41eu-raw/wire"
)

// Media: the robot streams camera frames and microphone audio as binary
// WebSocket messages (wire.BinCameraJPEG, wire.BinAudioPCM) only while at
// least one paired browser watches or listens. Browsers get the same
// messages over /api/robots/{id}/media.

const (
	maxPictureBytes  = 192 << 10
	mediaStatsPeriod = 10 * time.Second
)

// mediaSub is one browser media WebSocket.
type mediaSub struct {
	robot   string
	session string
	video   bool
	full    bool // video at 640x480 (?size=640x480) instead of 320x240
	audio   bool
	imu     bool
	touch   bool
	light   bool
	out     chan []byte
	sent    int // written by handleMedia
	dropped int // under Server.mu
}

// Browsers send the session cookie with the WebSocket handshake, so only
// accept pages from this server's own origin (no cross-site WebSocket hijack).
var browserUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 16 << 10,
	CheckOrigin: func(r *http.Request) bool {
		u, err := url.Parse(r.Header.Get("Origin"))
		return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
	},
}

// relayMedia forwards a binary message from a robot to the browsers that want it.
func (s *Server) relayMedia(robotID string, msg []byte) {
	if len(msg) < 2 {
		return
	}
	kind := msg[0]
	if kind == e2e.BinGroup {
		s.relayE2EBinary(robotID, msg)
		return
	}
	if kind == wire.BinSnapshot {
		s.storeSnapshot(robotID, msg[1:])
		return
	}
	stat := map[byte]int{wire.BinCameraJPEG: 0, wire.BinAudioPCM: 1, wire.BinAudioMulti: 1, wire.BinIMU: 2, wire.BinTouch: 3, wire.BinLight: 4}
	i, known := stat[kind]
	if !known {
		return // only media types go to browsers
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.robots[robotID]; st != nil {
		st.mediaFrames[i]++
		st.mediaBytes[i] += len(msg)
	}
	for sub := range s.media {
		wants := [5]bool{sub.video, sub.audio, sub.imu, sub.touch, sub.light}[i]
		if sub.robot != robotID || !wants {
			continue
		}
		select {
		case sub.out <- msg:
		default: // slow browser: drop this frame rather than delay the next ones
			sub.dropped++
		}
	}
}

// syncMedia turns the robot's camera and microphone on or off to match the
// current subscribers. Must be called with s.mu held.
func (s *Server) syncMedia(robotID string) {
	st := s.robots[robotID]
	if st == nil || st.conn == nil {
		return
	}
	var wantVideo, wantFull, wantAudio, wantIMU, wantTouch, wantLight bool
	for sub := range s.media {
		if sub.robot == robotID {
			wantVideo = wantVideo || sub.video
			wantFull = wantFull || (sub.video && sub.full)
			wantAudio = wantAudio || sub.audio
			wantIMU = wantIMU || sub.imu
			wantTouch = wantTouch || sub.touch
			wantLight = wantLight || sub.light
		}
	}
	send := func(command string, on bool) {
		f, err := wire.Marshal(wire.KindRobotCommand, wire.Meta{WorkerID: robotID},
			wire.RobotCommandBody{Command: command, Args: map[string]any{"on": on}})
		if err == nil && st.conn.enqueue(f) {
			s.log.Info("media", "robot", robotID, command, on)
		}
	}
	if (wantVideo != st.cameraOn || wantFull != st.cameraFull) && slices.Contains(st.caps.Commands, "camera") {
		// One size for all watchers: 640x480 while anyone asks for it.
		st.cameraOn, st.cameraFull = wantVideo, wantFull
		size := "320x240"
		if wantFull {
			size = "640x480"
		}
		f, err := wire.Marshal(wire.KindRobotCommand, wire.Meta{WorkerID: robotID},
			wire.RobotCommandBody{Command: "camera", Args: map[string]any{"on": wantVideo, "size": size}})
		if err == nil && st.conn.enqueue(f) {
			s.log.Info("media", "robot", robotID, "camera", wantVideo, "size", size)
		}
	}
	if wantAudio != st.micOn && slices.Contains(st.caps.Commands, "mic") {
		st.micOn = wantAudio
		send("mic", wantAudio)
	}
	if wantIMU != st.imuOn && slices.Contains(st.caps.Commands, "imu_stream") {
		st.imuOn = wantIMU
		send("imu_stream", wantIMU)
	}
	if wantTouch != st.touchOn && slices.Contains(st.caps.Commands, "touch_stream") {
		st.touchOn = wantTouch
		send("touch_stream", wantTouch)
	}
	if wantLight != st.lightOn && slices.Contains(st.caps.Commands, "light_stream") {
		st.lightOn = wantLight
		send("light_stream", wantLight)
	}
}

// handleMedia streams a paired robot's camera (?video=1) and microphone
// (?audio=1) to the browser as binary WebSocket messages.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	q := r.URL.Query()
	sub := &mediaSub{robot: id, session: session, video: q.Get("video") == "1", full: q.Get("size") == "640x480", audio: q.Get("audio") == "1", imu: q.Get("imu") == "1", touch: q.Get("touch") == "1", light: q.Get("light") == "1",
		out: make(chan []byte, 8)}

	s.mu.Lock()
	paired := s.sessions[session][id]
	s.mu.Unlock()
	if !paired {
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	}
	tl := tierTable[s.sessionTier(session, time.Now())]
	sub.full = sub.full && tl.FullVideo // 640x480 from tier 3 up; others get 320x240
	// The tier's limit is for video and audio; the sensor streams (IMU, touch, light: a few KB/s)
	// and quiet sockets have their own, larger allowance, so they do not use up the video.
	kind, perSession := "media", tl.MediaPerRobot
	if !sub.video && !sub.audio { // sensors, or a quiet socket (speaker, an encrypted robot's snapshot)
		kind, perSession = "sensors", maxSensorsPerRobot
	}
	keys := s.streamKeys(kind, session+" "+id, s.clientIP(r), perSession, maxMediaPerAddr)
	if !s.streams.acquire(keys) {
		// Said on the socket: a browser cannot read the answer to a refused upgrade, it would only
		// see the socket close and try again.
		if ws, err := browserUpgrader.Upgrade(w, r, nil); err == nil {
			ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(closeTooMany, s.tooManyText(session, kind)),
				time.Now().Add(writeWait))
			ws.Close()
		}
		return
	}
	defer s.streams.release(keys)
	ws, err := browserUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already answered (e.g. 403 for a foreign origin)
	}
	defer ws.Close()

	s.mu.Lock()
	s.media[sub] = struct{}{}
	s.syncMedia(id)
	s.mu.Unlock()
	s.log.Info("media viewer joined", "robot", id, "video", sub.video, "full", sub.full, "audio", sub.audio)
	defer func() {
		s.mu.Lock()
		delete(s.media, sub)
		s.syncMedia(id)
		s.mu.Unlock()
		s.mu.Lock()
		dropped := sub.dropped
		s.mu.Unlock()
		s.log.Info("media viewer left", "robot", id, "sent", sub.sent, "dropped", dropped)
	}()

	// The browser may send speaker audio (BinSpeakerPCM); reading also detects
	// that it went away.
	ws.SetReadLimit(maxSpeakerMessage)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		logged := false
		for {
			kind, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage && len(msg) > 0 && msg[0] == e2e.BinBrowser {
				s.forwardE2EBinary(id, msg) // sealed speaker audio
				continue
			}
			if kind != websocket.BinaryMessage || len(msg) < 4 || msg[0] != wire.BinSpeakerPCM {
				continue
			}
			if s.forwardSpeaker(id, msg) && !logged {
				logged = true
				s.log.Info("speaker audio from browser", "robot", id)
			}
		}
	}()
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	for {
		select {
		case <-gone:
			return
		case msg := <-sub.out:
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				s.log.Info("media viewer write failed", "robot", id, "err", err)
				return
			}
			sub.sent++
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

// e2eNoBinary: an encrypted robot refuses plaintext pictures and files from the relay (it could
// inject them); sealed ones come later (i41eu-arch docs/e2ee.md §7).
const e2eNoBinary = "this robot is end-to-end encrypted: pictures and files to it are not available yet"

// handlePicture sends a JPEG (scaled to 320x240 by the browser) to a paired
// robot, which shows it instead of the face until the "face" command.
func (s *Server) handlePicture(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	if !s.commandAllowed(session, time.Now()) {
		s.tooMany(w, session, "too many requests")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "image/jpeg" {
		http.Error(w, "Content-Type must be image/jpeg", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPictureBytes))
	if err != nil || !bytes.HasPrefix(body, []byte{0xFF, 0xD8}) {
		http.Error(w, "body must be a JPEG up to 192 KB", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	paired := s.sessions[session][id]
	st := s.robots[id]
	var conn *robotConn
	var supported, encrypted bool
	if st != nil {
		conn = st.conn
		supported = slices.Contains(st.caps.Commands, "image")
		encrypted = st.labels["e2e"] == "1"
	}
	s.mu.Unlock()
	switch {
	case !paired || st == nil:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	case encrypted:
		http.Error(w, e2eNoBinary, http.StatusConflict)
		return
	case !supported:
		http.Error(w, "robot does not show pictures", http.StatusBadRequest)
		return
	case conn == nil:
		http.Error(w, "robot is offline", http.StatusConflict)
		return
	}
	if !conn.enqueueBinary(append([]byte{wire.BinShowJPEG}, body...)) {
		http.Error(w, "robot is not accepting commands", http.StatusServiceUnavailable)
		return
	}
	s.log.Info("picture sent", "robot", id, "bytes", len(body))
	s.commandSent(id, "picture", map[string]any{"bytes": len(body)})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

// maxSpeakerMessage bounds one browser audio message (100 ms at 24 kHz is ~4.8 KB).
const maxSpeakerMessage = 64 << 10

// forwardSpeaker sends browser audio to the robot's speaker, if it has one and
// is online. Audio is dropped when the robot's queue is full: late audio is
// worse than a gap.
func (s *Server) forwardSpeaker(robotID string, msg []byte) bool {
	s.mu.Lock()
	st := s.robots[robotID]
	var conn *robotConn
	if st != nil && slices.Contains(st.caps.Commands, "speaker") {
		conn = st.conn
	}
	s.mu.Unlock()
	return conn != nil && conn.enqueueBinary(msg)
}

// logMediaStats logs what the robot streamed during the last period while the
// camera or microphone is on, and warns when a stream is on but nothing came.
func (s *Server) logMediaStats(robotID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[robotID]
	if st == nil {
		return
	}
	if st.mediaStatsAt.IsZero() { // first check: start the first period now
		st.mediaStatsAt = time.Now()
		return
	}
	elapsed := time.Since(st.mediaStatsAt)
	if elapsed < mediaStatsPeriod {
		return
	}
	frames, bytes := st.mediaFrames, st.mediaBytes
	st.mediaFrames, st.mediaBytes, st.mediaStatsAt = [5]int{}, [5]int{}, time.Now()
	if !st.cameraOn && !st.micOn && !st.imuOn && !st.touchOn && !st.lightOn && frames == [5]int{} {
		return
	}
	secs := elapsed.Seconds()
	attrs := []any{"robot", robotID, "camera", st.cameraOn, "mic", st.micOn,
		"video_fps", round1(float64(frames[0]) / secs), "video_kbps", round1(float64(bytes[0]) / 1024 / secs),
		"audio_msgs", frames[1], "audio_kbps", round1(float64(bytes[1]) / 1024 / secs),
		"imu", st.imuOn, "imu_msgs", frames[2], "touch", st.touchOn, "touch_msgs", frames[3], "light", st.lightOn, "light_msgs", frames[4]}
	if (st.cameraOn && frames[0] == 0) || (st.micOn && frames[1] == 0) || (st.imuOn && frames[2] == 0) {
		s.log.Warn("media stalled: robot sends nothing for a stream that is on", attrs...)
		return
	}
	s.log.Info("media from robot", attrs...)
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// storeSnapshot keeps a robot's latest full-resolution still and tells its paired browsers.
func (s *Server) storeSnapshot(robotID string, jpeg []byte) {
	if !bytes.HasPrefix(jpeg, []byte{0xFF, 0xD8}) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[robotID]
	if st == nil {
		return
	}
	st.snapshot = bytes.Clone(jpeg)
	st.snapshotAt = time.Now()
	s.publish(robotID, "", sseEvent{name: "snapshot", data: mustJSON(map[string]any{
		"robot": robotID, "bytes": len(jpeg), "ts": st.snapshotAt,
	})})
	s.log.Info("snapshot", "robot", robotID, "bytes", len(jpeg))
}

// handleSnapshot serves a paired robot's latest full-resolution still.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	s.mu.Lock()
	paired := s.sessions[session][id]
	var jpeg []byte
	if st := s.robots[id]; st != nil {
		jpeg = st.snapshot
	}
	s.mu.Unlock()
	switch {
	case !paired:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
	case jpeg == nil:
		http.Error(w, "no snapshot yet", http.StatusNotFound)
	default:
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(jpeg)
	}
}
