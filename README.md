Building on Windows:

MSYS2 UCRT64

pacman -S mingw-w64-ucrt-x86_64-go

echo 'export GOROOT=/ucrt64/lib/go' >> ~/.bash_profile
echo 'export PATH="/ucrt64/bin:$PATH"' >> ~/.bash_profile
source ~/.bash_profile

Then finally ./build.sh

## ICE configuration

The lobby WebSocket relays ICE signaling only; game traffic remains directly
peer-to-peer. By default the server advertises `216.128.157.98:3478` and no
TURN relay:

```text
lobby-server --stun-host 216.128.157.98 --stun-port 3478
```

TURN support is reserved for a future deployment and is disabled unless
`--turn-host` is set. The related options are `--turn-port`,
`--turn-username`, and `--turn-password`. Do not set them for a direct-only
deployment.

The existing UDP anchor options remain available for older clients, but
ICE-capable rollback-lobby clients do not use that anchor.


## Accounts and ranked play

Accounts, ratings and ranked set history are stored in an embedded SQLite
database. The rest of the lobby stays in memory.

| Environment variable | Default | Purpose |
|---|---|---|
| `RMGK_DB_PATH` | `rmgk.db` | SQLite database: accounts, sessions, ratings, set history. Back it up. |
| `RMGK_RANKED_PATH` | `ranked.json` | Ranked ladders, allowed ROMs and tier bands. See `ranked.example.json`. Without it, accounts work but ranked rooms are unavailable. |
| `RMGK_TRUSTED_PROXIES` | (none) | Comma-separated reverse proxy IPs or CIDRs, e.g. `127.0.0.1`. Requests from them take the client address from `X-Forwarded-For`, for bans and rate limits. Leave it unset when clients connect directly. |
| `RMGK_REPLAY_DIR` | `replays` | Uploaded ranked set replays (`<setId>.krec`, a few hundred KB each). One upload per set; never overwritten. Back it up with the database. |

Both paths are relative to the service's working directory, so set absolute
paths in the systemd unit (for example `/opt/rmgk-lobby/rmgk.db`).

### Email (optional)

With an SMTP account configured, players can confirm an email address and
email themselves a password reset code. Without one, resets go through a
moderator's `/pwreset` code and RMG-K hides the email options.

| Environment variable | Default | Purpose |
|---|---|---|
| `RMGK_SMTP_HOST` | (none) | SMTP server, e.g. `smtp.example.com`. Setting it turns email on. |
| `RMGK_SMTP_PORT` | 587, or 465 for `tls`, 25 for `none` | SMTP port. |
| `RMGK_SMTP_SECURITY` | `starttls` (`tls` when the port is 465) | `starttls`, `tls` (TLS from the first byte), or `none` for a relay on the same machine only. |
| `RMGK_SMTP_USERNAME`, `RMGK_SMTP_PASSWORD` | (none) | SMTP login. Set both or neither. |
| `RMGK_SMTP_FROM` | (required) | Sender, e.g. `RMG-K Lobby <noreply@example.com>`. |

The lobby refuses to start when the settings are incomplete, and logs
`email enabled` when they're in use. Each email opens its own connection, so a
wrong password shows up as `confirmation email failed` or
`password reset email failed` in the log when a player asks for a code.

**Passwords travel in `HELLO`, so serve the lobby over TLS** (`wss://`) before
enabling accounts. Put Caddy or nginx in front of `:8080`.

### How it works

