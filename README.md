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
  "maxConnectionsPerIP": 4
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

If an edit doesn't parse or a value is out of range, the server logs an error
and keeps the previous settings; at startup an invalid file stops the server.
Each applied change is logged as `lobby config reloaded` with the old and new
values. Unknown keys, usually typos, are logged as a warning and ignored.

`admins.json` (`RMGK_ADMINS_PATH`) and the blocklist override
(`RMGK_BLOCKLIST_PATH`) are re-read the same way, so moderators and blocked
terms can be added without a restart.
