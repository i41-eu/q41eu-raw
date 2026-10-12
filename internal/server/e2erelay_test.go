package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/e2e"
	"github.com/i41-eu/q41eu-raw/wire"
)

var rawB64 = base64.RawURLEncoding

// readKind reads robot frames until one of kind arrives and returns its raw body.
func (r *testRobot) readKind(kind string) json.RawMessage {
	r.t.Helper()
	r.ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		mt, data, err := r.ws.ReadMessage()
		if err != nil {
			r.t.Fatalf("waiting for %s: %v", kind, err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		frames, _ := wire.Parse(data)
		for _, f := range frames {
			if f.Kind == kind {
				return f.Body
			}
		}
	}
}

func (r *testRobot) sendRaw(kind string, body any) {
	r.t.Helper()
	f, _ := wire.Marshal(kind, wire.Meta{}, body)
	if err := r.ws.WriteMessage(websocket.TextMessage, f); err != nil {
		r.t.Fatal(err)
	}
}

func postE2E(t *testing.T, c *http.Client, url, kind string, body any) int {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"kind": kind, "body": body})
	resp, err := c.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// waitE2E reads SSE until an "e2e" event of kind arrives.
func waitE2E(t *testing.T, events <-chan sseMsg, kind string) (json.RawMessage, []byte) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case m := <-events:
			if m.name != "e2e" {
				continue
			}
			var e struct {
				Kind string          `json:"kind"`
				Body json.RawMessage `json:"body"`
			}
			json.Unmarshal(m.data, &e)
			if e.Kind == kind {
				return e.Body, m.data
			}
		case <-timeout:
			t.Fatalf("no e2e %s event", kind)
		}
	}
}

