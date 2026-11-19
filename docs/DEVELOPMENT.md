# Development

## Layout

| Path | What |
|---|---|
| `cmd/backup-manager/` | the program: `serve`, command line, agent mode, built-in HTTPS, `doctor`, recovery commands |
| `internal/core/` | engines (logical, physical, binary logs, PostgreSQL, Redis, folders), queue, scheduler, catalog, encryption, storage, sandbox, agents |
| `internal/api/` | HTTP API, roles, two-factor, tokens, metrics; `web/index.html` is the whole panel (no build step) |
| `install.sh` | the installer |
| `Dockerfile`, `docker-compose.yml` | container install |
| `scripts/` | `build.sh`, `release.sh`, `publish-release.sh` |
| `test/` | end-to-end tests ([test/README.md](../test/README.md)) |
| `VERSION` | the version written into builds and releases |

## Build

Go 1.27 or newer, no C compiler (`CGO_ENABLED=0`; SQLite is pure Go).

```bash
scripts/build.sh            # -> bin/backup-manager
scripts/build.sh test       # unit tests
go vet ./...
```

`scripts/build.sh` runs the compiler under a memory cap and low priority, so it is safe on a server that also runs
production work. It looks for Go in `/root/sdk/go/bin` or `GOROOT_BIN`, else on `PATH`.

## Tests

- Unit tests: `scripts/build.sh test`.
- End-to-end rounds against throwaway containers: [test/README.md](../test/README.md) (`01`–`09`).
- Installer on fresh machines with real systemd (Debian 12, Ubuntu 22.04 / 24.04, Rocky 9): `test/installer/`.
- Panel in a headless browser: `test/browser/*.mjs`.

## Release

1. Set the new version in `VERSION`, commit.
2. `scripts/release.sh` → `dist/` with `backup-manager-linux-amd64`, `backup-manager-linux-arm64`, `SHA256SUMS`,
   `VERSION`.
3. `GITHUB_TOKEN=… scripts/publish-release.sh` creates the release `v<VERSION>` on GitHub and uploads `dist/`.
   (Or create the release in the GitHub web page and upload the four files.)

The installer and `backup-manager upgrade` download `releases/latest`, so the newest release is what new installs get.
