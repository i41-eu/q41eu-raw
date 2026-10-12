# q41eu-raw

<img src="docs/img/robot3d-raw.png" width="240" align="right" alt="A Stackchan robot on Raw data: its own readings on its screen, its LEDs lit">

Relay between M5Stack Stackchan robots and web browsers for **Embody Mode**. It lets you see through the robot and control it from any browser, with no app to install.

- **Robot:** opens an outbound WebSocket to the server and registers. It then shows a QR code for pairing, streams telemetry and events, and runs commands.
- **Browser:** scans the robot's QR code to pair, then gets a live dashboard.
- **Server:** one Go binary with the web page built in. State lives in memory and is saved to a JSON file, so pairings survive a restart.

The robot side is the [Embody Mode app](https://github.com/i41-eu/i41eu-embody/tree/main/firmware/main/apps/app_embody_mode) in the robot's firmware, [i41eu-embody](https://github.com/i41-eu/i41eu-embody) (M5Stack's StackChan firmware with Embody Mode). **Setting up a robot** is the Stackchan manager's job ([i41eu-manager](https://github.com/i41-eu/i41eu-manager)), and optional: one button on its page in Chrome installs the firmware and gives the robot the apps you approve, this one among them; your own firmware build: [SETUP.md](https://github.com/i41-eu/i41eu-embody/blob/main/firmware/main/apps/app_embody_mode/SETUP.md).

Part of [i41eu-arch](https://github.com/i41-eu/i41eu-arch), a local first, privacy first platform for a home: this repo holds the Go implementation of its device wire protocol (the `wire` package), which the other servers use too.

> **A proof of concept, vibe coded.** Written with AI agents and tested on real hardware at
> home, but neither the code nor its security has been reviewed by humans. Use it on your
> own network, and don't trust it with anything private yet.
>
> **Early stage: no backward compatibility.** Protocols, APIs, file formats and stored settings
> change when something better comes along, without migrations: update the robot's firmware
> and the servers together.
>
> **Want more?** Ask in the [issues](https://github.com/i41-eu/q41eu-raw/issues), and ideally [sponsor mj41](https://github.com/sponsors/mj41) on GitHub:
> mj41 codes for attention food.

## How the pieces fit

| Piece | What it is | Needed? |
|---|---|---|
| **Robot** | a Stackchan with the [Embody Mode firmware](https://github.com/i41-eu/i41eu-embody/tree/main/firmware/main/apps/app_embody_mode). On its screen the **app switcher** (the QR screen: Next, Connect) lists its apps and switches between them; its gear opens the **Manager screen** (which manager it has, turn it off or on). | yes |
| **Apps** | what the robot does, one at a time. Each app is a web service: the robot connects to it, and a phone or browser opens its web page. This repo is [Raw data](https://s.i41.eu/raw); the apps online: [s.i41.eu](https://s.i41.eu/#apps) | at least one |
| **Phone or browser** | opens an app's page and pairs with the robot by scanning its QR code; with end-to-end encryption only paired browsers can read the robot | to use an app |
| **Manager** | [i41eu-manager](https://github.com/i41-eu/i41eu-manager): the web service that sets robots up over USB, gives each robot its own token per app, and switches and changes apps from its page; on your own computer (the home manager) or online at [m.i41.eu](https://m.i41.eu), which a home manager may link to | optional: without it, apps are written over USB and switched on the robot's app switcher |

"Manager" always means this web service; on the robot there is only the app switcher and the
Manager screen that shows which manager it has.

## On the robot: its readings

Raw data's screen on the robot is the robot's own **readings** (`Accepted {"view": "readings"}`),
not a face: a row per sensor in the order of its parts (LEDs as lit, touch, accelerometer, gyro,
magnetometer, light, proximity, the head touch's three zones and the head's angles, servo
feedback, power, the processor) and the last input ("head press 3 1 0", "nfc tag 04:A2:…"),
in amber while new, with a ring where a finger touches. The robot draws it five times a second
from its own sensors, so it works end to end encrypted too. A picture or a sprite still goes
over it; "Face" and "Readings" on the dashboard switch (`view`).

## Dashboard

For each paired robot, under a line with its battery, Wi-Fi and uptime, the dashboard shows the
robot **part by part, in the order of its 3D model** on [s.i41.eu/s3d](https://s.i41.eu/s3d/) (touch
screen, display, camera, microphones, motion sensor, light and proximity, head touch, head
servos, infrared, NFC, power management, body battery monitor, clock, processor, LEDs, speaker,
servo supply, then the robot's apps, files and standby). Each part has its chip, a **3D** link
that shows where it sits on the robot, its last event, its readings and its controls. Views pick
from them (the choice is kept in this browser):

| View | What it shows |
|---|---|
| All parts | everything, Events first |
| In | what the robot senses: readings, live streams, Events |
| Out | what it does: screen, LEDs, head, speaker, infrared, power LED |
| Audio & video | camera, microphones, speaker |
| Lights | the 12 LEDs, the power LED, the backlight, the light sensor |
| Motion | head servos, servo supply, the motion sensor, the camera to watch them |
| Robot | apps, files, standby, screensaver, latency |

What the parts offer:

- **Latency:** "Ping ×10" splits the round trip into browser ↔ server, server ↔ robot, and the robot's app loop.
- **Face** (display): the readings or the face (`view`), six emotions, `say` (speech bubble).
- **Head:** nod, shake, home, and yaw/pitch sliders with ±5/±15 chips. "Hold position" (Off by default; 30 s–5 min) keeps the servos powered at the current angle, then they go slack again. A slider holds your target and shows the robot's reported angle ("now …"). Pitch is limited to 5–85° (M5Stack's safe range).
- **LEDs:** one colour for all 12 LEDs, "Random" (a random colour per LED, for a quick test) and "Off"; effects drawn by the robot (Rainbow, Breathe, Chase, Blink) in that colour at three speeds; and a picker per LED.
- **Backlight** (display) and **Volume** (speaker).
- **Screen:** stickers over the face (heart, angry, sweat, shy, dizzy), 12 emoji (smile, grin, laugh, wink, love, cool, surprised, thinking, sleepy, cry, sob, angry) sent as a full-screen picture, or a picture from the phone. The picture is scaled to 320x240 in the browser and replaces the readings or the face until "Take the picture away".
- **Live touch** (touch screen): both fingers on the screen, drawn live (50 Hz) from the raw touch controller data. Events `touch_down` / `touch_up` (with duration) come from the same data.
- **Camera extras:** "Snapshot 640×480" (shown below the video, with a download link), Mirror / Flip, and raw sensor registers (read / write, answer in Events).
- **Apps:** the robot's apps (from its manager, or written over USB), with **Switch** (the robot asks on its screen). Adding, removing and the start app are the robot's manager's job ([i41eu-manager](https://github.com/i41-eu/i41eu-manager)), never an app's.
- **Head extras:** Servo power on/off, and Rotate ⟲ / Stop / ⟳ (continuous yaw, Slow/Medium/Fast, 3/10/30 s; the robot asks on its own screen first).
- **Live IMU** (motion sensor): raw accelerometer, gyro and magnetometer at 100 Hz while the button is on (the robot streams only while someone watches), with "Download CSV" of up to 60 s.
- **Camera & mic:** live video (JPEG, about 5 fps; 320×240, or 640×480 for tiers 1–3; one size for all watchers) and the robot's microphone, played through Web Audio. The robot streams only while someone watches or listens, and shows a red LIVE badge meanwhile. Open video and audio streams per browser and robot are limited by tier (4, or 2 signed in without a higher tier, 1 signed out); live sensors (IMU, touch, light) have their own allowance of 6, so they never use up the video. A refused stream says why on the page.
- **Speaker:** "Talk" (tap to start, tap again to stop) streams your microphone to the robot, "Play sound file" plays any audio file the browser can decode, and "Beep" is a test tone. Audio is resampled to 24 kHz in the browser and the robot's mouth moves while it plays. Browsers give the microphone only to secure pages: use `http://localhost` on the server machine, or run the server with `-tls-listen :8766` and open `https://<LAN IP>:8766` on the phone (the plain page links to it; accept the self-signed certificate warning once). Files and Beep work anywhere.
- **Infrared:** point a TV remote at the robot and press a button: the last code shows up with "Send again" and "Save…" (a named button, kept in this browser). "NEC address / command" sends a code by hand (a received NEC code fills it in), and "Add to list" saves it under a name, so you can try a few commands side by side. "Tap / Hold 0.5–2 s" sets how long every send holds the button (default 0.5 s: frame plus repeat codes, like a real press). "Send 3×" sends the whole code three times, for when the robot's weak IR LED only sometimes gets through. "Test LED (3 s)" lights the IR LED long enough to see it through a phone camera; "Self-test" checks that the robot hears its own signal (carrier and timing).
- **Power LED** (power management): on, blink, fast, off, or "Charging" (the charger drives it).
- **NFC:** "Reader on" / "Reader off". While on (the default), holding a tag or card near the robot adds an `nfc tag` event with its UID and type. For NTAG stickers it also shows the first NDEF record (a URL or text). Taking it away adds `nfc removed`.
- **Adding a browser** (e.g. a laptop without a camera): on the "no robot paired" page, "Ask a paired phone" shows a short code; every paired browser gets a banner with the same code and **Allow** / **Deny**. Allow pairs the new browser with the approver's robots. Requests expire after 3 minutes and are rate-limited. The page can also pair with the 8-character code shown under the robot's QR code.
- **Readings:** each part's telemetry in a table of its own: acceleration per axis (g) and rotation rate, the magnetic field per axis (µT, plus raw counts), servo load and temperature, servo supply voltage, chip temperature, room light (lux) and proximity, and power: the body battery (voltage, current, power, raw shunt voltage from the INA226) and the CoreS3 power chip (battery, USB and system voltage, charge state and phase, chip temperature, raw status registers).
- **Auto brightness** (display, for robots with a light sensor): the robot sets its backlight from the room light. Moving the brightness slider switches it off.
- **Proximity sensor** (light and proximity): on/off. Its IR LED next to the camera blinks about 10 times a second; switch it off, e.g., while using infrared.
- **Events:** shake, head touch (press with the touched zones, release with its duration, swipe forward / back), screen taps with coordinates, NFC tags (UID, type, and the link or text stored on the tag), and screensaver on/off. The list also shows what browsers sent (→ nod, → IR NEC 0x80 / 0x04 · hold ×4, → picture…), from every browser paired with the robot; a switch picks All, Robot only or Sent only. The list comes first in All parts and In, and each part shows its own last event. The server replays each robot's last 40 events and sent commands when a browser connects, so a phone that was asleep still sees them.
- **Screensaver** (display) and **Standby** (the robot): screensaver on / off, and standby for 1–60 min.
  - **Status** shows the screensaver as `off`, `auto` or `manual`.
  - **Auto:** the robot blanks after 60 s (firmware option) without touch **and** without commands or live media, so using it remotely keeps it awake.
  - **Manual:** a **double tap** on the robot or "Screensaver on". The robot keeps it until a touch or "Screensaver off". A double tap on a button (a sprite with `tap`) is two taps instead, for games and menus.
  - **Commands wake the screen** (checkbox, on by default): the dashboard follows every command (except ping), and every picture, with `screensaver off` when the screen is blank. The robot stays explicit; the browser decides.
  - **Standby:** the robot tells the server, then goes offline with backlight, LEDs, camera and mic off. It comes back after the time or on a touch. Meanwhile the card shows "standby until HH:MM".

Controls appear only for the commands a robot lists in its capabilities.

## Run

```bash
go run ./cmd/q41eu-raw              # listens on :8765
go run ./cmd/fake-robot                    # simulated robot, in a second terminal
```

- The first run generates the robot token at `~/.config/stackchan-server/robot-token`.
- `fake-robot` implements the whole command set: a test-pattern camera, a 440 Hz "microphone", picture checks, and random events. It prints a pairing URL; open it in a browser or phone on the same network.
- `-public-url` sets the base URL put into QR codes. The default is `http://<LAN IP>:<port>`.
- `-state-file` (default `~/.local/state/stackchan-server/state.json`, `""` disables) keeps pairings and known robots across restarts: browsers stay paired, and robots show up offline with their last telemetry and events until they reconnect. The whole file is rewritten every 5 s when something changed, right after a pairing, and on shutdown. It holds session IDs, so it is mode 0600.
- `-state-database postgres://user@host/db` (or `STATE_DATABASE_URL`; the password from `PGPASSWORD`) keeps the same document in Postgres instead, as a row of the `state` table with the last 50 saves in `state_history` (package `statestore`, shared with the manager and the pet). On its first start against an empty database it imports the `-state-file`, if there is one. It holds a lock while it runs: a second copy waits for the first to stop, and if the database connection is lost it exits rather than risk two writers.
- `-tls-listen :8766` also serves the dashboard over HTTPS, for the phone's microphone ("Talk"). Without `-tls-cert`/`-tls-key` it creates a self-signed certificate for localhost, this host name and every local IP, and keeps it in `~/.config/stackchan-server/` so a phone accepts it only once. Robots stay on `-listen`. The pairing cookie is shared by both ports (same host).
- `-robot-tokens-file <file>`: other people's robots, each with its own invite token (see "Other people's robots" below). Off by default.
- `-trusted-proxies 1` behind one reverse proxy that appends the client address to `X-Forwarded-For` (Envoy, nginx with `$proxy_add_x_forwarded_for`). The default 0 ignores that header, because clients can forge it. Limits and logs use the address.
- **Limits:** 20 failed logins per address and robot id in 10 minutes, then HTTP 429 for that robot from that address, right token included (other robots behind the same address keep working); 20 wrong pairing codes per address in 10 minutes, then 429. Invited robots may send 300 messages/s and 512 KB/s on average (bursts of 1000 messages and 4 MB), and are disconnected above that; the owner's robots are not limited. Browsers: at most 8 open event streams per session and 30 per address, and 20 media sockets per address; media sockets per session and robot, and commands, pictures or uploads per second per session, depend on the tier (**Tiers** in [Robots set up by a manager](#robots-set-up-by-a-manager-and-sign-in); without sign-in everybody is tier 1: 4 sockets, 20 per second, bursts of 60); above that HTTP 429.
- `-no-address-limits` turns the per-address limits off, for a server that cannot see client addresses (behind a TCP load balancer without the PROXY protocol, every client has the balancer's address, and one stranger's failures would lock everybody out). The send budgets for invited robots stay.
- `-manager-url https://manager.example -manager-secret-file <file>`: robots set up by a Stackchan manager connect with tokens it gave them for this app; this server asks the manager about them ([below](#robots-set-up-by-a-manager-and-sign-in)).
- `-ui-dir internal/server/ui` serves the dashboard from disk on every request (development). UI edits then need only a page reload.
- Open the dashboard with the same host as the QR code (e.g. `http://192.168.1.10:8765/`, not `localhost`). The pairing cookie belongs to that host.
- Run `go test -race ./...` for the tests.

### Robots set up by a manager, and sign-in

A robot set up by a [Stackchan manager](https://github.com/i41-eu/i41eu-manager) has a token
of its own for this app. This server asks the manager about it when the robot connects
(`POST <manager>/api/robot-auth`, with this app's secret) and learns whose robot it is and
whether it is public; it keeps no robot tokens itself. Every 15 s while the robot stays, it
tells the manager the browsers paired with it (device, since, last seen, watching now,
end-to-end id; not their sessions), and drops the pairings the owner removed there. With sign-in through the manager
(`-manager-sign-in`: one sign-in for all its apps) owners use their robots here:

```bash
q41eu-raw -public-url https://raw.example \
  -manager-url https://manager.example -manager-secret-file /secrets/manager \
  -manager-sign-in -admin-emails you@example.com -state-file /state/state.json \
  -tiers-file /config/tiers -sponsor-url https://github.com/sponsors/you
```

- **Private or public.** With sign-in configured, every robot is private by default: only its
  owner, signed in, can pair with it and use it (the account the manager names; for this
  server's own robots, the admins), and the owner's signed-in browsers get their robots
  without a code. Anyone else, even with the code from its screen, gets "this robot is
  private". The owner makes a robot **public** on the manager: then the code pairs anyone, as
  on a server without sign-in, from the robot's next connection. Making it private again
  unpairs everybody else. Anonymous browsers cannot send "Ask a paired phone" requests.
- The manager's answers are cached for a minute; when the manager cannot be reached, a robot
  it confirmed within the last hour may still connect. A refusal is never cached.
- **The robot's apps are its manager's business.** The robot keeps its own connection to its
  manager (i41eu-arch docs/manager-channel.md): app lists, switches and the rest go there, signed;
  this server only carries the robot's traffic for this app.
- Other apps check robot tokens the same way with the package
  [`robotauth`](robotauth/robotauth.go) (`github.com/i41-eu/q41eu-raw/robotauth`), e.g. the pet.
- Without sign-in configured (a LAN server), every robot is public.
- **Tiers** set the limits (not access: pairing still needs the robot's code, or a public
  robot):

  | Tier | Who | Commands/s (burst) | Media sockets per robot | 640×480 video |
  |---|---|---|---|---|
  | 1 | `-admin-emails`, tier 1 in `-tiers-file` | 20 (60) | 4 | yes |
  | 2 | sponsors (tier 2 in the file) | 20 (60) | 4 | yes |
  | 3 | others the owner approved (tier 3) | 20 (60) | 4 | yes |
  | 4 | signed in | 10 (30) | 2 | no (320×240) |
  | 5 | anonymous | 3 (15) | 1 | no |

  Over a limit, the answer (429) says how to get more: tier 5 "Sign in for higher limits.",
  tier 4 "Sponsors get higher limits: `-sponsor-url`". The dashboard shows the tier next to
  the account. `-tiers-file` is plain text for a config repository, re-read when it changes
  (a broken file keeps the rules from before):

  ```
  # tier  who                      (anything after it is a note)
  1       github:mj41
  2       github:octocat           sponsor since 2026-10
  3       email:friend@example.com
  ```

  `email:` (and `-admin-emails`) match only e-mails verified by GitHub or Google (not Microsoft: a tenant admin can set any e-mail there); `github:` (login) and `github-id:` (user id) match
  only through Dex's GitHub connector (scope `federated:id`). Without sign-in (a LAN server)
  everybody is tier 1.
- Robots that connect with the shared token (`-token-file`) are this server's own: they belong
  to the admins.
- **One sign-in for all apps.** "Sign in" goes to the manager (`GET <manager>/sso`), which
  signs the person in once (GitHub or Google through Dex) and sends them back with a one-time
  code; this server trades it for the account (`POST <manager>/api/sso/token`, with this app's
  secret). After someone signs in on the manager, it sets a hint cookie for its whole domain
  (`i41_signed_in`, a random value, no credential); a page load here that sees a new hint tries
  the sign-in silently once, so they are signed in here without a click. Without the hint
  nobody is sent anywhere: anonymous visitors just use the page. Every minute this server asks the manager whether each sign-in is still on: signing out
  on the manager or in any app signs out here too, and signing out here signs out everywhere.
  The browser's cookie for the manager never comes here; this server keeps a session of its
  own. The package [`sso`](sso/sso.go) does it for any app (the pet uses it too).
- Changing requests must come from this server's origin.
- Sign-ins live in the state file (`-state-file`), so it must be on persistent storage.

### Other people's robots: invite tokens (without sign-in)

The shared robot token is for your own robots: anyone who has it can connect a robot under
any id. It is compiled into your own firmware build or written over USB; the release firmware
has no token. For other people's robots, without sign-in, give each robot its own **invite
token**:

```bash
q41eu-raw invite stackchan-0a1b2c3d4e50   # the robot id its screen shows (stackchan-<MAC>)
```

It prints a new token and a line. **The token** goes to the robot's owner, for their
`sdkconfig` (`CONFIG_STACKCHAN_EMBODY_TOKEN`). **The line** (`<robot id> <SHA-256 of the
token>`) goes into the file you pass as `-robot-tokens-file`; the file holds no tokens.

- A token works only for its own robot id, so an invited robot cannot pose as another one.
- The server reads the file again when it changes: adding a robot (a new line) or revoking
  one (deleting its line) needs no restart. A broken file lets no invited robot in, and is
  logged; robots with the shared token keep working.
- Browsers still pair only by the code on the robot's own screen, so an invite gives a robot
  a place on the server, not anyone access to it.
- Invited robots have send limits (above, "Limits").

### End-to-end encryption

The server relays the sealed `E2E*` frames and the binary `0x30`/`0x31` messages between a
robot and its paired browsers without reading them
([e2ee.md](https://github.com/i41-eu/i41eu-arch/blob/main/docs/e2ee.md)). The browser side is
the dashboard's `e2e.js`; `go run ./cmd/fake-robot -e2e` tests it without hardware.

## Set a robot up over USB

Moved to the Stackchan manager, [i41eu-manager](https://github.com/i41-eu/i41eu-manager):
its `/setup` page (Chrome, Web Serial) installs the official firmware and writes the approved
apps with their tokens into the robot, and its `i41eu-usb` does the same from a terminal.
For a robot of this server alone, a home manager's catalog lists this server with its shared
token (`token_file`).

## Running in a container

A public instance runs at **https://raw.q41.eu** (robots set up on [m.i41.eu](https://m.i41.eu)). To run your own:

1. **Image:** pushing a `v*` tag builds `ghcr.io/i41-eu/q41eu-raw:<tag>`
   (`.github/workflows/release.yml`). Pin it by digest where you deploy it.
2. **Robot token:** mount the robot token file at `/secrets/robot-token` (keep it out of
   git), and pass `-public-url https://<your host>`.

What any host needs:

- **One instance:** state is in memory, so run exactly one, and stop the old one before
  the new one starts. The image disables the state file (`-state-file ""`), so a restart
  forgets pairings unless you mount a volume and pass `-state-file /state/state.json`.
- **No request timeout** on the proxy in front of it: robot WebSockets, the browser's
  event stream and the media sockets stay open for hours.
- **TLS** may end at the proxy: `-public-url https://…` also makes the session cookie
  `Secure`.
- **Audio bandwidth:** microphone audio is raw PCM (about 48 KB/s). Compress it before
  offering audio over the internet.

## Protocol

This is the device wire protocol v1. Its reference specification is [wire-protocol.md](https://github.com/i41-eu/i41eu-arch/blob/main/docs/wire-protocol.md) in i41eu-arch; the sections below are this server's view of it.

**Connect:** `ws://<server>/api/devices/connect` with these headers:

- `Authorization: Bearer <robot token>`
- `X-Device-Id: <robot id>`: 1–64 of `[A-Za-z0-9._-]`.

### Frames

Every WebSocket text message is one JSON frame, `{"kind", "meta", "body"}`; the kinds and
their bodies are in [wire-protocol.md §4](https://github.com/i41-eu/i41eu-arch/blob/main/docs/wire-protocol.md#4-frame-kinds).
What this server adds:

- `Register` must be the first frame.
- `Accepted` names the robot's readings screen as this app's view (`{"view": "readings"}`):
  Raw data shows the robot's own readings, not a face.
- `Paired` comes after a pairing, and right after `Accepted` when browsers are already paired
  (pairings survive restarts), so the robot starts with its face instead of the QR screen.
- `E2E*` frames are relayed unread ([End-to-end encryption](#end-to-end-encryption)).
- Browsers get each `RobotEvent` with a per-robot `seq`.

### Events

`RobotEvent` names a Stackchan sends, with `data` (values are numbers or strings): `shake`,
`head_press` with zone intensities `{"z0", "z1", "z2"}` (0–3), `head_release` with `{"ms"}`,
`head_swipe_forward`, `head_swipe_backward`, `screen_tap` with `{"x", "y"}` (anywhere on the
face, its eyes and mouth included), `screensaver_on` with `{"manual": 0 or 1}`,
`screensaver_off`, `standby` with `{"minutes"}`, `standby_end` with `{"touched"}`, `nfc_tag`
with `{"uid": "04:A2:…", "type", "atqa", "sak"}` plus `"text"` when the tag holds an NDEF URI
or text record, `nfc_removed` with `{"uid"}`, `proximity_near` / `proximity_far` with
`{"value"}`, `ir_received` with `{"protocol": "nec" or "raw", "address", "command", "raw"}`
(raw marks/spaces in µs, always present), `screen_long_press` with `{"x", "y"}` (free for
apps), `servers` with `{"list": JSON array of {name, url, origin, token: bool}, "current",
"default"}`, `power_button` with `{"press": "short" or "long"}`, `usb_plugged` /
`usb_unplugged`, `battery_inserted` / `battery_removed`, `privacy` with `{"camera_mic": "on" or
"off", "reason", "mode", "night"}` (the camera and microphone as set on the robot itself: its
Manager screen or USB; telemetry `camera_mic_off` 0/1), `privacy_refused` with `{"command",
"reason"}` (a `camera`, `mic` or `snapshot` while they are off there). The dashboard shows it
at the top of the robot ("📷 Camera and microphone are off on the robot"), greys out Watch,
Listen and Snapshot, and says why in Camera & mic. `fake-robot -camera-off` acts so. Answers to commands are in the command table below.

### Commands

`RobotCommand.command`, with `args`:

| Command | Args |
|---|---|
| `ping` | `{"id"}`: the robot answers with `RobotPong`. The server times its leg and sends a `pong` SSE event only to the browser that pinged. |
| `nod`, `shake`, `home` | none |
| `look` | `{"yaw": -128..128, "pitch": 5..85}` in degrees |
| `screensaver` | `{"on": bool}`: blank the screen (manual) or wake it |
| `standby` | `{"minutes": 1..120}`: the robot sends a `standby` event, goes offline, and reconnects after the time or on a touch (then sends `standby_end`). `/api/robots` shows `standby_until` while it is away |
| `emotion` | `{"name": "neutral\|happy\|angry\|sad\|doubt\|sleepy"}` |
| `say` | `{"text", "seconds"}`: speech bubble |
| `sticker` | `{"name": "heart\|angry\|sweat\|shy\|dizzy", "seconds"}`: decoration over the face |
| `face` | none: back from a picture to the view underneath (the face or the readings) |
| `view` | `{"name": "face" \| "readings"}`: what the robot shows under pictures and sprites. `readings` is the robot's own readings screen (its sensors in the order of its parts, the LEDs as lit, the last input, a ring where a finger touches), drawn by the robot five times a second, so it works end to end encrypted too; this server starts robots with it (`Accepted {"view": "readings"}`). Every app starts on the face unless it names a view |
| `image` | capability only: pictures arrive as binary `0x10` |
| `leds` | `{"left": "#rrggbb", "right": "#rrggbb"}` fades a whole side. `{"pixels": [...]}` sets up to 12 single LEDs (left 0–5, right 6–11; `null` skips one). Left 0 sits at the screen end of its strip, and so does right 11 (the right strip runs from the back). `{"effect": "rainbow\|breathe\|chase\|blink\|off", "color", "speed": 0.2..5, "seconds"}` runs an animation on the robot (`seconds` 0 = until the next `leds`) |
| `brightness` | `{"value": 1..100}` sets it by hand and ends auto-brightness; `{"auto": bool}` switches brightness that follows the room light (robots with a light sensor report `auto_brightness`) |
| `volume` | `{"value": 0..100}` |
| `camera`, `mic` | `{"on": bool}`, the camera also `"size": "320x240" \| "640x480"`: sent by the server, not by browsers, while someone watches or listens. A watcher asks for the size with `/media?video=1&size=640x480`; the robot streams one size for all, 640x480 while anyone asks for it |
| `ir_send` | `{"address", "command"}` sends an NEC code; `{"raw": "9000,4500,562,…", "carrier_hz": 38000}` sends marks and spaces in µs (e.g. what `ir_received` reported). `"frames": 1..5` sends the whole frame that many times, 108 ms apart (for weak links; a toggle button may toggle twice). `"repeat": 0..20` is like holding the button: NEC repeat codes every 108 ms, or the raw frame again after 40 ms. `"loopback": true` lets the robot hear its own signal (self-test) |
| `hold` | `{"seconds": 30..300}`: keep the head servos powered at the current angle (they go slack at rest otherwise); `0` releases now. Events `hold_on` `{"seconds"}` / `hold_off`, telemetry `hold_s` (seconds left) |
| `snapshot` | none: a 640x480 still (the robot switches the sensor for one frame), arriving as binary `0x07` |
| `camera_config` | `{"mirror": bool, "flip": bool}` |
| `camera_reg` | `{"reg": n}` reads, `{"reg": n, "value": v}` writes and reads back a raw GC0308 register; answer: event `camera_reg` `{"reg", "value"}` (−1 on failure) |
| `servo_power` | `{"on": bool}`: both head servos powered or limp. Events `servo_power_on` / `servo_power_off`, telemetry `servo_power` |
| `rotate` | `{"velocity": -1000..1000, "seconds": 1..30}`: continuous yaw rotation. The first one asks on the robot's screen (`rotate_asking`); Yes starts it (`rotate_confirmed`), No or 30 s without an answer refuse it (`rotate_refused` `{"reason"}`); the Yes holds until a USB cable is plugged in or out or the app closes. Stopped by `velocity: 0`, any other head command, standby, the time limit, high servo load or a cable plugged in (`rotate_stopped` `{"reason"}`). Events `rotate_on` / `rotate_off`, telemetry `rotate_s` |
| `assets` | none: the robot answers with an `assets` event, `list` = `{"files": [{"name", "bytes", "crc"}]}` (CRC-32 IEEE) plus `total` and `free` bytes |
| `asset_delete` | `{"name": "food/cake.png"}`: answered by `asset_deleted` or `asset_error` |
| `sprite` | `{"id", "asset", "x", "y", "scale", "angle", "opacity", "z", "hidden", "ms", "tap"}`: a stored picture (PNG with transparency, or JPEG) over the face; `x`, `y` is its center in screen pixels, `ms` glides it there; `tap: true` makes it a button: taps on it (not on its transparent parts) report `sprite`, `asset`, `sprite_x`, `sprite_y` in `screen_tap` / `screen_long_press` (menus); a new id needs `asset`; up to 24; `sprite_error` {id, reason} if it cannot be shown |
| `sprite_hide`, `sprite_clear` | `{"id"}` / none: remove one sprite / all |
| `picture` | `{"asset": "pet/dream.jpg"}`: a stored picture instead of the face (`face` ends it) |
| `play` | `{"asset": "snd/hello.wav", "volume": 0..100}`: a stored WAV (16-bit PCM, mono or stereo, 4-48 kHz), streamed from the file so any length works; `sound_done` {asset} at the end, `sound_error` {asset, reason}; `play_stop` ends it |
| `touch_stream`, `imu_stream`, `light_stream` | `{"on": bool}`: sent by the server while a browser has `?touch=1` / `?imu=1` / `?light=1` open (an app server such as [q41eu-pet](https://github.com/i41-eu/q41eu-pet) may send `light_stream` itself) |
| `server_switch` | `{"server": url or name}`: suggest another of the robot's apps; the robot asks on its screen first |
| `proximity` | `{"on": bool}`: the proximity sensor, whose IR LED next to the camera pulses ~10×/s. Off stops the LED and the approach events; light and auto-brightness keep working. Telemetry `proximity_on` |
| `power_led` | `{"mode": "on\|off\|blink\|fast\|charging"}`: the red power LED; `charging` hands it back to the charger |
| `nfc` | `{"on": bool}`: NFC tag polling, on by default. Listed only when the robot found its reader |
| `automation` | `{"autostart": bool}`: open Embody Mode after every power-on or restart (stored on the robot, off until set); the robot answers with an `automation {autostart}` event, also sent after it registers. Only with firmware built with `CONFIG_STACKCHAN_EMBODY_AUTOMATION` (off by default), like the next two |
| `restart` | none: restart the robot, back into Embody Mode |
| `launch` | `{"app": "AVATAR\|AI.AGENT\|DANCE\|SETUP\|…"}`: restart into another launcher app once (`""` = the launcher); `launch_unknown {app}` event for a name it does not have |
| `car_enable` | `{"on": bool, "board"?: "v1\|v2\|both"}`: the optional TPBot car (micro:bit with [tpbot-ble](https://github.com/i41-eu/i41eu-tpbot-ble)) over BLE (firmware `CONFIG_STACKCHAN_EMBODY_CAR`). Off by default and kept on the robot. While on, the robot registers again with the `car_*` commands and telemetry listed in sbot's readme, [Car capability](https://github.com/i41-eu/q41eu-sbot#car-capability), plus `car_connected`, and sends `car_connected` / `car_disconnected` events. This dashboard has no car controls; sbot has |

### Binary messages

The first byte is the type; the types and their payloads are in
[wire-protocol.md §6](https://github.com/i41-eu/i41eu-arch/blob/main/docs/wire-protocol.md#6-binary-message-types).
How this server uses them:

- **From the robot to browsers** on the media socket (`/api/robots/{id}/media`): camera
  `0x01`, microphone `0x02` / `0x04`, and `0x05` (IMU), `0x06` (touch), `0x08` (light and
  proximity, every 50 ms) only while a browser has `?imu=1` / `?touch=1` / `?light=1` open.
  On StackChan, `0x04` channel 1 is the microphone and channel 0 the speaker reference (what
  the robot plays, looped back for echo cancellation); the dashboard plays either one.
- **`0x07`** (the `snapshot` command's still): the server keeps the latest in memory, serves
  it at `GET /api/robots/{id}/snapshot` and tells browsers with the SSE event `snapshot`
  `{"robot", "bytes", "ts"}`.
- **`0x03`** (speaker) from a browser's media socket goes only to robots that list `speaker`.
- **`0x10`** (picture): a JPEG, 320×240, from `POST /api/robots/{id}/picture` (up to 192 KB).
- **`0x11`** (file chunks): from `POST /api/robots/{id}/assets?name=…` (header
  `X-Stackchan-Upload: 1`, the file as the body, up to 2 MB), sent in order in 32 KB chunks.
- **`0x30` / `0x31`** (end to end encrypted, robot → browsers / browser → robot): relayed
  unread.

### Browser API

Every browser gets a session cookie. Endpoints under `/api/robots/{id}` need a session paired
with that robot; requests that change something must come from
this server's origin.

| Endpoint | Purpose |
|---|---|
| `GET /` | the dashboard |
| `GET /pair?code=…` | QR target; adds the robot to this browser's session |
| `GET /api/info` | about this server: `https_url` when it has an HTTPS listener (`-tls-listen`) |
| `GET /api/me` | sign-in state, tier and its hint, whether 640×480 video is allowed, the manager's URL; signed in also the account |
| `GET /auth/login`, `GET /auth/sso`, `POST /auth/logout` | sign-in through the manager (only with `-manager-sign-in`); logout signs out of every app |
| `POST /api/join` | an unpaired browser asks paired browsers for access; returns `{"id", "code", "from", "agent", "expires"}`. Paired browsers get a `join_request` SSE event (also replayed when they connect) |
| `POST /api/join/{id}/approve`, `…/deny` | a paired browser answers; approve pairs the requester with the approver's robots. The requester gets `join_result` `{"status"}`, the other paired browsers `join_closed` |
| `GET /api/robots` | paired robots (JSON) |
| `GET /api/events` | SSE: `robot` (full state; telemetry `screensaver` 0 off / 1 auto / 2 manual), `robot_event` and `command_sent` (`{"robot", "seq", "command", "args", "ts"}`: what a browser sent; both go to every paired browser, and the last 40 are replayed on connect), `pong` (only to the browser that pinged), `snapshot`, `e2e` (a robot's sealed frame), `join_request` / `join_closed` / `join_result` (see `/api/join`) |
| `POST /api/robots/{id}/command` | `{"command", "args"}` as JSON |
| `POST /api/robots/{id}/picture` | `image/jpeg` body, up to 192 KB |
| `POST /api/robots/{id}/e2e` | `{"kind": "E2EEnroll\|E2EHello\|E2ECommand", "body"}`: a sealed frame for the robot, relayed unread |
| `GET /api/robots/{id}/media?video=1&audio=1` | WebSocket, same origin only, carrying the binary messages that were asked for: `video=1` (`size=640x480` for the larger stream, tiers 1–3), `audio=1`, `imu=1`, `touch=1`, `light=1`; the server turns the robot's streams on while at least one browser wants them. Browsers send speaker audio (`0x03`) on it |
| `GET /api/robots/{id}/snapshot` | the latest full-resolution still (JPEG) |
| `POST /api/robots/{id}/assets?name=…` | a file for the robot's file store (above, `0x11`) |
| `GET /e2e.js`, `GET /vendor/{file}`, `GET /emoji/…` | the dashboard's scripts and emoji |
| `GET /healthz` | `ok`, for health checks |

**Liveness:** the server sends a WebSocket ping every 5 s and drops the robot after 60 s without traffic. Reconnect with backoff from 1 s up to 30 s.

**Pairing:** codes are one-time and valid for `-pair-ttl` (default 5 min). A used or expiring code is replaced by a fresh `PairCode`, so the QR on the robot's screen always works.

## Status

A prototype. Sign-in, per-robot tokens, private robots, tiers and an end-to-end relay are
in; still open: the shared owner token, permissions per capability, an external review. See
the [roadmap](docs/roadmap.md).

## Related projects

- [i41eu-embody](https://github.com/i41-eu/i41eu-embody): the robot's firmware (M5Stack's StackChan firmware with Embody Mode).
- Other app servers the robot can switch to, on the same `wire` package: [q41eu-pet](https://github.com/i41-eu/q41eu-pet) (a Tamagotchi for kids) and [q41eu-sbot](https://github.com/i41-eu/q41eu-sbot) (a cockpit for the robot and a TPBot car, growing into the home node).
- [i41eu-arch](https://github.com/i41-eu/i41eu-arch): the platform, its use cases, architecture and the wire protocol's reference. All the repos: [The repos today](https://github.com/i41-eu/i41eu-arch#the-repos-today).
- The robot's [hardware coverage](https://github.com/i41-eu/i41eu-embody/blob/main/firmware/main/apps/app_embody_mode/HARDWARE.md) and the [trust design](https://github.com/i41-eu/i41eu-arch/blob/main/docs/implementations/stackchan-trust.md) behind the [roadmap](docs/roadmap.md).

## Credits

The dashboard's emoji are [Fluent Emoji](https://github.com/microsoft/fluentui-emoji) (Flat style) by Microsoft, MIT license; see [internal/server/ui/emoji/LICENSE](internal/server/ui/emoji/LICENSE).

Stack-chan (スタックチャン) is a registered trademark of Shinya Ishikawa; this project is independent and only made to work with [Stack-chan](https://github.com/stack-chan/stack-chan) robots.

## License

MIT, see [LICENSE](LICENSE). The bundled emoji are Microsoft's Fluent Emoji (MIT), see [internal/server/ui/emoji/LICENSE](internal/server/ui/emoji/LICENSE).
