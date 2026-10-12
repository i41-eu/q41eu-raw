// Package wire defines the robot <-> server WebSocket frames. It is public so other
// servers the robot can switch to (e.g. q41eu-pet) speak the same protocol.
//
// It implements the device wire protocol v1 specified in the i41eu-arch repo
// (docs/wire-protocol.md), which is the reference: change the spec there first.
// Every frame is a JSON object {"kind", "meta", "body"}. Frames stay flat and
// small so the ESP-IDF side can handle them with cJSON.
package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Connection handshake (wire protocol §1).
const (
	ConnectPath    = "/api/devices/connect"
	DeviceIDHeader = "X-Device-Id"
)

// DeviceID returns the device id a connecting device sent in DeviceIDHeader.
func DeviceID(h http.Header) string {
	return h.Get(DeviceIDHeader)
}

// Frame kinds.
const (
	// Robot -> server.
	KindRegister       = "Register"
	KindHeartbeat      = "Heartbeat"
	KindRobotTelemetry = "RobotTelemetry"
	KindRobotEvent     = "RobotEvent"
	KindRobotPong      = "RobotPong"

	// Server -> robot.
	KindAccepted     = "Accepted"
	KindRejected     = "Rejected"
	KindPairCode     = "PairCode"
	KindPaired       = "Paired"
	KindRobotCommand = "RobotCommand"

	// End-to-end encrypted (i41eu-arch docs/e2ee.md; package e2e). The server only relays
	// them: browser -> robot E2EEnroll, E2EHello, E2ECommand; robot -> browsers E2EGroupKey,
	// E2EData. Binary 0x30 (robot -> browsers) and 0x31 (browser -> robot) likewise.
	KindE2EEnroll   = "E2EEnroll"
	KindE2EHello    = "E2EHello"
	KindE2ECommand  = "E2ECommand"
	KindE2EGroupKey = "E2EGroupKey"
	KindE2EData     = "E2EData"
)

// E2EEnrollBody is a browser's request to be enrolled: its key and the MAC that proves it
// read the robot's QR code. Field names follow docs/e2ee.md.
type E2EEnrollBody struct {
	B   string `json:"b"`   // the browser's X25519 public key, base64url
	MAC string `json:"mac"` // HMAC-SHA256(P, ...), base64url
}

// E2EHelloBody: a returning browser asks for the current group key.
type E2EHelloBody struct {
	B string `json:"b"` // browser id
}

// ClassRobot is the only worker class this server accepts.
const ClassRobot = "robot"

// Frame is the envelope of every message.
type Frame struct {
	Kind string          `json:"kind"`
	Meta Meta            `json:"meta"`
	Body json.RawMessage `json:"body"`
}

