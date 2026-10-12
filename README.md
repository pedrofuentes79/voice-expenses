# voice-expenses

Log expenses by voice from an iPhone home-screen web app. Audio is transcribed
(OpenAI `gpt-4o-mini-transcribe`, or local faster-whisper for testing) and parsed
into entries by `gpt-5-nano`.

This is the Python prototype used to test prompts; the app itself is being
rewritten in Go to run on a Raspberry Pi.

## Run

```
cp config.example.json config.json   # or: uv run python import_kesef.py <kesef-backup.json>
echo 'OPENAI_API_KEY=...' > .env      # optional: APP_TZ=<IANA time zone>, default UTC
uv run uvicorn server:app --port 8000
```

`terms.txt` (optional, one per line) lists names you say often; they're passed to the
transcriber as hints. `config.json`, `terms.txt`, `.env` and `data/` are gitignored.
