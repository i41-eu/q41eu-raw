package server

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/wire"
)

// Liveness timing, as in Rancher's remotedialer.
const (
	pingPeriod      = 5 * time.Second
	pongWait        = 60 * time.Second
	writeWait       = 10 * time.Second
	registerTimeout = 10 * time.Second
	maxMessageBytes = 256 << 10 // camera frames and pictures travel as binary messages
)

var robotIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Robots authenticate with a bearer token, not cookies, so a cross-origin
	// browser page gains nothing by opening this socket.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// outMsg is one queued WebSocket message to a robot.
type outMsg struct {
	binary bool
	data   []byte
}

type robotConn struct {
	id     string
	ws     *websocket.Conn
	send   chan outMsg
	limits *guestLimits // invited robots only; nil for the owner's robots

	mgrToken string // a token from the manager: its paired browsers go there (manager.go)

	closeOnce sync.Once
	done      chan struct{}
}

func (c *robotConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

// enqueue queues a JSON frame for the writer; false if the robot is gone or stuck.
func (c *robotConn) enqueue(frame []byte) bool {
	return c.queue(outMsg{data: frame})
}

// enqueueBinary queues a binary message (type byte + payload, see wire.Bin*).
func (c *robotConn) enqueueBinary(msg []byte) bool {
	return c.queue(outMsg{binary: true, data: msg})
}

func (c *robotConn) queue(m outMsg) bool {
	select {
	case <-c.done:
		return false
	case c.send <- m:
		return true
	default:
		return false
	}
}

func (s *Server) handleRobotConnect(w http.ResponseWriter, r *http.Request) {
	ip, now := s.clientIP(r), time.Now()
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id := wire.DeviceID(r.Header)
	if !robotIDPattern.MatchString(id) {
		http.Error(w, "missing or invalid "+wire.DeviceIDHeader, http.StatusBadRequest)
		return
	}
	// Per address and robot id: guessing one robot's token gets blocked, other robots behind
	// the same address (a home NAT) keep working.
	failKey := ip + " " + id
	if s.robotFails.blocked(failKey, now) {
		http.Error(w, "too many failed logins for this robot from this address, try again later", http.StatusTooManyRequests)
		return
	}
	ok, guest := s.robotAuth(r.Context(), id, token)
	if !ok {
		s.robotFails.fail(failKey, now)
		s.log.Info("robot unauthorized", "robot", id, "remote", ip)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("robot upgrade failed", "robot", id, "err", err)
		return
	}
	ws.SetReadLimit(maxMessageBytes)
	c := &robotConn{id: id, ws: ws, send: make(chan outMsg, 32), done: make(chan struct{})}
	if guest && s.manager != nil {
		c.mgrToken = token
	}
	if guest {
		c.limits = newGuestLimits(now)
	} else {
		s.mu.Lock()
		if !s.ownerRobots[id] {
			s.ownerRobots[id] = true // the owner's robot: no account can claim this id
			s.requestSave()
		}
		s.mu.Unlock()
	}
	defer c.close()

	reg, reason := readRegister(ws, id)
	if reason != "" {
		s.log.Warn("robot rejected", "robot", id, "reason", reason, "remote", s.clientIP(r))
		if f, err := wire.Marshal(wire.KindRejected, wire.Meta{}, wire.RejectedBody{Reason: reason}); err == nil {
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			ws.WriteMessage(websocket.TextMessage, f)
		}
		return
	}

	s.attach(c, reg)
	defer s.detach(c)
	s.log.Info("robot connected", "robot", id, "model", reg.Capabilities.Model,
		"firmware", reg.Capabilities.Firmware, "guest", guest, "remote", s.clientIP(r))
	defer s.log.Info("robot disconnected", "robot", id)

	// Raw data's screen on the robot is its own readings, not a face.
	if f, err := wire.Marshal(wire.KindAccepted, wire.Meta{WorkerID: id, SessionID: newCode()}, wire.AcceptedBody{View: "readings"}); err == nil {
		c.enqueue(f)
	}
	s.reportSeen(r.Context(), c)
	s.sendPairCode(c)
	// Browsers paired before (pairings survive restarts): tell the robot right away, so
	// it starts with its face instead of the QR screen.
	s.mu.Lock()
	n, watching := s.viewerCountLocked(id), s.watchingLocked(id)
	s.mu.Unlock()
	if n > 0 {
		if f, err := wire.Marshal(wire.KindPaired, wire.Meta{WorkerID: id}, wire.PairedBody{Viewers: n, Watching: watching, Reconnect: true}); err == nil {
			c.enqueue(f)
		}
	}
	// Turn camera and mic back on if browsers were already watching.
	s.mu.Lock()
	s.syncMedia(id)
	s.mu.Unlock()

	go s.writeLoop(c)
	s.readLoop(c)
}

// readRegister waits for the mandatory first Register frame.
func readRegister(ws *websocket.Conn, id string) (wire.RegisterBody, string) {
	var reg wire.RegisterBody
	ws.SetReadDeadline(time.Now().Add(registerTimeout))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return reg, "no Register frame"
	}
	frames, err := wire.Parse(data)
	if err != nil || len(frames) == 0 || frames[0].Kind != wire.KindRegister {
		return reg, "first frame must be Register"
	}
	f := frames[0]
	if f.Meta.WorkerID != "" && f.Meta.WorkerID != id {
		return reg, "meta.worker_id does not match " + wire.DeviceIDHeader
	}
	if err := f.Decode(&reg); err != nil {
		return reg, "invalid Register body"
	}
	if reg.Class != wire.ClassRobot {
		return reg, "unknown worker class"
	}
	return reg, ""
}

