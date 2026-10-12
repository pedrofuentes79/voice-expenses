# voice-expenses

Log expenses by voice from an iPhone home-screen web app, self-hosted on a Raspberry Pi.

Tap, say "almorcé en la facu, 8 lucas con débito", and it becomes an entry: audio goes to
OpenAI `gpt-4o-mini-transcribe`, then `gpt-5-nano` turns the transcript into amount,
currency, category, place and payment method for you to confirm.

**Offline-first.** The app shell is cached by a service worker and all data lives in
IndexedDB, so the app opens, records, and edits with no connection (or with the server
unreachable). Changes queue in an outbox and sync, idempotently, when the server is back.

## Layout

- `*.go` — one binary: SQLite (pure Go, no cgo), OpenAI over plain HTTP, the web app embedded.
- `web/` — vanilla JS PWA: `app.js` (IndexedDB + outbox sync), `sw.js` (offline shell).
- `deploy/deploy.sh` — cross-compiles for arm64, installs a systemd unit, `tailscale serve`s it.
- `prototype/` — the Python prototype used to compare transcription engines and prompts.

## Run

```
mkdir -p data && cp prototype/config.example.json data/config.json
echo 'OPENAI_API_KEY=...' > .env          # optional: APP_TZ=<IANA zone>, ADDR, DATA_DIR
go run .                                 # http://127.0.0.1:8000
DEPLOY_HOST=user@host deploy/deploy.sh   # serves https://<host>.<tailnet>.ts.net:9443
```

`data/config.json` holds categories (with one-line descriptions the model reads), payment
methods and example entries; `prototype/import_kesef.py` builds it from a Kesef backup.
`data/terms.txt` (one per line) lists names you say often, passed to the transcriber as hints.
`data/` and `.env` are gitignored.
