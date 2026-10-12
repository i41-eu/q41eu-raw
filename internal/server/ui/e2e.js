// End-to-end encryption in the browser (i41eu-arch docs/e2ee.md; Go reference: package e2e).
// The relay carries only ciphertext between a robot and the browsers enrolled through its
// QR code. WebCrypto only: X25519, HKDF-SHA256, HMAC-SHA256, AES-256-GCM. The browser's
// private key is non-extractable and stays in this browser's IndexedDB.
"use strict";
const E2E = (() => {
  const te = new TextEncoder(), td = new TextDecoder();
  const subtle = crypto.subtle;
  const ENROLL = "i41-e2e-enroll|", PAIRWISE = "i41-e2e pairwise", GROUP = "i41-e2e group|";

  const b64u = {
    enc: (u8) => btoa(String.fromCharCode(...u8)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, ""),
    dec: (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4)), (c) => c.charCodeAt(0)),
  };
  const hex = (u8) => Array.from(u8, (b) => b.toString(16).padStart(2, "0")).join("");
  const concat = (...parts) => {
    const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let i = 0;
    for (const p of parts) { out.set(p, i); i += p.length; }
    return out;
  };

  // --- storage: one IndexedDB object store, key -> value (CryptoKeys clone fine) ---
  let dbp;
  const db = () => dbp || (dbp = new Promise((resolve, reject) => {
    const r = indexedDB.open("i41-e2e", 1);
    r.onupgradeneeded = () => r.result.createObjectStore("kv");
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  }));
  const kv = async (mode, fn) => {
    const d = await db();
    return new Promise((resolve, reject) => {
      const tx = d.transaction("kv", mode);
      const req = fn(tx.objectStore("kv"));
      tx.oncomplete = () => resolve(req && req.result);
      tx.onerror = () => reject(tx.error);
    });
  };
  const get = (k) => kv("readonly", (s) => s.get(k));
  const put = (k, v) => kv("readwrite", (s) => s.put(v, k));

  // --- primitives ---
  async function x25519Shared(priv, peerRaw) {
    const peer = await subtle.importKey("raw", peerRaw, { name: "X25519" }, true, []);
    return new Uint8Array(await subtle.deriveBits({ name: "X25519", public: peer }, priv, 256));
  }
  async function hkdf(ikm, salt, info) {
    const k = await subtle.importKey("raw", ikm, "HKDF", false, ["deriveBits"]);
    return new Uint8Array(await subtle.deriveBits({ name: "HKDF", hash: "SHA-256", salt, info: te.encode(info) }, k, 256));
  }
  async function hmac(key, msg) {
    const k = await subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    return new Uint8Array(await subtle.sign("HMAC", k, te.encode(msg)));
  }
  const aesKey = (raw) => subtle.importKey("raw", raw, "AES-GCM", false, ["encrypt", "decrypt"]);
  const seal = async (key, nonce, plain, aad) =>
    new Uint8Array(await subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData: aad }, key, plain));
  const open = async (key, nonce, sealed, aad) =>
    new Uint8Array(await subtle.decrypt({ name: "AES-GCM", iv: nonce, additionalData: aad }, key, sealed));
  async function browserIdOf(pubRaw) {
    return hex(new Uint8Array(await subtle.digest("SHA-256", pubRaw)).slice(0, 8));
  }
  async function pairwiseRaw(priv, peerRaw, rPub, bPub) {
    return hkdf(await x25519Shared(priv, peerRaw), concat(rPub, bPub), PAIRWISE);
  }
  // Nonces: 4 random bytes per page load, then a counter (never repeat under one key).
  const prefix = crypto.getRandomValues(new Uint8Array(4));
  let counter = 0n;
  const nonce = () => {
    const n = new Uint8Array(12);
    n.set(prefix);
    new DataView(n.buffer).setBigUint64(4, ++counter);
    return n;
  };

  // --- this browser's key ---
  let mine;
  async function me() {
    if (mine) return mine;
    let pair = await get("browser");
    if (!pair) {
      pair = await subtle.generateKey({ name: "X25519" }, false, ["deriveBits"]); // private part non-extractable
      await put("browser", pair);
    }
    const pub = new Uint8Array(await subtle.exportKey("raw", pair.publicKey));
    mine = { pair, pub, id: await browserIdOf(pub) };
    return mine;
  }

  // --- per robot: its key (from the QR code), our pairwise key, group keys by epoch ---
  const robots = new Map(); // id -> { rPub, k, groups: Map(epoch -> CryptoKey), seq, telemetry }
  async function robot(id) {
    let r = robots.get(id);
    if (!r) {
      const saved = await get("robot:" + id);
      r = { rPub: saved ? b64u.dec(saved.rPub) : null, k: null, groups: new Map(), seq: Date.now() * 1000, telemetry: {} };
      robots.set(id, r);
    }
    if (r.rPub && !r.k) {
      const m = await me();
      r.k = await aesKey(await pairwiseRaw(m.pair.privateKey, r.rPub, r.rPub, m.pub));
    }
    return r;
  }

  const post = (id, kind, body) => fetch("/api/robots/" + encodeURIComponent(id) + "/e2e", {
    method: "POST", headers: { "Content-Type": "application/json" }, credentials: "same-origin",
    body: JSON.stringify({ kind, body }),
  }).then(async (res) => { if (!res.ok) throw new Error((await res.text()).trim() || res.statusText); });

  // Data under an epoch we have no key for: the robot started anew or forgot a browser. Ask for
  // the current group key (at most every 10 s); a forgotten browser gets no answer.
  const newEpoch = async (id, r, epoch) => {
    if (!r.k || epoch < Math.max(0, ...r.groups.keys()) || Date.now() - (r.helloAt || 0) < 10000) return;
    r.helloAt = Date.now();
    try { await post(id, "E2EHello", { b: (await me()).id }); } catch {}
  };

  let pending = null; // { id, rPub, p } from the QR code's fragment, until enrolled

  return {
    b64u, hex,

    // The pairing URL's fragment (#e2e=1.<R_pub>.<P>): never sent to the server. Take it and
    // remove it from the address bar.
    takeFragment(robotId, hash = location.hash) {
      const m = /^#e2e=1\.([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)$/.exec(hash);
      if (location.hash) history.replaceState(null, "", location.pathname + location.search);
      if (!m) return false;
      pending = { id: robotId, rPub: b64u.dec(m[1]), p: b64u.dec(m[2]) };
      return true;
    },

    // Called for every robot view: enroll if we just scanned it, or say hello to get the
    // current group key.
    async noteRobot(view) {
      if (!view.e2e || !view.online) return;
      if (pending && pending.id === view.id) {
        const p = pending;
        pending = null;
        const m = await me();
        const mac = await hmac(p.p, ENROLL + b64u.enc(p.rPub) + "|" + b64u.enc(m.pub));
        await put("robot:" + view.id, { rPub: b64u.enc(p.rPub) });
        const r = robots.get(view.id);
        if (r) { r.rPub = p.rPub; r.k = null; }
        await robot(view.id);
        await post(view.id, "E2EEnroll", { b: b64u.enc(m.pub), mac: b64u.enc(mac) });
        return;
      }
      const r = await robot(view.id);
      if (r.rPub && !r.groups.size && !r.helloAt || (r.helloAt && Date.now() - r.helloAt > 10000 && !r.groups.size)) {
        r.helloAt = Date.now();
        await post(view.id, "E2EHello", { b: (await me()).id });
      }
    },

    // known: we have the robot's key (scanned its QR code); ready: we can read it.
    known: (id) => !!(robots.get(id) && robots.get(id).rPub),
    ready: (id) => !!(robots.get(id) && robots.get(id).groups.size),
    telemetry: (id) => (robots.get(id) && robots.get(id).telemetry) || {},

    // An SSE "e2e" event: {robot, kind, body}. Returns the opened frame {kind, body} or null.
    async onEvent(ev) {
      const r = await robot(ev.robot);
      const b = ev.body;
      if (ev.kind === "E2EGroupKey") {
        if (!r.k || b.b !== (await me()).id) return null;
        const g = await open(r.k, b64u.dec(b.n), b64u.dec(b.c), te.encode(GROUP + ev.robot + "|" + b.epoch));
        r.groups.set(b.epoch, await aesKey(g));
        return { kind: "E2EGroupKey", body: { epoch: b.epoch } };
      }
      if (ev.kind === "E2EData") {
        const g = r.groups.get(b.epoch);
        if (!g) { newEpoch(ev.robot, r, b.epoch); return null; }
        const f = JSON.parse(td.decode(await open(g, b64u.dec(b.n), b64u.dec(b.c), te.encode(ev.robot))));
        if (f.kind === "RobotTelemetry" && f.body.measurements) Object.assign(r.telemetry, f.body.measurements);
        return f;
      }
      return null;
    },

    // A 0x30 media message: returns the inner message (type byte + payload) or null.
    async openBinary(robotId, u8) {
      const r = robots.get(robotId);
      if (!r || u8.length < 33) return null;
      const epoch = new DataView(u8.buffer, u8.byteOffset).getUint32(1);
      const g = r.groups.get(epoch);
      if (!g) { newEpoch(robotId, r, epoch); return null; }
      try { return await open(g, u8.subarray(5, 17), u8.subarray(17), te.encode(robotId)); } catch { return null; }
    },

    // A plaintext browser -> robot message (speaker audio) as 0x31.
    async sealBinary(robotId, u8) {
      const r = await robot(robotId);
      const m = await me();
      const n = nonce();
      const c = await seal(r.k, n, u8, te.encode(robotId));
      const idBytes = Uint8Array.from(m.id.match(/../g), (h) => parseInt(h, 16));
      return concat(Uint8Array.of(0x31), idBytes, n, c);
    },

    // A command sealed under our pairwise key, with a sequence number the robot checks.
    async sendCommand(robotId, command, args) {
      const r = await robot(robotId);
      if (!r.k) throw new Error("not enrolled with this robot: scan its QR code");
      const n = nonce();
      const plain = te.encode(JSON.stringify({ command, args: args || {}, seq: ++r.seq }));
      const c = await seal(r.k, n, plain, te.encode(robotId));
      await post(robotId, "E2ECommand", { b: (await me()).id, n: b64u.enc(n), c: b64u.enc(c) });
    },

    // The Go reference's test vectors (e2e/testdata/vectors.json): returns the failures.
    async selfTest(v) {
      const bad = [];
      const check = (name, got, want) => { if (got !== want) bad.push(`${name}: got ${got}, want ${want}`); };
      const fromHex = (h) => Uint8Array.from(h.match(/../g), (x) => parseInt(x, 16));
      // Private keys from raw bytes: WebCrypto imports X25519 private keys as PKCS#8.
      const pkcs8 = (raw) => concat(fromHex("302e020100300506032b656e04220420"), raw);
      const rPriv = await subtle.importKey("pkcs8", pkcs8(fromHex(v.RobotPriv)), { name: "X25519" }, false, ["deriveBits"]);
      const bPriv = await subtle.importKey("pkcs8", pkcs8(fromHex(v.BrowserPriv)), { name: "X25519" }, false, ["deriveBits"]);
      const rPub = b64u.dec(v.RobotPub), bPub = b64u.dec(v.BrowserPub);
      check("browser id", await browserIdOf(bPub), v.BrowserID);
      const p = b64u.dec(v.Secret);
      check("enroll mac", b64u.enc(await hmac(p, ENROLL + v.RobotPub + "|" + v.BrowserPub)), v.EnrollMAC);
      check("pairwise (browser)", hex(await pairwiseRaw(bPriv, rPub, rPub, bPub)), v.Pairwise);
      check("pairwise (robot)", hex(await pairwiseRaw(rPriv, bPub, rPub, bPub)), v.Pairwise);
      const k = await aesKey(fromHex(v.Pairwise));
      const g = await open(k, b64u.dec(v.GroupKeyNonce), b64u.dec(v.GroupKeySealed), te.encode(GROUP + v.RobotID + "|" + v.Epoch));
      check("group key", hex(g), v.GroupKey);
      const bin = fromHex(v.BinarySealed);
      const gk = await aesKey(g);
      const inner = await open(gk, bin.subarray(5, 17), bin.subarray(17), te.encode(v.RobotID));
      check("binary inner", inner[0], v.BinaryInner);
      check("binary payload", hex(inner.subarray(1)), v.BinaryPayload);
      check("binary sealed", hex(concat(Uint8Array.of(0x30), bin.subarray(1, 5), fromHex(v.BinaryNonce),
        await seal(gk, fromHex(v.BinaryNonce), inner, te.encode(v.RobotID)))), v.BinarySealed);
      check("command sealed", b64u.enc(await seal(k, b64u.dec(v.CommandNonce), te.encode(v.CommandPlain), te.encode(v.RobotID))), v.CommandSealed);
      return bad;
    },
  };
})();
