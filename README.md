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

