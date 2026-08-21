# Voicemail to Telegram v2

Go rewrite of the original Python setup. One binary replaces the three
Python scripts (`main.py`, `telegram_listener.py`, `emailcleanup.py`) and adds
a small web dashboard.

## What it does

- **Mail watcher** — watches the IMAP inbox for PBX voicemail mails, transcribes
  the attached audio with Google Speech-to-Text and forwards the mail text +
  transcription + audio to the Telegram group. It holds one logged-in
  connection open and lets the server push new-mail notifications (IMAP IDLE),
  so a voicemail reaches Telegram within a second instead of waiting for the
  next poll. See [IMAP connection](#imap-connection).
- **Telegram commands** — `/deletevm`, `/vivia`, `/avics`, storingsdienst
  switches per phone number, `/info`, customer lookups from `customers.json`
  and of course `/lol`. All commands are defined in `config.yaml` — nothing is
  hardcoded anymore. `phone_numbers` keys may be full names ("Sjoerd van
  Dijk"); the Telegram command is the generated slug (`/sjoerd_van_dijk`),
  with a first-name shortcut (`/sjoerd`) when unambiguous — entries with a
  `(...)` suffix don't count against the shortcut. `/storingsdienst` lists
  all generated commands.
- **Weekly cleanup** — archives the inbox into `INBOX.<year>.<week-1>-<week>`
  (Friday 09:00 by default, configurable cron expression).
- **Web dashboard** — voicemail history with audio playback, transcription
  search, activity log and buttons for the same PBX actions. Voicemails can be
  marked as afgehandeld (with an open/done filter and open counter), so during
  storingsdienst you can track which meldingen are handled. Plain TypeScript,
  embedded into the binary; served on `:8080`.

## Differences from the Python version

- **No more audio splitting.** With `transcription.gcs_bucket` set, every
  voicemail is transcribed via GCS (uploaded, transcribed, deleted) — one code
  path that is exercised on every message. Without a bucket, audio is sent
  inline, which Google limits to ~60s; a longer voicemail is still delivered
  to Telegram, just with a "transcriptie mislukt" note. Splitting hurt
  accuracy at segment boundaries and is gone.
- **One IMAP login instead of thousands.** The Python version reconnected and
  logged in on every poll; hosted providers throttle repeated authentication as
  a brute-force defence, which showed up as occasional login timeouts and
  rejections. See [IMAP connection](#imap-connection).
- **No duplicate sends.** A single process with an exclusive lock file, plus
  every processed mail's Message-ID recorded in SQLite — a voicemail can never
  be forwarded twice, even across restarts or when the IMAP seen-flag race
  occurs.
- **One config file** (`config.yaml`, supports `${ENV_VAR}` references) instead
  of `config.ini` + `phone_numbers.ini`. `customers.json` still works as
  before and is hot-reloaded.
- **`/info` is generated dynamically** from the config (commands with their
  descriptions, storingsdienst commands, customer lookups) so it can never go
  stale. An optional `info_file` is appended below the list for free-form
  notes.
- Commands only work in the configured group chat (`telegram.chat_id`), not in
  any group the bot happens to be in.
- The Google service account key is mounted at runtime, not baked into the
  Docker image.

## Running locally

```sh
cp config/config.example.yaml config/config.yaml   # edit it
cd web && npm ci && npm run build && cd ..          # build the frontend
go run ./cmd/voicemailbot -config config/config.yaml
```

## Docker

The easiest way is docker compose:

```sh
cp config/config.example.yaml config/config.yaml   # edit it
cp .env.example .env                               # fill in the secrets
docker compose up -d --build
```

Or plain docker:

```sh
docker build -t voicemailapp:v2 .
docker run -d --name voicemailapp --restart unless-stopped \
  -v /path/to/config:/config \
  -v voicemail-data:/data \
  -p 8080:8080 \
  voicemailapp:v2
```

`/config` must contain `config.yaml`, `googlekey.json` and optionally
`info.txt` / `customers.json`. `/data` holds the SQLite database and stored
audio files.

## IMAP connection

The watcher keeps a single authenticated connection open for as long as the
server allows, rather than dialling and logging in on a timer. At a 30 second
interval the old approach meant roughly 2,900 logins a day, and the login — not
the inbox check — is the part mail hosts rate-limit.

On that connection it uses **IDLE** (RFC 2177): the server pushes a
notification the moment mail arrives, so there is no polling delay at all. Three
things keep that honest:

- `idle_fallback_interval` (default `2m`) runs a normal search anyway. It
  catches a missed notification and, because a dead connection fails the
  search, bounds how long the watcher can be silently blind.
- Servers that do not advertise IDLE fall back to plain polling every
  `poll_interval`. Set `use_idle: false` to force that.
- A failed connection retries with exponential backoff (`poll_interval`
  doubling up to 5 minutes), so a server that is throttling us is not hammered
  into extending the block.

The dashboard health indicator follows whichever mode is active; hover it to
see which one.

## Web dashboard

Open `http://host:8080`. Set `web.enabled: false` to turn it off entirely.

### Authentication

**Google sign-in (recommended):** set `web.google_auth.client_id` /
`client_secret` and list who may log in under `allowed_domains` (email
domains, or single addresses containing an `@`). Everything — pages and API —
is then behind a login screen with one "Inloggen met Google" button. Sessions
last 30 days and survive restarts.

Setup in the Google Cloud Console (APIs & Services → Credentials):
1. Create an OAuth client ID of type "Web application".
2. Add `<public_url>/auth/callback` as authorized redirect URI — exactly as
   configured in `web.public_url` (which is required for this feature).
3. Put the client ID and secret in `.env` (`GOOGLE_CLIENT_ID` /
   `GOOGLE_CLIENT_SECRET`); the config references them via `${...}`.

With an "External" OAuth consent screen, also add the team's Google accounts
as test users, or publish the app — `allowed_domains` is what actually gates
access on our side either way.

**Basic auth (fallback):** when `google_auth` is not configured, setting
`web.password` protects the dashboard with basic auth (username `admin`).
Leave both empty and the dashboard is open.
