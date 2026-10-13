package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/wire"
)

// The page in a real browser: headless Chrome against a test server and a fake robot. Skipped
// without Chrome (CHROME=… names it).

// TestHeadTurnsTheWayTheControlsShow pins the direction of the head controls. A positive yaw
// turns the head to the robot's own left (counter-clockwise seen from above), a negative one to
// its right. On the page right means the robot's own right, as its camera sees it: everything on
// the right of the yaw slider, the slider's right end and the rotate button on the right send a
// negative yaw or velocity, and the readout says which way the head points.
func TestHeadTurnsTheWayTheControlsShow(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobotWith(t, ts, "chan-1", wire.ClassRobot, []string{"nod", "shake", "home", "look", "rotate"})
	var accepted wire.AcceptedBody
	robot.expect(wire.KindAccepted, &accepted)
	var pc wire.PairCodeBody
	robot.expect(wire.KindPairCode, &pc)
	robot.send(wire.KindRobotTelemetry, wire.RobotTelemetryBody{Measurements: map[string]float64{"head_yaw_deg": 0, "head_pitch_deg": 45}})
	commands := robot.commands()

	b := startChrome(t)
	b.open(pc.URL) // scanning the robot's QR code pairs this browser
	b.waitFor(`document.querySelector('input[type=range][aria-label="Yaw"]')`)
	b.eval(`document.querySelector('button[data-view="motion"]').click()`) // the head controls without the growing Events list
	time.Sleep(300 * time.Millisecond)

	look := func(what string) float64 {
		t.Helper()
		cmd := commands.next(t, "look", what)
		yaw, ok := cmd.Args["yaw"].(float64)
		if !ok {
			t.Fatalf("%s: look without a yaw: %v", what, cmd.Args)
		}
		return yaw
	}

	// The chips beside the yaw slider: on its right they turn the head to the robot's right
	// (yaw down), on its left to the robot's left (yaw up).
	yaw := 0.0
	for _, side := range []string{"right", "right", "left", "left", "left"} {
		commands.drain()
		text := b.str(fmt.Sprintf(`(() => {
			const yaw = document.querySelector('input[type=range][aria-label="Yaw"]'), r = yaw.getBoundingClientRect();
			const chips = [...yaw.closest(".axis-row").querySelectorAll("button")]
				.filter((b) => %q === "right" ? b.getBoundingClientRect().left >= r.right : b.getBoundingClientRect().right <= r.left)
				.sort((a, b) => Math.abs(a.getBoundingClientRect().x - r.x) - Math.abs(b.getBoundingClientRect().x - r.x));
			chips[0].click(); // the one next to the slider
			return chips[0].textContent + " · " + chips[0].title;
		})()`, side))
		got := look("the yaw chip " + text)
		if side == "right" && got >= yaw || side == "left" && got <= yaw {
			t.Fatalf("the chip on the %s of the yaw slider (%s) sent yaw %v after %v: it turns the robot to its %s",
				side, text, got, yaw, map[bool]string{true: "left", false: "right"}[got > yaw])
		}
		yaw = got
	}

	// The slider's ends, clicked: the right end is the robot's right (yaw -128), the left end
	// its left (+128).
	for _, end := range []struct {
		side string
		want func(float64) bool
	}{{"right", func(y float64) bool { return y <= -120 }}, {"left", func(y float64) bool { return y >= 120 }}} {
		commands.drain()
		var r struct{ Left, Right, Y float64 }
		json.Unmarshal(b.eval(`(() => {
			const yaw = document.querySelector('input[type=range][aria-label="Yaw"]');
			yaw.scrollIntoView({ block: "center" });
			const r = yaw.getBoundingClientRect();
			return { left: r.left, right: r.right, y: r.top + r.height / 2 };
		})()`), &r)
		x := r.Left + 3
		if end.side == "right" {
			x = r.Right - 3
		}
		b.mouseClick(x, r.Y)
		if got := look("the yaw slider's " + end.side + " end"); !end.want(got) {
			t.Fatalf("clicking the yaw slider's %s end sent yaw %v", end.side, got)
		}
	}

	// Rotate: the button right of Stop turns clockwise seen from above (to the robot's right,
	// velocity down), the one on its left counter-clockwise.
	for _, side := range []string{"right", "left"} {
		commands.drain()
		text := b.str(fmt.Sprintf(`(() => {
			const stop = [...document.querySelectorAll("button")].find((b) => b.textContent === "Stop" && b.parentElement.textContent.includes("Rotate"));
			const s = stop.getBoundingClientRect();
			const btn = [...stop.parentElement.querySelectorAll("button")]
				.find((b) => %q === "right" ? b.getBoundingClientRect().left >= s.right : b.getBoundingClientRect().right <= s.left);
			btn.click();
			return btn.textContent + " · " + btn.title;
		})()`, side))
		cmd := commands.next(t, "rotate", "the rotate button "+text)
		v, _ := cmd.Args["velocity"].(float64)
		if side == "right" && v >= 0 || side == "left" && v <= 0 {
			t.Fatalf("the rotate button on the %s (%s) sent velocity %v", side, text, v)
		}
	}

	// The readout: the robot's reported yaw with the side it points to.
	for _, c := range []struct {
		yaw  float64
		want string
	}{{-20, "20° right"}, {30, "30° left"}} {
		robot.send(wire.KindRobotTelemetry, wire.RobotTelemetryBody{Measurements: map[string]float64{"head_yaw_deg": c.yaw, "head_pitch_deg": 45}})
		b.waitFor(fmt.Sprintf(`document.querySelector('input[type=range][aria-label="Yaw"]').closest(".axis").querySelector(".axis-head").textContent.includes("now %s")`, c.want))
	}
}

