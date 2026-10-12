# syntax=docker/dockerfile:1

FROM golang:1.26.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/q41eu-raw ./cmd/q41eu-raw

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/q41eu-raw /q41eu-raw
EXPOSE 8765
USER nonroot:nonroot
# Mount the robot token at /secrets/robot-token and pass -public-url https://<host>.
# With a read-only root file system there is no state file by default; mount a
# volume and pass -state-file /state/state.json to keep pairings across restarts.
ENTRYPOINT ["/q41eu-raw", "-listen", ":8765", "-token-file", "/secrets/robot-token", "-state-file", ""]
