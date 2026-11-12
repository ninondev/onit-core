# onit-core

Go backend for Onit, a personal adviser that keeps its data on your own
machine. One static binary, one SQLite file, one loopback HTTP API. No
network calls unless you configure a model endpoint yourself.

This repository contains the backend only. The web frontend and the
Android app are developed separately and are not part of it. Code
comments are written in Chinese.

## License

PolyForm Strict License 1.0.0. In plain terms: you may read the code and
run it for noncommercial purposes. You may not modify it, build new works
on it, or redistribute it, and you may not use it commercially. The full
text is in [LICENSE](LICENSE). Third-party module notices are in
[licenses/THIRD-PARTY.md](licenses/THIRD-PARTY.md).

## Build and run

Go 1.26 or newer. The default build uses the pure-Go SQLite driver and
needs no C compiler.

```sh
go build -o build/adviser .
./build/adviser init -dir ~/onit-demo
./build/adviser doctor -dir ~/onit-demo
./build/adviser serve -dir ~/onit-demo
```

`init` refuses existing directories and symlinks, creates `manifest.json`,
`credentials/`, `seed/`, `memory.sqlite`, and a first owner credential.
Directories get 0700, files 0600. `doctor` checks the manifest, database
integrity, credential permissions, that the directory is writable, and the
Go version, and exits nonzero on any failure.

`serve` listens on `127.0.0.1:7341` by default and only on numeric
loopback addresses; `-listen` changes the port. `GET /` returns 404 by
design: this is an API for clients, not a web page.

Add a second owner with `issue`, then send `Authorization: Bearer
<token>` on every request. Credentials are read from 0600 files only;
they never appear in URLs, command lines, or logs.

## Demo data

`seed` writes two sources, one goal, and one disabled preparation policy
through the normal HTTP API, all titled as synthetic (demo) data. It needs
no model key and reaches no network. Run it against a running service:

```sh
./build/adviser seed -dir ~/onit-demo -base http://127.0.0.1:7341 \
  -token-file ~/onit-demo/credentials/owner-a.token
```

Adding `-enable` turns the policy on and produces one preparation output
that is checked against a canary string. Re-running is idempotent and
resumes after interruption.

## Backup and restore

Both require the service to be stopped. `backup` copies the whole data
directory, records size, SHA-256, and permissions per file, and refuses
to run while the service holds its lock. `restore` writes only to a
directory that does not exist yet, verifies every digest first, and
leaves nothing behind on failure.

```sh
./build/adviser backup -dir ~/onit-demo -out ~/onit-backup
./build/adviser restore -backup ~/onit-backup -dir ~/onit-restored
```

## HTTP API

| Operation | Endpoint | Notes |
| --- | --- | --- |
| Add a source | `POST /v1/sources` | `kind` is `self`, `mail`, or `file`; original text is kept. |
| List and open sources | `GET /v1/sources`, `GET /v1/sources/{id}` | Lists return summaries; the body comes only on open. |
| Read the portrait | `GET /v1/portrait` | Each memory carries its current statement, evidence quote, source id, version, and confirmation state. |
| Confirm or correct | `PATCH /v1/memories/{id}` | Send the new text and the version just read. A conflict returns 409. |
| Forget an observation | `DELETE /v1/memories/{id}` | The same evidence is skipped when the source is read again. |
| Remove a source | `DELETE /v1/sources/{id}` | Clears memories that rely on it alone; selected memories can be kept. |
| Propose and accept a goal | `POST /v1/proposals`, `POST /v1/proposals/{id}/accept` | Acceptance takes `title`, `step`, `unit`, and optionally the basis memory and its version. |
| List goals | `GET /v1/goals` | Shows the accepted basis and its current version. |
| Read preparations | `GET /v1/preparation` | Revalidates the basis on every read; withdrawn sources mark old outputs stale. |
| Save or pause preparation | `PUT /v1/preparation/{clientKey}` | Enabling requires a request and selected sources. Pausing survives restarts. |
| Advance one tick | `POST /v1/preparation/tick` | Advances the current account only. |
| Model destination | `GET /v1/model` | Returns the configured endpoint and model. |
| Model reading | `POST /v1/sources/{id}/read` | Sends one selected passage with reading instructions; citations are validated against the original. |

A model endpoint is optional. Start `serve` with `-endpoint` and
`-model` to enable it; it must speak Chat Completions with JSON Schema
structured output. Users supply their own key per request, and the key is
never written to the database.

## Android

The same code builds for Android with CGO and the mattn/go-sqlite3
driver. On Linux, for the x86_64 emulator:

```sh
CGO_ENABLED=1 GOOS=android GOARCH=amd64 \
  CC="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin/x86_64-linux-android26-clang" \
  go build -buildmode=pie -o build/adviser-android-x86_64 .
```

ARM64 uses `GOARCH=arm64` and `aarch64-linux-android26-clang`; on macOS
the prebuilt host directory is `darwin-x86_64`. The minimum API level is
26. `CGO_ENABLED=0` builds are not valid for Android; the build
constraints in `internal/sqlitecgo` enforce this. On desktop,
`-tags onit_sqlite_native` switches to the CGO driver for testing.

## Verification

```sh
go test -count=1 ./...
CGO_ENABLED=1 go test -tags onit_sqlite_native -count=1 ./...
python3 verify_driver_exchange.py
```

The second command needs a local C compiler. The third builds both
drivers, has modernc write the synthetic fixture and a selected goal, lets
the native driver correct one sentence, and has modernc read the
correction, the other fact, the original text, and the action back. The
results land in `build/sqlite-driver-exchange/`.
`verify_android.py` runs the same flow over adb on a disposable ARM64
emulator. Three tests that replay browser modules from the frontend tree
skip themselves when `../src/web` is absent.

## Limits

The server binds loopback only and ships no TLS. Reaching it from a phone
requires a reverse proxy in front; nothing beyond loopback has been
validated here. First-run pairing with the Android app lives in the
private tree and is not exercised by this repository.
