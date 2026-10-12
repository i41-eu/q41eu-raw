// Command fake-robot plays the Stackchan side of the protocol so the server
// and dashboard can be tested without hardware: it registers, prints the
// pairing URL, streams telemetry, answers the basic command set (including
// ping), and emits an occasional robot event.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/wire"
)

// Same command set as the firmware (see wire.RobotCommandBody).
var commands = []string{"ping", "nod", "shake", "look", "home", "emotion", "say", "leds", "brightness", "volume",
	"sticker", "face", "image", "camera", "mic", "screensaver", "standby", "speaker", "nfc", "ir_send", "power_led",
	"hold", "servo_power", "rotate", "snapshot", "camera_config", "camera_reg", "imu_stream", "touch_stream",
	"server_switch", "assets", "asset_delete", "view"}

func main() {
	var (
		serverURL = flag.String("server", "ws://127.0.0.1:8765", "server base URL (ws:// or wss://)")
		id        = flag.String("id", "fake-robot-1", "robot id")
		tokenFile = flag.String("token-file", defaultTokenFile(), "file with the robot bearer token")
		interval  = flag.Duration("telemetry", 2*time.Second, "telemetry interval")
		events    = flag.Duration("events", 20*time.Second, "interval of fake robot events (0 disables)")
		e2eOn     = flag.Bool("e2e", false, "end-to-end encryption (i41eu-arch docs/e2ee.md): seal everything for enrolled browsers")
		e2eState  = flag.String("e2e-state", "", "file for the robot key and enrolled browsers (default ~/.config/stackchan-server/<id>-e2e.json)")
		camOff    = flag.Bool("camera-off", false, "camera and microphone off on the robot (its privacy setting): refused, reported")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	b, err := os.ReadFile(*tokenFile)
	if err != nil {
		log.Error("read token (start q41eu-raw once to generate it)", "err", err)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(b))
	url := strings.TrimRight(*serverURL, "/") + wire.ConnectPath

	r := &robot{started: time.Now(), battery: 87, pitch: 45, brightness: 60, volume: 50, nfcOn: true, log: log, camOff: *camOff}
	if *e2eOn {
		path := *e2eState
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".config", "stackchan-server", *id+"-e2e.json")
		}
		if r.e2e, err = newE2ERobot(*id, path, log); err != nil {
			log.Error("e2e state", "file", path, "err", err)
			os.Exit(1)
		}
		log.Info("end-to-end encryption on", "state", path, "epoch", r.e2e.epoch, "enrolled", len(r.e2e.enrolled))
	}
	// Reconnect with exponential backoff 1 s -> 30 s.
	backoff := time.Second
	for {
		start := time.Now()
		err := r.run(url, token, *id, *interval, *events)
		var sb standbyError
		if errors.As(err, &sb) {
			log.Info("standby: offline", "for", sb.d)
			time.Sleep(sb.d)
			r.afterStandby = true
			backoff = time.Second
			continue
		}
		log.Warn("disconnected", "err", err)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

// robot is the simulated state that commands change and telemetry reports.
type robot struct {
	assets              map[string]int // the file store: name -> size (seeded; no uploads)
	started             time.Time
	battery, yaw, pitch float64
	brightness, volume  float64
	cameraOn, micOn     bool
	nfcOn               bool             // NFC polling, on by default like the firmware
	screensaver         float64          // 0 off, 1 auto, 2 manual
	afterStandby        bool             // report standby_end after the next connect
	camOff              bool             // camera and microphone off on the robot (-camera-off)
	servers             []map[string]any // like the firmware's server list (no switching here)
	serverDefault       string
	frame               int     // camera frames sent, drives the test pattern
	phase               float64 // microphone tone phase
	log                 *slog.Logger
	e2e                 *e2eRobot // nil: plaintext
}

type received struct {
	cmd     wire.RobotCommandBody
	at      time.Time
	e2eKind string          // an E2E frame from a browser, instead of cmd
	e2eBody json.RawMessage // its body
}

func (r *robot) run(url, token, id string, interval, eventEvery time.Duration) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set(wire.DeviceIDHeader, id)
	ws, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %w (HTTP %s)", err, resp.Status)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.Close()

	plainSend := func(kind string, body any) error {
		f, err := wire.Marshal(kind, wire.Meta{}, body)
		if err != nil {
			return err
		}
		return ws.WriteMessage(websocket.TextMessage, f)
	}
	// With e2e, what the robot reports goes sealed under the group key.
	send := plainSend
	if r.e2e != nil {
		send = func(kind string, body any) error {
			switch kind {
			case wire.KindRobotTelemetry, wire.KindRobotEvent, wire.KindRobotPong:
				s, err := r.e2e.sealFrame(kind, body)
				if err != nil {
					return err
				}
				return plainSend(wire.KindE2EData, s)
			}
			return plainSend(kind, body)
		}
	}

	reg := wire.RegisterBody{
		Class: wire.ClassRobot,
		Capabilities: wire.RobotCapabilities{
			Model:    "fake",
			Firmware: "fake-robot",
			Commands: commands,
			Measurements: []string{"battery_pct", "charging", "head_yaw_deg", "head_pitch_deg",
				"wifi_rssi_dbm", "free_heap_kb", "uptime_s", "brightness_pct", "volume_pct", "screensaver"},
		},
	}
	if r.e2e != nil {
		reg.Labels = map[string]string{"e2e": "1"}
	}
	if err := send(wire.KindRegister, reg); err != nil {
		return err
	}
	if err := send(wire.KindRobotEvent, r.serversEvent(strings.TrimSuffix(url, wire.ConnectPath))); err != nil {
		return err
	}
	if r.camOff { // as the firmware: its privacy setting after connecting
		if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "privacy", Data: map[string]any{"camera_mic": "off",
			"reason": "off on the robot", "mode": "off", "night": "22:00-07:00"}}); err != nil {
			return err
		}
	}
	if r.afterStandby {
		r.afterStandby, r.screensaver = false, 0
		if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "standby_end", Data: map[string]any{"touched": 0}}); err != nil {
			return err
		}
	}

	// gorilla allows one concurrent writer: the reader hands commands to this loop.
	errc := make(chan error, 1)
	cmds := make(chan received, 16)
	go func() { errc <- readLoop(ws, cmds, r.log, r.e2e) }()

	telemetry := time.NewTicker(interval)
	defer telemetry.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	var eventTick <-chan time.Time
	if eventEvery > 0 {
		t := time.NewTicker(eventEvery)
		defer t.Stop()
		eventTick = t.C
	}
	fakeEvents := []string{"head_press", "head_swipe_forward", "shake", "screen_tap", "nfc_tag", "ir_received"}
	videoTick := time.NewTicker(200 * time.Millisecond) // 5 fps, like the firmware
	defer videoTick.Stop()
	audioTick := time.NewTicker(40 * time.Millisecond) // 40 ms PCM chunks
	defer audioTick.Stop()
	sendBinary := func(msg []byte) error {
		if r.e2e != nil {
			sealed, err := r.e2e.sealBinary(msg)
			if err != nil {
				return err
			}
			msg = sealed
		}
		return ws.WriteMessage(websocket.BinaryMessage, msg)
	}

	for {
		var err error
		select {
		case err = <-errc:
			return err
		case c := <-cmds:
			if c.e2eKind != "" {
				switch c.e2eKind {
				case wire.KindE2EEnroll, wire.KindE2EHello:
					var f wire.Frame
					if c.e2eKind == wire.KindE2EEnroll {
						f, err = r.e2e.handleEnroll(c.e2eBody)
					} else {
						f, err = r.e2e.handleHello(c.e2eBody)
					}
					if err != nil {
						r.log.Warn("e2e "+c.e2eKind+" refused", "err", err)
						err = nil
						break
					}
					err = plainSend(f.Kind, f.Body)
				case wire.KindE2ECommand:
					cmd, oerr := r.e2e.openCommand(c.e2eBody)
					if oerr != nil {
						r.log.Warn("sealed command refused", "err", oerr)
						break
					}
					c.cmd, c.e2eKind = cmd, ""
				}
				if c.e2eKind != "" || err != nil {
					break
				}
			} else if r.e2e != nil && !relaySwitches[c.cmd.Command] {
				r.log.Warn("plaintext command refused (e2e on)", "command", c.cmd.Command)
				break
			}
			if c.cmd.Command == "snapshot" { // the test pattern as the "full resolution" still
				frame := r.cameraFrame()
				frame[0] = wire.BinSnapshot
				err = sendBinary(frame)
				break
			}
			err = r.handle(c, send)
		case <-telemetry.C:
			r.battery = max(0, r.battery-0.01)
			err = send(wire.KindRobotTelemetry, wire.RobotTelemetryBody{Measurements: r.telemetry()})
		case <-heartbeat.C:
			err = send(wire.KindHeartbeat, nil)
		case <-eventTick:
			ev := wire.RobotEventBody{Name: fakeEvents[rand.IntN(len(fakeEvents))]}
			switch {
			case ev.Name == "screen_tap":
				ev.Data = map[string]any{"x": rand.IntN(320), "y": rand.IntN(240)}
			case ev.Name == "nfc_tag" && r.nfcOn:
				ev.Data = map[string]any{"uid": "04:A2:3B:1C:5D:80:00", "type": "type2", "atqa": 68, "sak": 0,
					"text": "https://github.com/i41-eu/q41eu-raw"}
			case ev.Name == "nfc_tag":
				ev.Name = "shake"
			case ev.Name == "ir_received":
				ev.Data = map[string]any{"protocol": "nec", "address": 4, "command": 8, "raw": "9000,4500,562,562"}
			}
			r.log.Info("robot event", "name", ev.Name, "data", ev.Data)
			err = send(wire.KindRobotEvent, ev)
		case <-videoTick.C:
			if r.cameraOn {
				err = sendBinary(r.cameraFrame())
			}
		case <-audioTick.C:
			if r.micOn {
				err = sendBinary(r.micChunk(40 * time.Millisecond))
			}
		}
		if err != nil {
			return err
		}
	}
}

