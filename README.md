# git-forge-core

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/yniantongtian-oss/git-forge-core/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/yniantongtian-oss/git-forge-core/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in one static Go binary, built from the standard library alone (zero dependencies, no git binary). It serves git smart HTTP (upload-pack protocol v2 with a v0 fallback, receive-pack v0), the REST v3 API, GraphQL v4 and an MCP endpoint from one in-memory store, so a repository or issue created over one surface reads back unchanged over the others.

- `cmd/forge/` is the forge: a raw-TCP HTTP/1.1 keep-alive server, packfile reader/writer with lazy cached deflate, pre-rendered and per-ref-version cached git replies, single-flight clone caching, and the REST, GraphQL and MCP surfaces.
- `main.ts` is a thin launcher: it starts the prebuilt binary for the platform (`forge-linux`, or `forge-win.exe` on Windows), forwards SIGINT/SIGTERM and exits with the binary's exit code.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o forge-linux ./cmd/forge
go build -ldflags="-s -w" -trimpath -o forge-win.exe ./cmd/forge   # local Windows runs (git-ignored)
```

## Run it

```sh
node --experimental-strip-types main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=yq04c8zw&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