func (s *Server) sendPairCode(c *robotConn) {
	body := s.issueCode(c.id)
	if f, err := wire.Marshal(wire.KindPairCode, wire.Meta{WorkerID: c.id}, body); err == nil {
		c.enqueue(f)
	}
}

// writeLoop owns all writes to the socket: queued frames, pings, and
// replacing the pairing code before it expires.
func (s *Server) writeLoop(c *robotConn) {
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	rotate := time.NewTicker(s.cfg.PairTTL)
	defer rotate.Stop()
	for {
		select {
		case <-c.done:
			return
		case m := <-c.send:
			kind := websocket.TextMessage
			if m.binary {
				kind = websocket.BinaryMessage
			}
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(kind, m.data); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				c.close()
				return
			}
			s.logMediaStats(c.id)
		case <-rotate.C:
			s.sendPairCode(c)
		}
	}
}

func (s *Server) readLoop(c *robotConn) {
	resetDeadline := func() { c.ws.SetReadDeadline(time.Now().Add(pongWait)) }
	resetDeadline()
	c.ws.SetPongHandler(func(string) error { resetDeadline(); return nil })
	c.ws.SetPingHandler(func(data string) error {
		resetDeadline()
		return c.ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
	})

	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		resetDeadline()
		if c.limits != nil && !c.limits.allow(len(data), time.Now()) {
			s.log.Warn("invited robot over its limits, disconnecting", "robot", c.id)
			c.ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "over the limits for invited robots"),
				time.Now().Add(writeWait))
			return
		}
		if kind == websocket.BinaryMessage {
			s.relayMedia(c.id, data)
			continue
		}
		frames, err := wire.Parse(data)
		if err != nil {
			s.log.Warn("bad frame from robot", "robot", c.id, "err", err)
			continue
		}
		for _, f := range frames {
			s.handleRobotFrame(c, f)
		}
	}
}

func (s *Server) handleRobotFrame(c *robotConn, f wire.Frame) {
	switch f.Kind {
	case wire.KindHeartbeat:
		s.touch(c.id)
	case wire.KindRobotEvent:
		var body wire.RobotEventBody
		if err := f.Decode(&body); err != nil || body.Name == "" {
			s.log.Warn("bad robot event", "robot", c.id, "err", err)
			return
		}
		s.log.Info("robot event", "robot", c.id, "name", body.Name)
		s.robotEvent(c.id, body)
	case wire.KindRobotPong:
		var body wire.RobotPongBody
		if err := f.Decode(&body); err != nil {
			s.log.Warn("bad pong", "robot", c.id, "err", err)
			return
		}
		s.finishPing(c.id, body)
	case wire.KindRobotTelemetry:
		var body wire.RobotTelemetryBody
		if err := f.Decode(&body); err != nil {
			s.log.Warn("bad telemetry", "robot", c.id, "err", err)
			return
		}
		s.setTelemetry(c.id, body.Measurements)
	case wire.KindE2EGroupKey, wire.KindE2EData:
		s.relayE2E(c.id, f) // sealed: relayed, never read (e2erelay.go)
	default:
		// Unknown kinds are ignored for forward compatibility.
		s.log.Debug("ignoring frame", "robot", c.id, "kind", f.Kind)
	}
}

// clientIP is the robot's address for logs: behind the TLS gateway RemoteAddr
// is the gateway, and the real client is the first X-Forwarded-For entry.