func (r *robot) handle(c received, send func(string, any) error) error {
	num := func(key string) (float64, bool) {
		v, ok := c.cmd.Args[key].(float64)
		return v, ok
	}
	switch c.cmd.Command {
	case "ping":
		id, _ := c.cmd.Args["id"].(string)
		return send(wire.KindRobotPong, wire.RobotPongBody{
			ID: id, QueueMs: float64(time.Since(c.at).Microseconds()) / 1000,
		})
	case "look":
		if v, ok := num("yaw"); ok {
			r.yaw = max(-128, min(128, v))
		}
		if v, ok := num("pitch"); ok {
			r.pitch = max(5, min(85, v))
		}
	case "home":
		r.yaw, r.pitch = 0, 45
	case "standby":
		// Like the firmware: report it, then go offline for the given minutes
		// (fractions allowed here, for quick tests with curl).
		minutes, ok := num("minutes")
		if !ok || minutes <= 0 {
			minutes = 5
		}
		r.cameraOn, r.micOn, r.screensaver = false, false, 2
		if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "standby", Data: map[string]any{"minutes": minutes}}); err != nil {
			return err
		}
		return standbyError{time.Duration(minutes * float64(time.Minute))}
	case "screensaver":
		// Like the firmware: the command blanks "manually" (2) or wakes (0).
		on, _ := c.cmd.Args["on"].(bool)
		switch {
		case on && r.screensaver == 0:
			r.screensaver = 2
			if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "screensaver_on", Data: map[string]any{"manual": 1}}); err != nil {
				return err
			}
		case !on && r.screensaver != 0:
			r.screensaver = 0
			if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "screensaver_off"}); err != nil {
				return err
			}
		}
	case "assets":
		return send(wire.KindRobotEvent, r.assetsEvent())
	case "asset_delete":
		name, _ := c.cmd.Args["name"].(string)
		delete(r.assets, name)
		if err := send(wire.KindRobotEvent, wire.RobotEventBody{Name: "asset_deleted", Data: map[string]any{"name": name}}); err != nil {
			return err
		}
		return send(wire.KindRobotEvent, r.assetsEvent())
	case "camera", "mic":
		on, _ := c.cmd.Args["on"].(bool)
		if on && r.camOff {
			return send(wire.KindRobotEvent, wire.RobotEventBody{Name: "privacy_refused", Data: map[string]any{"command": c.cmd.Command, "reason": "off on the robot"}})
		}
		if c.cmd.Command == "camera" {
			r.cameraOn = on
		} else {
			r.micOn = on
		}
	case "nfc":
		r.nfcOn, _ = c.cmd.Args["on"].(bool)
	case "brightness":
		if v, ok := num("value"); ok {
			r.brightness = max(1, min(100, v))
		}
	case "volume":
		if v, ok := num("value"); ok {
			r.volume = max(0, min(100, v))
		}
	}
	r.log.Info("command received", "command", c.cmd.Command, "args", c.cmd.Args)
	return send(wire.KindRobotTelemetry, wire.RobotTelemetryBody{Measurements: r.telemetry()})
}

