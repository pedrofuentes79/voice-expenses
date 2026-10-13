# Plan

## Where it stands

- Go server on the Pi (systemd, `tailscale serve`, ~12 MB RSS): SQLite, OpenAI transcription
  (`gpt-4o-mini-transcribe`) and parsing (`gpt-5-nano`, strict JSON schema).
- iPhone PWA: tap to record, list by day, review/edit sheet, manual entry, audio playback.
- Offline-first: service-worker shell, IndexedDB mirror, outbox with idempotent recordings.
  Verified in headless Chromium; not yet on iOS Safari.
- Categories, descriptions, payment methods and examples come from `data/config.json`
  (built from a Kesef backup); `data/terms.txt` holds transcription hints.

## Short term

1. **Validate on the iPhone.** Airplane mode → reopen app → record two, edit one, add one
   manually → reconnect → check nothing is lost or duplicated. Repeat after a phone restart.
2. **Bluetooth headphones.** A recording made with headphones connected came out unintelligible.
   Try forcing the built-in mic (`enumerateDevices` + `deviceId`); if iOS doesn't allow it,
   show which input is active before recording.
3. **Backups.** Nightly copy of `app.db` (via `sqlite3 .backup` or `VACUUM INTO`) plus
   `audio/`, keeping the last N; hook into the Pi's existing backup job.
4. **Commit the tests.** Move the Playwright offline test into `e2e/`; add Go tests for
   recording idempotency and last-write-wins on edits.
5. **Learn from corrections.** Use recently confirmed entries (and the fields the user changed)
   as the prompt's examples, replacing the static Kesef ones.
6. **Config without restarts.** Reload `config.json` on change; later, edit categories and
   payment methods from the app.
7. **Small UX fixes.**
   - Play pending recordings offline, from IndexedDB.
   - Undo after delete.
   - A "to review" filter.

## Medium term

- **Money Manager import.** Full history as entries, and as a much better example set for
  categorization. Needs a look at its export format first.
- **Desktop view.** Table with filters and search, monthly totals per category (ARS and USD
  kept separate, never converted), bulk edit.
- **CSV export.**
- **Prompt regression set.** Saved recordings with expected entries, rerun when the prompt,
  model or category descriptions change.

## Integrations

### Apple Pay

A Shortcuts *Transaction* automation fires on each Apple Pay payment (amount, merchant, card).

- The shortcut first appends `{id, amount, merchant, card, date}` to a local file (works offline).
- Then it POSTs the whole file to a new `POST /api/inbox` (batch, idempotent by `id`) and
  clears the file only on success.
- Flush-only automations retry the queue: charger connected, a frequently used app opened,
  a daily time.
- Server side: map card → payment method, merchant → category through the model, and store
  as `review`.

### Mercado Pago

- Get an access token and curl `/v1/payments/search?payer.id=…` to confirm QR payments,
  purchases and transfers show up when you are the payer.
- If they do: a polling goroutine every 10–15 min, deduplicated by payment id. No public
  endpoint needed.
- Webhooks only if polling falls short. They need a public URL: expose only that path through
  the Pi's Cloudflare tunnel and verify `x-signature`.
- Fallback if the API doesn't cover it: parse the per-payment receipt emails.

### Cross-source duplicates

An Apple Pay payment made with a Mercado Pago card would arrive twice. Match on amount and
a time window, then flag the pair for review instead of merging silently.

### Capture shortcuts

Back Tap or "Hey Siri" → record → POST to `/api/recordings`, for logging without opening the app.

## Open questions

- Can iOS Safari pick the built-in mic while headphones are connected?
- Access is tailnet-only today. Anything public (webhooks) needs auth on the rest of the API.
- Should confirmed entries ever go back to "review" when a duplicate shows up later?