// TestE2ERelay runs the whole design through the real server: enrollment through the
// QR fragment, the group key, sealed telemetry and video to the browser, a sealed command
// to the robot, and checks the server saw none of it in the clear.
func TestE2ERelay(t *testing.T) {
	ts, s := newTestServer(t)
	const id = "chan-1"
	robot := connectRobot(t, ts, id, wire.ClassRobot)
	browser := pairBrowser(t, robot)
	events := openEvents(t, browser, ts)

	// Robot side: its key, a pairing secret in its QR fragment, a group key.
	rKey, _ := e2e.NewKey()
	p := e2e.NewSecret()
	fragment := e2e.Fragment(rKey.PublicKey(), p)
	g := append(e2e.NewSecret(), e2e.NewSecret()...)
	const epoch = 1
	nonces := e2e.NewNonces()

	// Browser side: reads the fragment (never sent to the server), enrolls its key.
	rPub, gotP, err := e2e.ParseFragment(fragment)
	if err != nil {
		t.Fatal(err)
	}
	bKey, _ := e2e.NewKey()
	enroll := wire.E2EEnrollBody{B: e2e.EncodePublic(bKey.PublicKey()),
		MAC: rawB64.EncodeToString(e2e.EnrollMAC(gotP, rPub, bKey.PublicKey()))}
	url := ts.URL + "/api/robots/" + id + "/e2e"
	if code := postE2E(t, browser, url, wire.KindE2EEnroll, enroll); code != http.StatusAccepted {
		t.Fatalf("enroll relay: %d", code)
	}

	// The robot checks the MAC, derives K_B and sends the group key.
	var got wire.E2EEnrollBody
	json.Unmarshal(robot.readKind(wire.KindE2EEnroll), &got)
	bPub, err := e2e.ParsePublic(got.B)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2e.CheckEnroll([][]byte{p}, rKey.PublicKey(), bPub, got.MAC); err != nil {
		t.Fatal("robot refused a valid enrollment")
	}
	kRobot, _ := e2e.Pairwise(rKey, bPub, rKey.PublicKey(), bPub)
	gk, _ := e2e.SealJSON(kRobot, nonces.Next(), g, e2e.GroupAAD(id, epoch))
	gk.B, gk.Epoch = e2e.BrowserID(bPub), epoch
	robot.sendRaw(wire.KindE2EGroupKey, gk)

	// The browser gets the group key.
	body, raw := waitE2E(t, events, wire.KindE2EGroupKey)
	var sealedG e2e.Sealed
	json.Unmarshal(body, &sealedG)
	kBrowser, _ := e2e.Pairwise(bKey, rPub, rPub, bKey.PublicKey())
	gotG, err := e2e.OpenJSON(kBrowser, sealedG, e2e.GroupAAD(id, sealedG.Epoch))
	if err != nil || !bytes.Equal(gotG, g) {
		t.Fatalf("browser could not open the group key: %v", err)
	}
	if bytes.Contains(raw, []byte(rawB64.EncodeToString(g))) {
		t.Fatal("the group key passed the server in the clear")
	}

	// Sealed telemetry: relayed, not read.
	const secretValue = 42.4242
	tele, _ := json.Marshal(wire.RobotTelemetryBody{Measurements: map[string]float64{"battery_v": secretValue}})
	data, _ := e2e.SealJSON(g, nonces.Next(), tele, []byte(id))
	data.Epoch = epoch
	robot.sendRaw(wire.KindE2EData, data)
	body, raw = waitE2E(t, events, wire.KindE2EData)
	if bytes.Contains(raw, []byte("42.4242")) || bytes.Contains(raw, []byte("battery_v")) {
		t.Fatal("telemetry passed the server in the clear")
	}
	var sealedData e2e.Sealed
	json.Unmarshal(body, &sealedData)
	plain, err := e2e.OpenJSON(gotG, sealedData, []byte(id))
	if err != nil || !strings.Contains(string(plain), "42.4242") {
		t.Fatalf("browser could not open telemetry: %v", err)
	}
	s.mu.Lock()
	_, stored := s.robots[id].telemetry["battery_v"]
	s.mu.Unlock()
	if stored {
		t.Fatal("the server parsed sealed telemetry")
	}

	// Sealed video: binary 0x30 to the browser's media socket.
	ws, _, err := dialMedia(ts, browser, id, "video=1", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	time.Sleep(100 * time.Millisecond) // the subscription is registered
	frame := []byte("JPEG secret frame")
	bin, _ := e2e.SealBinary(g, epoch, nonces.Next(), id, wire.BinCameraJPEG, frame)
	if err := robot.ws.WriteMessage(websocket.BinaryMessage, bin); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("no sealed frame: %v", err)
		}
		if len(msg) == 0 || msg[0] != e2e.BinGroup {
			continue
		}
		inner, payload, err := e2e.OpenBinary(func(uint32) []byte { return gotG }, id, msg)
		if err != nil || inner != wire.BinCameraJPEG || !bytes.Equal(payload, frame) {
			t.Fatalf("browser could not open the frame: %v", err)
		}
		break
	}

	// A sealed command to the robot; a replay is refused by the robot.
	cmd, _ := json.Marshal(map[string]any{"command": "nod", "args": map[string]any{}, "seq": 1})
	sealedCmd, _ := e2e.SealJSON(kBrowser, e2e.NewNonces().Next(), cmd, []byte(id))
	sealedCmd.B = e2e.BrowserID(bKey.PublicKey())
	if code := postE2E(t, browser, url, wire.KindE2ECommand, sealedCmd); code != http.StatusAccepted {
		t.Fatalf("command relay: %d", code)
	}
	var atRobot e2e.Sealed
	json.Unmarshal(robot.readKind(wire.KindE2ECommand), &atRobot)
	plainCmd, err := e2e.OpenJSON(kRobot, atRobot, []byte(id))
	if err != nil || !strings.Contains(string(plainCmd), `"nod"`) {
		t.Fatalf("robot could not open the command: %v", err)
	}
	seqs := e2e.NewSeqs()
	if seqs.Accept(atRobot.B, 1) != nil || seqs.Accept(atRobot.B, 1) == nil {
		t.Fatal("replay protection")
	}

	// Only paired browsers may send, and only E2E kinds.
	stranger := newBrowser()
	if code := postE2E(t, stranger, url, wire.KindE2ECommand, sealedCmd); code != http.StatusForbidden {
		t.Fatalf("unpaired browser: %d", code)
	}
	if code := postE2E(t, browser, url, wire.KindRobotCommand, map[string]any{"command": "nod"}); code != http.StatusBadRequest {
		t.Fatalf("plain kind through the e2e endpoint: %d", code)
	}
}

// An encrypted robot would drop a plaintext picture or file from the relay: the relay refuses it
// at once, with the reason.
func TestE2ENoPlaintextPictures(t *testing.T) {
	ts, _ := newTestServer(t)
	ws, _, err := dialRobot(ts, testToken, "chan-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	robot := &testRobot{t: t, ws: ws}
	robot.send(wire.KindRegister, wire.RegisterBody{Class: wire.ClassRobot, Labels: map[string]string{"e2e": "1"},
		Capabilities: wire.RobotCapabilities{Model: "test", Commands: []string{"image", "assets"}}})
	browser := pairBrowser(t, robot)
	for _, path := range []string{"/picture", "/assets?name=a.png"} {
		req, _ := http.NewRequest("POST", ts.URL+"/api/robots/chan-1"+path, strings.NewReader("\xff\xd8 jpeg"))
		req.Header.Set("Origin", ts.URL)
		req.Header.Set("X-Stackchan-Upload", "1")
		req.Header.Set("Content-Type", map[bool]string{true: "image/jpeg", false: "application/octet-stream"}[path == "/picture"])
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "end-to-end encrypted") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
}
