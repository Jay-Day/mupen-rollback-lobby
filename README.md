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
  "liveReplayKeyframeOnDemand": false,
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
  "adminLoginMaxFailures": 5,
  "adminLoginBanSeconds": 0,
  "blockAWS": true
}
```

| Key | Effect |
| --- | --- |
| `minClientVersion` | Oldest RMG-K release that may log in. `vdev-` builds are always allowed. Checked at login, so raising it doesn't disconnect anyone already online. |
| `outdatedClientMessage` | What rejected clients are told. Empty names the required version. |
| `liveReplayKeyframeSeconds` | 10-600. How often a live replay broadcaster uploads a savestate keyframe; late viewers start from the latest one. Shorter means less catch-up for them and more upload for the broadcaster. Sent in `MATCH_BEGIN`, so it applies from the next match. RMG-K builds that don't read it use 30. |
| `liveReplayKeyframeOnDemand` | `true` takes keyframes when viewers join instead of every `liveReplayKeyframeSeconds`. A viewer who joins when the cached keyframe is older than `liveReplayKeyframeSeconds` waits (up to 10 s) while the broadcaster takes a new one; everyone joining within the next `liveReplayKeyframeSeconds` gets that same keyframe. During a match's first `liveReplayKeyframeSeconds`, viewers replay from the start instead, which is no longer. Sent in `MATCH_BEGIN`, so turning it on or off applies from the next match; the age limit is read at each join. Broadcasters on RMG-K builds without it keep uploading on the timer. |
| `quickMatchWarmupSeconds` | 2-60. Pause between a quick match pairing and its start, for both clients to measure ping. |
| `connectAttempts`, `connectWindowSeconds`, `connectBanSeconds` | One IP connecting more than `connectAttempts` times within `connectWindowSeconds` is refused for `connectBanSeconds`. Loopback is exempt. |
| `maxConnectionsPerIP` | Connections one IP may hold at once (4 lets a full room share a home NAT). |
| `roomCreateLimit` ... `lobbyUpdateWindowSeconds` | 0-1000 requests per 1-3600 seconds. Per-IP limits on the requests that reach every player; 0 lifts one. See [Rate limits](#rate-limits). |
| `abuseBanStrikes`, `abuseBanWindowSeconds`, `abuseBanSeconds` | 0-1000 refused requests within 1-86400 seconds time the IP out for 1 second to 30 days; 0 strikes turns that off. See [Rate limits](#rate-limits). |
| `abuseReportDays` | 1-365. Days the moderators' Rate limits report keeps an IP after its last refused request. |
| `roomListMinIntervalMs` | 0-5000. The full room list goes to every player at most once per this many milliseconds; 0 sends one for every change. |
| `adminLoginMaxFailures` | 1-1000. Wrong moderator passwords in a row from one IP (the lobby's `/login`) before it is banned. |
| `adminLoginBanSeconds` | 0-315360000. How long that ban lasts; 0 is for good. Loopback is never banned: it is locked out of moderator logins for 15 minutes instead. |
| `blockAWS` | Refuse connections from AWS address space. The server downloads Amazon's `ip-ranges.json` at startup and every 12 hours; nothing is blocked until the first download succeeds. |

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
in `bans.json`. Every connection from the
IP is closed with a message saying until when it's blocked (a signed-in
moderator's excepted), and a `moderator action` line goes to the server log.
Like any ban, it keeps out everyone behind that address. Lift it early with
`/unban <ip>` in the lobby; the next run of strikes can time it out again. `abuseBanStrikes: 0` turns the automatic
timeout off (the limits still apply).

### The Rate limits report

Every IP that had a request refused in the last `abuseReportDays` (7) days
has a record: when it was first and last refused, how many requests of each
kind, the names it used and its automatic timeouts. The records are kept in
`ratelimits.json` beside `bans.json` (changes at most every 30 seconds, and
at shutdown) and loaded at startup, so `cat` it on the server to see who
has been hitting the limits. At most 5,000 IPs are tracked; the least
recently refused go first. The server log gets the first refusal for an IP
and kind, then at most one summary line a minute.

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


## Moderation

Moderators sign in from the desktop client with `/login`, against an entry
in `admins.json` made with `cmd/modhash`. Use a long random password:
`go run ./cmd/modhash <name>` makes one up (24 characters, printed once to
the terminal; give it to the moderator privately) and prints the entry to
paste into the `admins.json` array. `go run ./cmd/modhash <name> <password>`
uses one you chose.

**`/login` crosses the network in the clear**: the lobby runs on `ws://`, so
anyone who can watch a moderator's traffic (a shared Wi-Fi, a LAN party, an
ISP) can read the password. Give every moderator a long random password used
nowhere else, change it if it may have been seen, and move the lobby to
`wss://` once the server has a domain name.

