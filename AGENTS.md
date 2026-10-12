# q41eu-raw

Go relay server for Stackchan Embody Mode. See [readme.md](readme.md) for how to run it and for the protocol.

- Go only. Use the standard library, plus gorilla/websocket, and pgx for Postgres (package `statestore` only). Sign-in goes through the manager (package `sso`).
- Protocol change: the spec in `i41eu-arch` `docs/wire-protocol.md` first, then `wire/wire.go`, then the Commands table in `readme.md` (frames and binary types are only linked from there).
- The firmware must be able to parse frames with cJSON: flat JSON objects, one frame per message from the server to the robot.
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.
- Test end to end without hardware with `go run ./cmd/fake-robot`.
