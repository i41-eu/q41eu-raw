# From prototype to v1

**Status:** plan; updated with each release.

## Where it stands

`q41eu-raw` works and is used: one robot, every day, on a home network, with
phones and laptops pairing by QR; the public instance at `raw.q41.eu` runs the same code. It is a **prototype** in these ways:

| Area | Today | Why it matters |
|---|---|---|
| Robot identity | the owner's robots share one token; other robots get per-robot tokens (accounts, invites); the release firmware has no token, it is written over USB | reading one of the owner's robots' flash gives the token for all of them |
| Permissions | with sign-in, robots are private to their owner by default, with a public toggle; tiers set the limits; a paired browser may use every command and see all data | no guests, no "camera only for parents", no time limits |
| Network | plain `ws://` and `http://` on the LAN (`-tls-listen` for browsers only); TLS otherwise only behind a proxy | anyone on the network can listen |
| State | in memory, plus a JSON snapshot on restart; one instance only | a crash or a deploy loses live state |
| Audio | raw PCM, about 48 KB/s | too heavy for mobile networks |
| Quality checks | tests run on the developer's machine; CI only builds release images | the v0.2.0 and v0.10.0 image builds broke without anyone noticing before the tag (v0.10.0: the Dockerfile lacked the `e2e` package) |
| Abuse | join requests, failed robot logins and wrong pairing codes are limited per address; invited robots have send budgets; browsers' streams and commands are limited per session, address and tier | an external review is still to come |

## Stages

Each stage leaves a release that is better than the one before, and has a clear
**done when**.

### 1. Solid on a home network

- **CI on every push:** gofmt, vet, race tests, and a container build, so a broken image
  is found before a tag.
- **Fuzz tests** for everything robots and browsers send: `wire.Parse`, frame bodies,
  binary messages (media, files), with size limits checked.
- **A browser security pass:** the session cookie's flags, origin checks on every
  WebSocket and state-changing request, request size limits, and a content security
  policy for the dashboard.
- **The version everywhere:** in `/api/info`, on the dashboard, and in the robot's
  `Register`, so a mismatch between firmware and server is visible.
- **Restart behaviour tested:** a server restart, a robot that reconnects with backoff,
  and a browser that resumes its event stream, all covered by tests.

**Done when:** someone else can follow the robot setup guide on their own network and it
works, CI is green, and the fuzzers have run without findings.

### 2. Each robot its own identity

**First step, done:** per-robot tokens, for robots added by signed-in accounts and for
invited robots (`-robot-tokens-file`, `q41eu-raw invite`): each bound to its robot id
and revocable on its own; the server keeps only the tokens' hashes. They are still bearer
tokens on the robot, so the steps below remain.

- An **owner key** and a small tool that signs robot certificates and configuration.
- A **device key per robot** (in its storage first, in the ESP32-S3's secure key
  peripheral later), proved with a challenge instead of the shared token.
- **Revoking one robot** without touching the others.

**Done when:** reading one robot's flash compromises only that robot, and the shared
token is gone.

### 3. Permissions per person, robot, capability and time

- Permission entries: who, which robot, which commands or data (camera, microphone,
  movement…), and for how long; enforced by the server and checked again by the robot.
- **Guests:** a QR scan on the robot gives a small set of rights for an hour.
- **An audit view:** who used which robot, which commands, and who watched the camera, in
  plain words.

**Done when:** a parent can let a babysitter see one robot's camera for an evening, and
nothing else, and can read afterwards what was used.

### 4. Safe on the internet

- **End-to-end encryption through the public instance:** the relay is done: the server
  passes sealed frames and media between a robot and its browsers without reading them
  ([e2ee.md](https://github.com/i41-eu/i41eu-arch/blob/main/docs/e2ee.md)). Still open: TLS
  ending on the owner's own server.
- **Compressed audio** (Opus) for the microphone and the speaker.
- **Abuse limits:** done: failed robot logins and wrong pairing codes per address (with
  `-trusted-proxies`, so a forged `X-Forwarded-For` cannot dodge it; raw.q41.eu gets real
  client addresses through the PROXY protocol from its load balancer), message and byte
  budgets for invited robots, and browser limits per session, address and tier. Still open:
  short log retention on the public instance.
- **An external security review** of the protocol and the server.

**Done when:** the public instance can be offered to people we do not know without
reading or storing their traffic, and the review's findings are fixed.

### 5. Version 1.0

- **Wire protocol v2** frozen, with the compatibility rule written down: which firmware
  versions a server accepts, and for how long.
- **Releases** with signed container images and a software bill of materials. The robot
  firmware is already built reproducibly in a pinned container, so anyone can compare
  hashes ([mj41cz-approved](https://gitlab.com/mj41cz/mj41cz-approved)).
- **Documentation** for owners (setup, everyday use, privacy) and for app developers (the
  protocol, the command catalog, an example app).

**Done when:** a 1.0 tag, and an upgrade from it to the next minor version works without
re-pairing.

## Not planned

- Accounts that hold owners' keys: sign-in only names who adds a robot and the tier.
- Recording by default: media streams only while someone watches or listens, as today.
