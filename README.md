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


## Runtime settings (lobby.json)

Settings that may need changing between releases live in `lobby.json` in the
server's working directory (set `RMGK_CONFIG_PATH` to use another path). The
server checks the file every 2 seconds and applies edits without a restart. A
missing file or key uses the default:

```json
{
  "minClientVersion": "0.9.13b",
  "outdatedClientMessage": "",
  "liveReplayKeyframeSeconds": 30,
  "quickMatchWarmupSeconds": 5,
  "connectAttempts": 5,
  "connectWindowSeconds": 60,
  "connectBanSeconds": 300,
  "maxConnectionsPerIP": 4,
  "roomCreateLimit": 5,
  "roomCreateWindowSeconds": 60,
  "roomJoinLimit": 15,
  "roomJoinWindowSeconds": 60,
  "matchStartLimit": 10,
  "matchStartWindowSeconds": 60,
  "spectateLimit": 10,
  "spectateWindowSeconds": 60,
  "roomUpdateLimit": 40,
  "roomUpdateWindowSeconds": 10,
  "lobbyUpdateLimit": 30,
  "lobbyUpdateWindowSeconds": 60,
  "abuseBanStrikes": 40,
  "abuseBanWindowSeconds": 600,
  "abuseBanSeconds": 3600,
  "abuseReportDays": 7,
  "roomListMinIntervalMs": 250,
  "chatLogDays": 90,
  "connectionLogDays": 90,
  "gameLogDays": 365,
  "modLogMaxMB": 1024,
  "adminAllowedIPs": [],
  "adminLoginMaxFailures": 5,
  "adminLoginBanSeconds": 0
}
```