- **Ratings:** each roster (ruleset) of a ladder is a separate ranking with its own Glicko-2 rating, so a Remix-roster set doesn't move a player's Vanilla-roster rating. A room's skill filter, the standings it shows and the set's rating update all use the room's roster. Migration 5 split the ratings table this way and rebuilt every rating from the set history.
- **2v2 team ladders** (`"teamSize": 2` in `ranked.json`, e.g. `smashremix-2v2` in the example) are rated with OpenSkill (Plackett-Luce, the reference libraries' default parameters, `internal/rating/openskill.go`), one rating per player on the same 1500 ±350 scale. Every game of a set is rated, in order. A ROM can have one ladder per team size; a room picks one with `ranked.ladderId` (default: the 1v1 ladder).
  - Placement and tiers are per ladder: `placementDeviation` and `tierDeviations` (players stay in placement while their ± is above the first; tiers use rating − tierDeviations × ±). Defaults are 110 and 2 for 1v1, 175 and 1 for 2v2, because a team result says less about each player and 2v2 ratings stay less certain.

- **Login:** `HELLO` takes an optional `auth`. It can be `{"mode":"register"|"login","password":...}` or `{"mode":"token","token":...}`.
  - `HELLO_OK` then returns the canonical `username`, `account.sessionToken` (a 30-day remember-me token) and the player's standings, one per roster of each ladder.
  - Guests can't use a registered name.
  - An account logging in replaces its own older session, or a guest holding its name.
  - Failed logins are rate limited per IP.
  - `ACCOUNT_CHANGE_PASSWORD {currentPassword, newPassword}` changes the password, revokes every saved login and answers with `ACCOUNT_PASSWORD_CHANGED {sessionToken}` or `ACCOUNT_PASSWORD_FAIL`.
  - Forgotten passwords: a moderator runs `/pwreset <user>` and passes on the one-time code (valid 24 hours). The player logs in with `{"mode":"reset","resetCode":...,"password":<new>}`.
  - With email configured, a player can get the code by email instead:
    - `GET /api/auth/email` answers 200 (404 without email), so RMG-K knows to offer it.
    - `POST /api/auth/password-reset` with JSON `{"username":...,"email":...}` emails a reset code (valid 1 hour) to the account's confirmed address.
      - The request must name that address (case doesn't matter), so knowing a player's username isn't enough to send reset emails to their inbox.
      - Every request gets the same 202 answer, whether or not the account exists or the address matches.
    - The limits are 5 requests per IP per 15 minutes (then 429 with `Retry-After`) and 3 emails per account per hour.
    - Only JSON is accepted, so other websites can't make a visitor's browser send it.
    - A new code, emailed or from a moderator, replaces the previous one.
  - Account email, only with email configured. `HELLO_OK.account.email` is `{address}` (`""` when the account has none) and is absent without email.
    - `ACCOUNT_SET_EMAIL {email}` emails a confirmation code to the new address and answers `ACCOUNT_EMAIL_CODE_SENT {address}` once the mail server accepted it. Up to 5 per account per hour.
    - `ACCOUNT_CONFIRM_EMAIL {code}` makes it the account's address and answers `ACCOUNT_EMAIL {address}`. The old address stays until then.
    - `ACCOUNT_REMOVE_EMAIL` forgets it (`ACCOUNT_EMAIL {address:""}`).
    - Failures are `ACCOUNT_EMAIL_FAIL {reason, message}`, with reasons `invalid_email`, `email_failed`, `email_unavailable`, `auth_failed` (wrong code) or `too_many_attempts`.
- **Ranked rooms:** set with `ROOM_CREATE.ranked` or `ROOM_UPDATE_RANKED`, which is host-only and only while the room is waiting.
  - Ranked rooms seat 2 (1v1) or 4 (2v2) verified players and start only when full.
  - They're limited to ROMs on a ladder, plus the room's optional tier gap.
- **Match start:** `MATCH_BEGIN.ranked` carries the ladder, module, ruleset, set length and team size.
- **Results:** when the set ends, each player sends `MATCH_RESULT`.
  - Agreeing reports with a valid winner are rated: Glicko-2 for 1v1, counting the whole set as one result; OpenSkill for 2v2, every game.
  - 2v2 reports also carry `teams` (each slot's team: 1 for the team holding the lowest slot, 2 for the other), `winnerTeam`, and each game's `winnerTeam`. All four must agree, including the pairing, with two players per team; otherwise the set is disputed (`reports_disagree` or `invalid_teams`).
  - Disagreeing or desynced reports are recorded as `disputed`.
  - A player leaving before reporting, or a missing report 60 seconds after the first, makes the set `voided`. Leave penalties are still to be designed.
  - Players get the outcome and rating change in `RANKED_RESULT`.
  - Moderators review disputes from the lobby chat. `/disputes` lists them with both players' reports; `/resolve <setId> <winner|void> [note]` rates the set from the players' current ratings or voids it (for a 2v2 set, naming any player of the winning team), and online players get a `RANKED_RESULT`.
- **Spectators:** for ranked matches the broadcaster also sends `BROADCAST_JOURNAL` chunks (the game module's memory write journal). Every spectator gets all of them, in order and ahead of the krec data, as `SPECTATE_JOURNAL`.
- **Tiers:** based on the conservative rating (rating minus twice the deviation). Players stay provisional until their deviation drops below 110.

### Rankings page

The lobby serves a public rankings page at `/` (also `/rankings`). It's one self-contained HTML file embedded in the binary, about 10 KB gzipped, with no external assets. It shows rankings, recent sets, player profiles, rulesets, tiers, how scoring works (with a rating calculator) and how to play.

Its read-only JSON API:

| Endpoint | Returns |
|---|---|
| `GET /api/ranked/overview` | ladders, rulesets (with the characters played on each), rules, tiers, Glicko-2 constants (for the Scoring page) |
| `GET /api/ranked/leaderboard?ladder=<id>&ruleset=<roster id>` | one roster's ranked players, then players still in placement (each roster of a ladder is rated separately; the default roster when `ruleset` is left out) |
| `GET /api/ranked/player?name=<username>` | standings and rating history per roster, and how many sets they played each character in |
| `GET /api/ranked/sets?…` | sets, newest first, with the total that match. Filters: `ladder` and `ruleset` (a roster), `player`, `opponent` (with `player`), `character` (an id; played by `player` when given), `result=win\|loss` (with `player`). Paging: `offset`, `limit` (≤ 200). Each set says `hasReplay` and, when there is one, `replayUrl`. |
| `GET /api/ranked/sets/<id>` | one set, with replay size, module version and bookmarks when a replay exists |
| `GET /api/ranked/sets/<id>/replay` | the set's `.krec` (immutable, cached forever) |

To try the site with a realistic amount of data, `go run ./cmd/ranked-seed --db seed.db --ranked ranked.json --players 600 --days 150` fills a new database with simulated players and sets (password `password123` for every account). Run the lobby with `RMGK_DB_PATH=seed.db` to browse it.

The Rulesets page text comes from `ranked.json`: each ladder's `rules`, `rulesetNames`, `rulesetDescriptions` and `rulesetStages` (see `ranked.example.json`). `rulesetStages` lists each roster's stages; the game module enforces them, so keep it in sync with the module.

### Moderator pages

`/#/admin` (linked as "Moderators" in the page footer) lets the moderators from `admins.json` sign in with the same password as the lobby's `/login`. It has three pages, each loading only its own data: Accounts (`#/admin/accounts`, the default), Disputes (`#/admin/disputes`; its tab shows how many sets wait, from `GET /api/admin/session`) and Activity (`#/admin/activity`). From there they can:

- **Disputed sets:** see both players' reports, then rate the set for either player or void it. A note is optional. Online players get the usual `RANKED_RESULT`.
- **Accounts:** search accounts, see their standings and sets, and issue a one-time password reset code (valid 24 hours). An account can also be disabled, which signs it out everywhere, disconnects it from the lobby, blocks logins and hides it from the rankings. It can be re-enabled later.
- **Activity:** see what every moderator did, from the website or from lobby chat (`/resolve`, `/pwreset`).

Security:

- Admin sessions are kept in memory (12 hours), so a restart signs everyone out.
- The session cookie is HttpOnly, `SameSite=Strict` and scoped to `/api/admin`. It is `Secure` when the request is HTTPS (directly, or `X-Forwarded-Proto: https` from a proxy).
- Every change also requires the `X-RMGK-Admin: 1` header, which a cross-site page can't send.
- Failed sign-ins are limited to 10 per 15 minutes per IP. Behind a reverse proxy, set `RMGK_TRUSTED_PROXIES` so this is per client.
- Serve the site over HTTPS before using the moderator pages in production.