// Meta holds routing and correlation fields.
type Meta struct {
	WorkerID  string `json:"worker_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	TS        string `json:"ts,omitempty"`
}

type RegisterBody struct {
	Class        string            `json:"class"`
	Labels       map[string]string `json:"labels,omitempty"`
	Capabilities RobotCapabilities `json:"capabilities"`
}

// RobotCapabilities is the "robot" class capability schema.
type RobotCapabilities struct {
	Model        string   `json:"model,omitempty"`        // e.g. "stackchan-cores3"
	Firmware     string   `json:"firmware,omitempty"`     // firmware version string
	Commands     []string `json:"commands,omitempty"`     // commands the robot accepts, e.g. ["nod"]
	Measurements []string `json:"measurements,omitempty"` // telemetry keys it sends
}

// AcceptedBody: the device is registered. View names the device's own screen the app starts
// with ("face", "readings" on a Stackchan); empty keeps the device's default.
type AcceptedBody struct {
	View string `json:"view,omitempty"`
}

type RejectedBody struct {
	Reason string `json:"reason"`
}

type RobotTelemetryBody struct {
	Measurements map[string]float64 `json:"measurements"`
}

// PairCodeBody carries a one-time pairing code. The robot shows URL as a QR code.
type PairCodeBody struct {
	Code       string `json:"code"`
	URL        string `json:"url"`
	ExpiresInS int    `json:"expires_in_s"`
}

// PairedBody tells the robot a browser just paired; Viewers is the total count.
type PairedBody struct {
	Viewers  int `json:"viewers"`  // browsers paired with the robot
	Watching int `json:"watching"` // of them, with the app open now (what the robot shows)
	// Reconnect: sent right after Accepted because browsers were paired before, not
	// because someone just scanned. A robot showing its QR on purpose may keep it.
	Reconnect bool `json:"reconnect,omitempty"`
}

// RobotCommandBody is one command. The "basic" command set a Stackchan offers:
//
//	ping       {"id": "..."}                  answered by RobotPong
//	nod, shake, home                           head gestures
//	look       {"yaw": deg, "pitch": deg}
//	emotion    {"name": "neutral|happy|angry|sad|doubt|sleepy"}
//	say        {"text": "...", "seconds": 6}   speech bubble
//	leds       {"left": "#rrggbb", "right": "#rrggbb"}  fade a side; also
//	           {"pixels": ["#rrggbb" or null, ...]}   12 single LEDs, left 0-5, right 6-11
//	           (left 0 is at the screen end, right 11 is: the right strip runs from the back)
//	           {"effect": "rainbow|breathe|chase|blink|off", "color", "speed": 0.2..5, "seconds"}
//	brightness {"value": 1..100} by hand (ends auto), or {"auto": bool} to follow the room light
//	volume     {"value": 0..100}
//	sticker    {"name": "heart|angry|sweat|shy|dizzy", "seconds": 3}  decoration over the face
//	face                                       back from a picture to the view underneath
//	view       {"name": "face|readings"}       the robot's own screen under pictures: its face, or its readings
//	image                                      (no JSON: pictures arrive as BinShowJPEG)
//	camera     {"on": bool}                    sent by the server while browsers watch
//	mic        {"on": bool}                    sent by the server while browsers listen
//	imu_stream {"on": bool}                    sent by the server while browsers want BinIMU
//	touch_stream {"on": bool}                  sent by the server while browsers want BinTouch
//	light_stream {"on": bool}                  sent while browsers (or an app) want BinLight
//	assets                                     answered by an "assets" event: {"list": "{\"files\": [{name, bytes, crc}]}", total, free}
//	asset_delete {"name": "food/cake.png"}     answered by "asset_deleted" or "asset_error"
//	asset upload                               (no JSON: files arrive as BinAssetChunk)
//	sprite     {"id", "asset", "x", "y", "scale", "angle", "opacity", "z", "hidden", "ms", "tap"}  a stored picture over the face
//	           (x, y = center; ms = glide time; tap: a button, reported in screen_tap)
//	sprite_hide {"id"} / sprite_clear          remove one / all sprites
//	picture    {"asset"}                       a stored picture instead of the face
//	play       {"asset", "volume": 0..100}     a stored WAV (16-bit PCM); "sound_done" at the end; play_stop ends it
//	servo_power {"on": bool}                   power both head servos (off: limp)
//	rotate     {"velocity": -1000..1000, "seconds": 1..30}  continuous yaw, confirmed on the robot's screen
//	snapshot                                   full-resolution still, arrives as BinSnapshot
//	camera_config {"mirror": bool, "flip": bool}
//	camera_reg {"reg": n, "value": v}          raw sensor register write (optional) and read
//	server_switch {"server": url or name}     suggest another app (the robot asks on its screen)
//	screensaver {"on": bool}                   blank the screen / wake it
//	standby    {"minutes": 1..120}             offline with the screen off
//	speaker                                    (no JSON: audio arrives as BinSpeakerPCM)
//	nfc        {"on": bool}                    NFC tag polling (on by default)
//	ir_send    {"address", "command"} (NEC) or {"raw": "9000,4500,...", "carrier_hz"}  infrared, us mark/space
//	power_led  {"mode": "on|off|blink|fast|charging"}  the red power/charge LED
//	proximity  {"on": bool}                    proximity sensor (its IR LED); on after boot
//	hold       {"seconds": 30..300}            keep the head servos powered (0 = release now)
type RobotCommandBody struct {
	Command string         `json:"command"`
	Args    map[string]any `json:"args,omitempty"`
}

// RobotEventBody reports something that happened on the robot: "shake",
// "head_press" with the zone intensities {"z0", "z1", "z2"} (0-3),
// "head_release" with {"ms"}, "head_swipe_forward", "head_swipe_backward", "screen_tap"
// (anywhere on the face, its eyes and mouth too; a double tap blanks the screen, except
// on a "tap" sprite, where it is two taps) and "screen_long_press" with data {"x", "y"}
// in screen pixels (320x240) and what the
// finger hit: "sprite" (the topmost sprite with "tap"), its "asset", "sprite_x" and
// "sprite_y" (the point in that picture's pixels), and "picture" (the full-screen
// picture shown: its file, or "sent"), "nfc_tag" with data
// {"uid", "type", "atqa", "sak", optional "text"} (the first NDEF record: URI
// or text), "nfc_removed" with {"uid"}, "proximity_near" / "proximity_far"
// with {"value"} (someone came close / left), "ir_received" with {"protocol":
// "nec" or "raw", "address", "command" (NEC), "raw": "mark,space,..." (us)},
// "power_button" with {"press": "short" or "long"}, "usb_plugged", "usb_unplugged",
// "battery_inserted", "battery_removed", "hold_on" {"seconds"} / "hold_off",
// and screensaver/standby events.
type RobotEventBody struct {
	Name string         `json:"name"`
	Data map[string]any `json:"data,omitempty"`
}

// Binary WebSocket messages: one type byte, then the payload.
const (
	BinCameraJPEG byte = 0x01 // robot -> server -> browser: camera frame (JPEG)
	BinAudioPCM   byte = 0x02 // robot -> server -> browser: microphone, uint16 LE sample rate then s16le mono PCM
	BinSpeakerPCM byte = 0x03 // browser -> server -> robot: speaker, same layout as BinAudioPCM
	// BinAudioMulti: robot -> server -> browser: microphone, all codec channels:
	// uint16 LE sample rate, uint8 channel count, then interleaved s16le PCM.
	BinAudioMulti byte = 0x04
	// BinIMU: robot -> server -> browser: raw IMU samples (100 Hz) while a browser asks
	// for them: uint16 LE count, then per sample uint32 LE time (ms) and 9 float32 LE:
	// accel x/y/z (m/s^2), gyro x/y/z (deg/s), magnetic x/y/z (uT).
	BinIMU byte = 0x05
	// BinTouch: robot -> server -> browser: raw touch frames (every 20 ms) while a browser
	// asks for them: uint16 LE frame count, then per frame uint32 LE time (ms), uint8 n and
	// n x (uint8 id, uint16 LE x, uint16 LE y).
	BinTouch byte = 0x06
	// BinLight: robot -> server -> browser: raw light and proximity samples (every 50 ms)
	// while a browser asks for them (light_stream): uint16 LE count, then per sample
	// uint32 LE time (ms), uint16 LE proximity (0..2047, 0 while proximity is off),
	// uint16 LE light CH0 (visible + IR) and CH1 (IR), raw counts.
	BinLight byte = 0x08
	// BinSnapshot: robot -> server: a full-resolution JPEG still (the "snapshot" command).
	// The server keeps the latest per robot and serves it at /api/robots/{id}/snapshot.
	BinSnapshot byte = 0x07
	BinShowJPEG byte = 0x10 // server -> robot: picture (JPEG, 320x240) shown instead of the face
	// BinAssetChunk: server -> robot: part of a file for the robot's file store: uint8 name
	// length, name (relative path, e.g. "food/cake.png"), uint32 LE total size, uint32 LE
	// offset, then the data. Chunks come in order from offset 0; the robot answers the last
	// one with an "asset_saved" {name, bytes, crc} event (CRC-32 IEEE), or "asset_error".
	BinAssetChunk byte = 0x11
)

// RobotPongBody answers the "ping" command. QueueMs is how long the ping
// waited on the robot between arriving and being handled.
type RobotPongBody struct {
	ID      string  `json:"id"`
	QueueMs float64 `json:"queue_ms"`
}

// Marshal builds one frame. A nil body is sent as {}.
func Marshal(kind string, meta Meta, body any) ([]byte, error) {
	if meta.TS == "" {
		meta.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw := json.RawMessage("{}")
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal %s body: %w", kind, err)
		}
		raw = b
	}
	return json.Marshal(Frame{Kind: kind, Meta: meta, Body: raw})
}

// Parse decodes one WebSocket message: a single frame or, as the protocol
// allows (wire protocol §2), a JSON array of frames.
func Parse(data []byte) ([]Frame, error) {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var frames []Frame
		if err := json.Unmarshal(data, &frames); err != nil {
			return nil, fmt.Errorf("parse frame batch: %w", err)
		}
		return frames, nil
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse frame: %w", err)
	}
	return []Frame{f}, nil
}

// Decode unmarshals the frame body into v. An empty body leaves v unchanged.
func (f Frame) Decode(v any) error {
	if len(f.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(f.Body, v); err != nil {
		return fmt.Errorf("decode %s body: %w", f.Kind, err)
	}
	return nil
}

// AssetChunkSize is the data per BinAssetChunk message.
const AssetChunkSize = 32 << 10

// AssetChunks splits a file into BinAssetChunk messages (type byte included):
// uint8 name length, name, uint32 LE total size, uint32 LE offset, data.
// An empty file is one message without data.
func AssetChunks(name string, data []byte) [][]byte {
	var out [][]byte
	for off := 0; off == 0 || off < len(data); off += AssetChunkSize {
		part := data[off:min(off+AssetChunkSize, len(data))]
		msg := make([]byte, 0, 10+len(name)+len(part))
		msg = append(msg, BinAssetChunk, byte(len(name)))
		msg = append(msg, name...)
		msg = binary.LittleEndian.AppendUint32(msg, uint32(len(data)))
		msg = binary.LittleEndian.AppendUint32(msg, uint32(off))
		out = append(out, append(msg, part...))
		if len(data) == 0 {
			break
		}
	}
	return out
}