| Key | Effect |
| --- | --- |
| `minClientVersion` | Oldest RMG-K release that may log in. `vdev-` builds are always allowed. Checked at login, so raising it doesn't disconnect anyone already online. |
| `outdatedClientMessage` | What rejected clients are told. Empty names the required version. |
| `liveReplayKeyframeSeconds` | 10-600. How often a live replay broadcaster uploads a savestate keyframe; late viewers start from the latest one. Shorter means less catch-up for them and more upload for the broadcaster. Sent in `MATCH_BEGIN`, so it applies from the next match. RMG-K builds that don't read it use 30. |
| `quickMatchWarmupSeconds` | 2-60. Pause between a quick match pairing and its start, for both clients to measure ping. |
| `connectAttempts`, `connectWindowSeconds`, `connectBanSeconds` | One IP connecting more than `connectAttempts` times within `connectWindowSeconds` is refused for `connectBanSeconds`. Loopback is exempt. |
| `maxConnectionsPerIP` | Connections one IP may hold at once (4 lets a full room share a home NAT). |
| `roomCreateLimit` ... `lobbyUpdateWindowSeconds` | 0-1000 requests per 1-3600 seconds. Per-IP limits on the requests that reach every player; 0 lifts one. See [Rate limits](#rate-limits). |
| `abuseBanStrikes`, `abuseBanWindowSeconds`, `abuseBanSeconds` | 0-1000 refused requests within 1-86400 seconds time the IP out for 1 second to 30 days; 0 strikes turns that off. See [Rate limits](#rate-limits). |
| `abuseReportDays` | 1-365. Days the moderators' Rate limits report keeps an IP after its last refused request. |
| `roomListMinIntervalMs` | 0-5000. The full room list goes to every player at most once per this many milliseconds; 0 sends one for every change. |
| `chatLogDays` | 0-3650. Days the moderator log keeps chat (blocked lines too); 0 keeps it forever. Pruned every few hours. |
| `connectionLogDays` | 0-3650. Days it keeps connections, which hold IP addresses; 0 keeps them forever. |
| `gameLogDays` | 0-3650. Days it keeps games and room events; 0 keeps them forever. The moderators' audit log is never pruned. |
| `modLogMaxMB` | 0-1048576. Most data the moderator log may hold, in megabytes; past it the oldest chat and connections are pruned first (games and room events only after those), never the audit log. 0: no cap, only the days above. Checked every 10 minutes. |
| `adminAllowedIPs` | IPs and CIDR ranges (`"203.0.113.7"`, `"198.51.100.0/24"`, `"2001:db8::/48"`) that may reach the moderator site at all. Empty: no limit beyond where it listens. Loopback is always allowed. Anyone else's connection is reset before any HTTP. Not a substitute for a host firewall. |
| `adminLoginMaxFailures` | 1-1000. Wrong moderator passwords in a row from one IP (site and lobby `/login` together) before it is banned. |
| `adminLoginBanSeconds` | 0-315360000. How long that ban lasts; 0 is for good. Loopback is never banned: it is locked out of moderator logins for 15 minutes instead. |

If an edit doesn't parse or a value is out of range, the server logs an error
and keeps the previous settings; at startup an invalid file stops the server.
Each applied change is logged as `lobby config reloaded` with the old and new
values. Unknown keys, usually typos, are logged as a warning and ignored.

`admins.json` (`RMGK_ADMINS_PATH`) and the blocklist override
(`RMGK_BLOCKLIST_PATH`) are re-read the same way, so moderators and blocked
terms can be added without a restart.


## Rate limits

Some lobby requests reach every connected player. A new room, a join or a
leave, a started or dropped game, and a live replay starting or ending each
send everyone the full room list (about 300 bytes per room) and usually a
presence update: with 150 players and 30 rooms, one client creating and
leaving a room costs the server about 3 MB of sending. So the server limits
these requests per IP address (an IP's connections share one budget, and
reconnecting doesn't reset it), and sends the room list at most four times a
second however many players cause changes.

### What is limited

| Budget | Requests | Default per IP |
| --- | --- | --- |
| `roomCreate` | Create Room | 5 per minute |
| `roomJoin` | Joining a room (every attempt, wrong passwords too), starting a Quick Match search | 15 per minute |
| `matchStart` | Start, and adding a player to a running game | 10 per minute |
| `spectate` | Opening a live replay (each sends the viewer megabytes) | 10 per minute |
| `roomUpdate` | Delay, prediction, live replay, late join and save data settings, seat swaps (seen only by the room) | 40 per 10 seconds |
| `lobbyUpdate` | Going away and coming back, removing a player from a room, asking for the chat history | 30 per minute |

A match's live replay can start once (clients only ever ask once, when the
match begins).

Never limited, so nobody can get stuck: leaving a room, dropping out of a
game, cancelling a Quick Match search, closing a live replay, ending a
broadcast, disconnecting, the late-join hand-off and ICE messages, live
replay uploads, chat (it has its own flood limit) and ping probes. Signed-in
moderators aren't limited, and neither are loopback addresses, so local
multi-client testing works. A build made with `go build -tags
ratelimit_loopback` limits loopback addresses too (and bans them): use it
only on a local test rig.

### What a player sees

Every reply is one that RMG-K releases since 0.9.13 already show, with the
wait in seconds:

| Request | What they see |
| --- | --- |
| Create Room | "Couldn't create room": "You're creating rooms too fast. Try again in 42 seconds." On 0.9.13-0.9.16 the Create Room window then stays on "Creating room..." until they press Cancel, as it does for any refused room. |
| Joining a room | "Couldn't join room": "You're joining rooms too fast. Try again in 20 seconds." |
| Start, a Quick Match search, a room setting, removing a player | A line in the room and lobby chat. A refused setting snaps back to the room's. |
| Watching a live replay | "Couldn't watch: You're opening live replays too fast. Try again in 40 seconds." |
| Adding a player to a running game | "Couldn't add *name*: something went wrong (you're adding players too fast; try again in 30 seconds)." |
| Going away or coming back | Nothing; the change shows up a heartbeat later. |

### The automatic timeout

Every refused request is a strike. `abuseBanStrikes` (40) strikes within
`abuseBanWindowSeconds` (10 minutes) time the IP out for `abuseBanSeconds`
(1 hour): a ban by **automatic** with a reason starting "Rate limit:", saved
in `bans.json` and listed on the Sanctions page. Every connection from the
IP is closed with a message saying until when it's blocked (a signed-in
moderator's excepted), and the audit log gets an entry. Like any ban, it
keeps out everyone behind that address. Lift it early with Unban on the
Sanctions or Rate limits page, or `/unban <ip>` in the lobby; the next run of
strikes can time it out again. `abuseBanStrikes: 0` turns the automatic
timeout off (the limits still apply).

### The Rate limits report

The moderator site's **Rate limits** tab shows the limits in effect and every
IP that had a request refused in the last `abuseReportDays` (7) days: when it
was first and last refused, how many requests of each kind, the names it
used, who is on it now, its automatic timeouts and its current ban or mute.
Ban, time out or unban it there, or Forget its record (which also resets its
budgets). The records are saved in `modlog.db` (their changes at most every
30 seconds, and at shutdown) and loaded at startup; with the log off they are
kept in memory only. At most 5,000 IPs are tracked; the least recently
refused go first. Refusals reach the moderator log only through this report,
not one row each. The server log gets the first refusal for an IP and kind,
then at most one summary line a minute.

### Room list coalescing

`roomListMinIntervalMs` (250): the first change sends the room list at once;
changes in the next 250 ms go out together, once, when the interval ends.
Clients replace their whole room list with each one, so skipping the ones in
between loses nothing. At most four lists a second go out however many IPs
take part. 0 sends a list for every change, as before.

### Tuning

- The defaults are well above what a player does. If players do hit one (say,
  several people on one home connection creating and joining a lot), raise
  that kind's `*Limit` or shorten its `*WindowSeconds`. 0 lifts one kind's
  limit.
- Under an attack from many addresses, lower `roomCreateLimit` and
  `roomJoinLimit` and raise `roomListMinIntervalMs` (500-1000 still feels
  live in the room list).
- Edits apply within 2 seconds, without a restart; requests already counted
  keep counting under the new limits.

### Not covered yet

- IPv6: each address has its own budget, so one host with a /64 can rotate
  addresses. Keying IPv6 by /64 (here, in the connect limits and in bans)
  is still to do.
- Live replays have per-broadcast caps (64 MiB spool on disk, 32 MiB keyframe
  in memory) but no cap across all broadcasts, and each viewer joining
  re-reads the whole spool to find the keyframe's place.


## Moderator site

The server has a website for moderators: **Live** (who's online, rooms and
the lobby chat as it happens, with kick, mute, timeout, ban and close room),
**Chat** (the chat log, including lines the filter, a mute or the flood
limit stopped), **People** (every IP a name used and every name seen on an
IP, to find alts), **Games** (every match with its players, late joiners,
leavers and timeline), **Sanctions** (bans and mutes; ban or mute any IP),
**Rate limits** (the limits in effect and the IPs they refused; see
[Rate limits](#rate-limits)) and **Audit log** (every moderator action, from
the site or the lobby, and the server's automatic bans).

Moderators sign in with the same password as the lobby's `/login`: an entry
in `admins.json`, made with `cmd/modhash`. Use a long random password:
`go run ./cmd/modhash <name>` makes one up (24 characters, printed once to
the terminal; give it to the moderator privately) and prints the entry to
paste into the `admins.json` array. `go run ./cmd/modhash <name> <password>`
uses one you chose.

### Reaching it

The site has its own listener, apart from the players' `/ws` port:

| Setting | Default | Effect |
| --- | --- | --- |
| `--admin-addr` | `127.0.0.1:8081` | Where the site listens. `off` (or empty) turns it off. |
| `RMGK_ADMIN_ADDR` | unset | The same, for a systemd unit; `--admin-addr` wins when both are given. |

There is no TLS certificate, so the site is plain HTTP, and **a password
typed into it crosses the network in the clear** unless something else
encrypts it. The same is true of the lobby's own `/login`: it is the same
credential, sent over `ws://` in clear text, so anyone who can watch the
moderator's traffic (a shared Wi-Fi, a LAN party, an ISP) can read it there
too, whatever the site does. So:

- give every moderator a long random password (`cmd/modhash`, above), never
  one used anywhere else, and change it if it may have been seen;
- reach the site through an SSH tunnel; and
- once the server has a domain name, put both the lobby and the site behind
  TLS (`wss://` and `https://`).

The two setups:

1. **SSH tunnel (the default, recommended).** Leave `--admin-addr` alone. On
   your own computer run `ssh -L 8081:127.0.0.1:8081 user@server` and open
   <http://127.0.0.1:8081/>. Nothing is exposed; SSH encrypts everything.
2. **An open port for known addresses.** Start the server with
   `--admin-addr :8081`, list the moderators' IPs in lobby.json
   `adminAllowedIPs`, and allow only those in the host firewall, for example
   `ufw allow from 203.0.113.7 to any port 8081 proto tcp`. The firewall is
   the real protection; `adminAllowedIPs` is a second line behind it. The
   password still travels unencrypted, so prefer the tunnel where you can.

**Nothing may proxy `/ws` from loopback.** The lobby takes every player's
address from the TCP connection; behind a reverse proxy on the same machine
every player would arrive from 127.0.0.1, which the connect limits, request
limits and login bans all leave alone. The lobby must take players' connections
itself.

At startup the server logs where the site listens, or that it's off. It warns
loudly when the site listens beyond loopback with `adminAllowedIPs` empty
(and again whenever lobby.json is reloaded that way).

### How it's protected

- The client address is the TCP peer address only; no `X-Forwarded-For` or
  similar header is believed (there is no proxy).
- Addresses outside a non-empty `adminAllowedIPs`, and banned addresses, get
  nothing: the listener resets their connection as soon as it accepts it,
  before reading any HTTP, the way a closed port's connection ends. So they
  can't hold the site's connections either. One address may hold at most 8
  of the site's 64 connections (loopback, which every tunnel user shares, 32),
  every signed-out response closes its connection, and a connection that
  sends nothing is dropped after 5 seconds. Loopback is always let through.
  None of this replaces a host firewall.
- The site answers only to an IP address or `localhost` in the address bar,
  so a web page can't point its own domain at a moderator's tunnel (DNS
  rebinding).
- Signed out, only the sign-in page (`GET /`) and the sign-in request answer;
  everything else is a bare 404, Go's own (none of the site's headers), as on
  any Go server. The sign-in page names no server, version, count or
  moderator, and every failed sign-in gets the same "Sign-in failed." answer.
- **5 wrong passwords in a row from one IP ban that IP for good**, whether
  they're typed into the site or the desktop client's `/login` (one count for
  both; a correct password resets it). `adminLoginMaxFailures` and
  `adminLoginBanSeconds` in lobby.json change the count and make the ban a
  timeout, without a restart. The ban closes the IP's lobby connections and
  shows on the Sanctions page and in the Audit log. Loopback addresses are
  never banned (everyone using the tunnel arrives from 127.0.0.1); after the
  same number of failures from loopback, sign-ins from that address are
  refused for 15 minutes instead. Password checks run one at a time, so
  parallel requests can't sneak in extra guesses.
- A web page can open a WebSocket to the lobby from any visitor's browser.
  The browser marks such a connection with an `Origin` header (RMG-K's desktop
  client never sends one), and on such a connection `/login` is refused with
  the usual wrong-password answer, unchecked and uncounted: a page can neither
  guess passwords through its visitors nor get their addresses banned.
- A lobby moderator whose `admins.json` entry is removed, or whose password
  changes, loses the lobby's moderator powers at their next action (and the
  site can then kick or ban them like anyone else).
- The site won't ban (or time out) the address the moderator is signed in
  from, and asks before banning an address another signed-in moderator uses.
- A session is a random 256-bit token in an `HttpOnly`, `SameSite=Strict`
  cookie (not `Secure`: there's no HTTPS for it to need). The server keeps
  only its hash, in memory. It works only from the IP that signed in and ends
  after 12 hours, after 60 minutes without the moderator doing anything (the
  pages' own refreshes don't count), at sign-out, when that moderator's
  `admins.json` entry is removed or its password changes, and when the server
  restarts. At most 32 sessions exist at once.
- Every API call needs the `X-RMGK-Admin: 1` header; changes are POSTs with
  a JSON body, and a request whose `Origin` or `Referer` names another host is
  refused. Another site can't make a signed-in browser do anything.
- The sign-in page and every signed-in response carry `Cache-Control: no-store`,
  `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `X-Robots-Tag: noindex, nofollow` and a `Content-Security-Policy` that
  allows only the page's own inline script and style (by hash) and requests
  back to the site. Nothing is loaded from anywhere else. Player names, chat
  and room names reach the page only as text, never as markup.
- Request bodies are capped at 16 KiB, query parameters are checked, every
  list (the Sanctions lists too) is paged (at most 500 rows), log searches
  stop after 10 seconds, and the HTTP server has read, write and idle
  timeouts.

Open items, known and not yet addressed:

- Login failures (and the request limits) are counted per address. Someone
  with many addresses, such as an IPv6 /64 or a large proxy pool, gets
  `adminLoginMaxFailures` guesses from each. Long random passwords are what
  keep that from mattering.
- `admins.json` stores sha256(salt + password), a fast hash: a stolen copy
  of the file can be guessed at quickly offline, so keep it readable by the
  server's user only, and passwords long and random.

### Unbanning an address that locked itself out

A moderator who mistyped the password 5 times has banned their own IP: the
site gives them nothing and the lobby refuses them. To lift it:

- Sign in through the SSH tunnel (loopback is never banned) and press Unban
  on the Sanctions page, or
- have another moderator do it there, or type `/unban <ip>` in the lobby, or
- stop the server, delete the entry from `bans.json`, and start it again
  (bans are read only at startup, and the next ban or unban rewrites the
  file, so an edit made while it runs can be lost).

To make these bans lift by themselves, set `adminLoginBanSeconds` (say 3600).

The ban list is written in the background, off the lock every connection
takes, and a burst of bans is folded into a few writes; it is written once
more at shutdown. A write that fails is logged as an error (the bans stay in
force until the server restarts).

### The moderator log

Connections (with IPs), chat, games and room events, and the audit log, go
into a SQLite file of their own, written in the background so the lobby never
waits on the disk.

| Setting | Default | Effect |
| --- | --- | --- |
| `RMGK_MODLOG_DB_PATH` | `modlog.db` | The file (with `-wal` and `-shm` beside it). `off` turns logging off; the site's log pages then say so, and live moderation still works. |

The file holds IP addresses and chat, so the server creates it readable by
its owner only (mode 0600) and tightens an existing one. Rows older than
`chatLogDays`, `connectionLogDays` and `gameLogDays` are pruned; the audit
log is kept. After a restart, new room and match ids continue past the
highest ones in the log, so an id never names two different rooms or games.

Its size is bounded too:

- Chat the lobby stopped (the filter, a mute, the flood limit) is sampled per
  address: the first 3 such lines a minute are logged, the rest counted; the
  next line logged from that address (or, when the minute ends, the last one
  held back) carries the count, shown on the Chat page as "+N more lines not
  logged". A muted spammer costs a few rows a minute, not every line.
- `modLogMaxMB` (default 1024) caps the data. Over it, the oldest chat and
  connections are pruned (a warning is logged each time) until it is 10% under;
  games and room events only after those are gone; the audit log and the
  Rate limits report never.
- A file created by this version gives pruned space back to the disk
  (incremental auto-vacuum). An older file reuses the freed space for new rows
  instead, so it stops growing; `sqlite3 modlog.db VACUUM` with the server
  stopped shrinks it once.

### Files

| File | Variable | What it is |
| --- | --- | --- |
| `admins.json` | `RMGK_ADMINS_PATH` | Moderator names and salted password hashes (`cmd/modhash`). Re-read when it changes. |
| `bans.json` | `RMGK_BANLIST_PATH` | Bans and timeouts, including the automatic ones. Read at startup, written on every change. |
| `modlog.db` | `RMGK_MODLOG_DB_PATH` | The moderator log, audit log and Rate limits report. |
| `lobby.json` | `RMGK_CONFIG_PATH` | Runtime settings, including the log retention and size cap, `adminAllowedIPs` and the login ban. |

### Trying it locally

```text
go run ./cmd/modhash Wren rig-mod-password        # a test password; paste into admins.json as [ {...} ]
go run ./cmd/modlog-seed --db modlog.db --bans bans.json
go run ./cmd/lobby-server --ws-addr 127.0.0.1:18480 --udp-addr 127.0.0.1:18482 --admin-addr 127.0.0.1:18481
go run ./cmd/lobby-sim --url ws://127.0.0.1:18480/ws --flood
```

Then open <http://127.0.0.1:18481/>. `modlog-seed` writes a few made-up days
of history (documentation-range addresses only); `lobby-sim` keeps fake
players online, each from its own loopback address (127.0.0.10 and up), with
two rooms, a broadcast match with a spectator, and chat.