**Nothing may proxy `/ws` from loopback.** The lobby takes every player's
address from the TCP connection; behind a reverse proxy on the same machine
every player would arrive from 127.0.0.1, which the connect limits, request
limits and login bans all leave alone.

### How logins are protected

- **5 wrong passwords in a row from one IP ban that IP for good** (a correct
  password resets the count). `adminLoginMaxFailures` and
  `adminLoginBanSeconds` in lobby.json change the count and make the ban a
  timeout, without a restart. The ban closes the IP's lobby connections.
  Loopback addresses are never banned; after the same number of failures
  from loopback, logins from that address are refused for 15 minutes
  instead. Password checks run one at a time, so parallel requests can't
  sneak in extra guesses.
- A web page can open a WebSocket to the lobby from any visitor's browser.
  The browser marks such a connection with an `Origin` header (RMG-K's desktop
  client never sends one), and on such a connection `/login` is refused with
  the usual wrong-password answer, unchecked and uncounted: a page can neither
  guess passwords through its visitors nor get their addresses banned.
- A moderator whose `admins.json` entry is removed, or whose password
  changes, loses the moderator powers at their next action.

Open items, known and not yet addressed:

- Login failures (and the request limits) are counted per address. Someone
  with many addresses, such as an IPv6 /64 or a large proxy pool, gets
  `adminLoginMaxFailures` guesses from each. Long random passwords are what
  keep that from mattering.
- `admins.json` stores sha256(salt + password), a fast hash: a stolen copy
  of the file can be guessed at quickly offline, so keep it readable by the
  server's user only, and passwords long and random.

### Unbanning an address that locked itself out

A moderator who mistyped the password 5 times has banned their own IP. To
lift it:

- have another moderator type `/unban <ip>` in the lobby, or
- stop the server, delete the entry from `bans.json`, and start it again
  (bans are read only at startup, and the next ban or unban rewrites the
  file, so an edit made while it runs can be lost).

To make these bans lift by themselves, set `adminLoginBanSeconds` (say 3600).

The ban list is written in the background, off the lock every connection
takes, and a burst of bans is folded into a few writes; it is written once
more at shutdown. A write that fails is logged as an error (the bans stay in
force until the server restarts).

### The moderator log

What moderators need to look back on goes to the server's own log (journald
on the servers) as one structured line each, so it is read with `journalctl`
and `grep`; how far back it goes is journald's retention.

| Line (`msg`) | What it records |
| --- | --- |
| `hello accepted`, `connection opened` | A connection: name, user id, IP, then client version and country |
| `chat` | Every lobby, room and spectator chat line, with the sender's name, id and IP; a line the filter, a mute or the flood limit stopped also has `blocked` |
| `lobby event` | Rooms created, joined, left, kicked from and closed, with names and IPs |
| `game event` | Games started (every player with their IP), late joins, players leaving, failed connections and how each game ended |
| `moderator action` | Every moderator action from the lobby, and the server's automatic bans |

For example, on a server running with `--log-json`:

```text
journalctl -u rmgk-lobby --since today | grep '"msg":"chat"' | grep '"ip":"203.0.113.5"'
journalctl -u rmgk-lobby --since today | grep '"msg":"moderator action"'
```

Chat the lobby stopped (the filter, a mute, the flood limit) is sampled per
address: the first 3 such lines a minute are logged, the rest counted; the
next line logged from that address (or, when the minute ends, the last one
held back) carries the count as `suppressed`. A muted spammer costs a few
lines a minute, not every line.

### Files

| File | Variable | What it is |
| --- | --- | --- |
| `admins.json` | `RMGK_ADMINS_PATH` | Moderator names and salted password hashes (`cmd/modhash`). Re-read when it changes. |
| `bans.json` | `RMGK_BANLIST_PATH` | Bans and timeouts, including the automatic ones. Read at startup, written on every change. |
| `ratelimits.json` | `RMGK_RATELIMITS_PATH` | The Rate limits report. Defaults to the folder `bans.json` is in; `off` keeps it in memory only. |
| `lobby.json` | `RMGK_CONFIG_PATH` | Runtime settings, including the request limits and the login ban. |

A systemd unit with `ProtectSystem=strict` must list the folder these are
written to in `ReadWritePaths=`, or every write fails.

### Trying it locally

```text
go run ./cmd/modhash Wren rig-mod-password        # a test password; paste into admins.json as [ {...} ]
go run ./cmd/lobby-server --ws-addr 127.0.0.1:18480 --udp-addr 127.0.0.1:18482
go run ./cmd/lobby-sim --url ws://127.0.0.1:18480/ws --flood
```

`lobby-sim` keeps fake players online, each from its own loopback address
(127.0.0.10 and up), with two rooms, a broadcast match with a spectator, and
chat.