// assetsEvent: the file store as the firmware reports it (the list as a JSON string).
func (r *robot) assetsEvent() wire.RobotEventBody {
	if r.assets == nil {
		r.assets = map[string]int{}
		for i := range 40 {
			ext := []string{"png", "jpg", "wav"}[i%3]
			r.assets[fmt.Sprintf("pet/item-%02d.%s", i, ext)] = 2000 + i*731%9000
		}
	}
	type file struct {
		Name  string `json:"name"`
		Bytes int    `json:"bytes"`
		CRC   uint32 `json:"crc"`
	}
	files, used := []file{}, 0
	for name, n := range r.assets {
		files = append(files, file{name, n, crc32.ChecksumIEEE([]byte(name))})
		used += n
	}
	list, _ := json.Marshal(map[string]any{"files": files})
	const total = 1932 * 1024
	return wire.RobotEventBody{Name: "assets", Data: map[string]any{"list": string(list), "free": total - used, "total": total, "mounted": 1}}
}

func (r *robot) telemetry() map[string]float64 {
	return map[string]float64{
		"battery_pct":    round1(r.battery),
		"charging":       0,
		"head_yaw_deg":   round1(r.yaw),
		"head_pitch_deg": round1(r.pitch),
		"wifi_rssi_dbm":  float64(-55 - rand.IntN(10)),
		"free_heap_kb":   float64(180 + rand.IntN(20)),
		"uptime_s":       float64(int(time.Since(r.started).Seconds())),
		"camera_mic_off": map[bool]float64{true: 1}[r.camOff],
		"brightness_pct": r.brightness,
		"volume_pct":     r.volume,
		"screensaver":    r.screensaver,
	}
}