// --- the robot's side: the commands it receives -----------------------------------------------

type commandFeed struct {
	ch chan wire.RobotCommandBody
}

// commands reads the robot's frames from now on and passes on the commands.
func (r *testRobot) commands() *commandFeed {
	r.ws.SetReadDeadline(time.Time{}) // expect left one
	f := &commandFeed{ch: make(chan wire.RobotCommandBody, 64)}
	go func() {
		for {
			mt, data, err := r.ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			frames, err := wire.Parse(data)
			if err != nil {
				continue
			}
			for _, fr := range frames {
				var c wire.RobotCommandBody
				if fr.Kind == wire.KindRobotCommand && fr.Decode(&c) == nil {
					f.ch <- c
				}
			}
		}
	}()
	return f
}

// drain drops the commands received so far (a slider can send one on input and one on change).
func (f *commandFeed) drain() {
	time.Sleep(300 * time.Millisecond)
	for {
		select {
		case <-f.ch:
		default:
			return
		}
	}
}

func (f *commandFeed) next(t *testing.T, name, what string) wire.RobotCommandBody {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-f.ch:
			if c.Command == name {
				return c
			}
		case <-deadline:
			t.Fatalf("%s: the robot got no %s command", what, name)
		}
	}
}

// --- the browser --------------------------------------------------------------------------------

type chrome struct {
	t   *testing.T
	ws  *websocket.Conn
	mu  sync.Mutex
	id  int
	res map[int]chan json.RawMessage
}

func chromePath() string {
	if p := os.Getenv("CHROME"); p != "" {
		return p
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// startChrome runs a headless Chrome with one tab, controlled over the DevTools protocol.
func startChrome(t *testing.T) *chrome {
	t.Helper()
	bin := chromePath()
	if bin == "" {
		t.Skip("no Chrome (set CHROME)")
	}
	profile, err := os.MkdirTemp("", "raw-page-chrome-") // not t.TempDir: Chrome's helpers may still write there at the end
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--remote-debugging-port=0", "--user-data-dir="+profile, "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM) // Chrome closes its helper processes too
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		os.RemoveAll(profile)
	})

	// Chrome writes the port it listens on to DevToolsActivePort.
	var port string
	for i := 0; port == ""; i++ {
		if f, err := os.Open(filepath.Join(profile, "DevToolsActivePort")); err == nil {
			s := bufio.NewScanner(f)
			if s.Scan() {
				port = strings.TrimSpace(s.Text())
			}
			f.Close()
		}
		if i > 150 {
			t.Fatal("Chrome did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var tab struct{ WebSocketDebuggerURL string }
	req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:"+port+"/json/new?about:blank", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&tab)
	resp.Body.Close()
	ws, _, err := websocket.DefaultDialer.Dial(tab.WebSocketDebuggerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	c := &chrome{t: t, ws: ws, res: map[int]chan json.RawMessage{}}
	go func() {
		for {
			var m struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if ws.ReadJSON(&m) != nil {
				return
			}
			c.mu.Lock()
			ch := c.res[m.ID]
			delete(c.res, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
		}
	}()
	c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 800, "height": 1400, "deviceScaleFactor": 1, "mobile": false})
	return c
}

func (c *chrome) call(method string, params any) json.RawMessage {
	c.t.Helper()
	c.mu.Lock()
	c.id++
	id := c.id
	ch := make(chan json.RawMessage, 1)
	c.res[id] = ch
	err := c.ws.WriteJSON(map[string]any{"id": id, "method": method, "params": params})
	c.mu.Unlock()
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		c.t.Fatalf("%s: no answer", method)
		return nil
	}
}

// eval runs JavaScript in the page and returns its value as JSON.
func (c *chrome) eval(js string) json.RawMessage {
	c.t.Helper()
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	json.Unmarshal(c.call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "awaitPromise": true}), &r)
	if r.ExceptionDetails != nil {
		c.t.Fatalf("in the page: %s", r.ExceptionDetails.Exception.Description)
	}
	return r.Result.Value
}

func (c *chrome) str(js string) string {
	var s string
	json.Unmarshal(c.eval(js), &s)
	return s
}

func (c *chrome) open(url string) {
	c.t.Helper()
	c.call("Page.navigate", map[string]any{"url": url})
	c.waitFor(`document.readyState === "complete"`)
}

// waitFor waits until the JavaScript condition is true (10 s).
func (c *chrome) waitFor(cond string) {
	c.t.Helper()
	for i := 0; i < 100; i++ {
		if string(c.eval("!!("+cond+")")) == "true" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("waiting for %s; the page says: %q", cond, c.str(`document.body.innerText.slice(0, 400)`))
}

// mouseClick presses and releases the left mouse button at a point of the viewport.
func (c *chrome) mouseClick(x, y float64) {
	c.t.Helper()
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		c.call("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1})
	}
}