func readLoop(ws *websocket.Conn, cmds chan<- received, log *slog.Logger, e *e2eRobot) error {
	var speakerSec float64 // speaker audio received since the last log line
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		at := time.Now()
		if kind == websocket.BinaryMessage {
			switch {
			case len(data) > 1 && data[0] == wire.BinShowJPEG:
				cfg, err := jpeg.DecodeConfig(bytes.NewReader(data[1:]))
				log.Info("picture received", "bytes", len(data)-1, "width", cfg.Width, "height", cfg.Height, "err", err)
			case len(data) > 3 && data[0] == wire.BinSpeakerPCM:
				if rate := binary.LittleEndian.Uint16(data[1:3]); rate > 0 {
					speakerSec += float64((len(data)-3)/2) / float64(rate)
					if speakerSec >= 1 {
						log.Info("speaker audio received", "seconds", round1(speakerSec), "rate", rate)
						speakerSec = 0
					}
				}
			}
			continue
		}
		frames, err := wire.Parse(data)
		if err != nil {
			log.Warn("bad frame", "err", err)
			continue
		}
		for _, f := range frames {
			switch f.Kind {
			case wire.KindAccepted:
				var b wire.AcceptedBody
				f.Decode(&b)
				log.Info("registered", "view", b.View)
			case wire.KindRejected:
				var b wire.RejectedBody
				f.Decode(&b)
				return errors.New("rejected: " + b.Reason)
			case wire.KindPairCode:
				var b wire.PairCodeBody
				f.Decode(&b)
				if e != nil {
					b.URL = e.qrURL(b.URL) // the robot adds the fragment; the server never sees it
				}
				log.Info("pair by opening this URL (the robot would show it as a QR)", "url", b.URL, "expires_in_s", b.ExpiresInS)
			case wire.KindPaired:
				var b wire.PairedBody
				f.Decode(&b)
				log.Info("browser paired", "viewers", b.Viewers)
			case wire.KindRobotCommand:
				var b wire.RobotCommandBody
				f.Decode(&b)
				cmds <- received{cmd: b, at: at}
			case wire.KindE2EEnroll, wire.KindE2EHello, wire.KindE2ECommand:
				if e != nil {
					cmds <- received{e2eKind: f.Kind, e2eBody: f.Body, at: at}
				}
			default:
				log.Debug("ignoring frame", "kind", f.Kind)
			}
		}
	}
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

func defaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "robot-token"
	}
	return filepath.Join(dir, "stackchan-server", "robot-token")
}

// cameraFrame is a 320x240 test pattern: a gradient with a moving square.
func (r *robot) cameraFrame() []byte {
	r.frame++
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / 320), uint8(y * 255 / 240), uint8(r.frame * 8), 255})
		}
	}
	sx := (r.frame * 12) % 280
	for y := 100; y < 140; y++ {
		for x := sx; x < sx+40; x++ {
			img.Set(x, y, color.White)
		}
	}
	var buf bytes.Buffer
	buf.WriteByte(wire.BinCameraJPEG)
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50})
	return buf.Bytes()
}

// micChunk is a quiet 440 Hz tone: sample rate (uint16 LE), then s16le mono PCM.
func (r *robot) micChunk(d time.Duration) []byte {
	const rate = 16000
	n := int(d.Seconds() * rate)
	out := make([]byte, 1, 3+2*n)
	out[0] = wire.BinAudioPCM
	out = binary.LittleEndian.AppendUint16(out, rate)
	for i := 0; i < n; i++ {
		s := int16(3000 * math.Sin(r.phase))
		r.phase += 2 * math.Pi * 440 / rate
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}

// standbyError ends a connection on purpose: stay offline for d, then reconnect.
type standbyError struct{ d time.Duration }

func (e standbyError) Error() string { return fmt.Sprintf("standby for %v", e.d) }

// serversEvent reports the fake server list the way the firmware does (no tokens).
func (r *robot) serversEvent(current string) wire.RobotEventBody {
	if r.servers == nil {
		r.servers = []map[string]any{{"name": "built-in", "url": current, "origin": "built-in", "token": true}}
		r.serverDefault = current
	}
	list, _ := json.Marshal(r.servers)
	return wire.RobotEventBody{Name: "servers", Data: map[string]any{"list": string(list), "current": current, "default": r.serverDefault}}
}
