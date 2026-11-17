package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/config"
	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/modsite"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
	"github.com/jay-day/rmgk-lobby/internal/udp"
	"github.com/jay-day/rmgk-lobby/internal/ws"
)

// How often the server checks its config files (lobby.json, admins.json and
// the blocklist override) for edits.
const configPollInterval = 2 * time.Second

func main() {
	var (
		wsAddr        = flag.String("ws-addr", ":8080", "WebSocket listen address")
		udpAddr       = flag.String("udp-addr", ":6364", "UDP anchor listen address")
		udpPublicAddr = flag.String("udp-public-addr", "127.0.0.1:6364", "Public host:port of the UDP anchor advertised to clients")
		logJSON       = flag.Bool("log-json", false, "Emit logs as JSON")
		stunHost      = flag.String("stun-host", "216.128.157.98", "STUN host advertised for ICE")
		stunPort      = flag.Int("stun-port", 3478, "STUN port advertised for ICE")
		turnHost      = flag.String("turn-host", "", "Optional TURN host (disabled when empty)")
		turnPort      = flag.Int("turn-port", 3478, "Optional TURN port")
		turnUsername  = flag.String("turn-username", "", "Optional TURN username")
		turnPassword  = flag.String("turn-password", "", "Optional TURN password")
		adminAddr     = flag.String("admin-addr", defaultAdminAddr,
			"Moderator website listen address (\"off\" or empty disables it; also RMGK_ADMIN_ADDR). "+
				"Loopback by default: reach it through an SSH tunnel, or open it with e.g. :8081 plus adminAllowedIPs")
	)
	flag.Parse()
	adminListen := resolveAdminAddr(*adminAddr)

	var handler slog.Handler
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	slog.SetDefault(slog.New(handler))

	// Settings that can change while the server runs (see internal/config).
	configPath := envOr("RMGK_CONFIG_PATH", "lobby.json")
	settings, err := config.Open(configPath)
	if err != nil {
		slog.Error("lobby config load failed", "path", configPath, "err", err)
		os.Exit(1)
	}
	slog.Info("lobby config ready", "path", configPath,
		"min_client_version", settings.Get().MinClientVersion,
		"keyframe_seconds", settings.Get().LiveReplayKeyframeSeconds)

	gameState := state.New()
	gate := ratelimit.NewConnectGate()
	gate.SetLimits(func() ratelimit.Limits { return settings.Get().ConnectLimits() })

	// Moderation: hate-speech filter (embedded blocklist, optional file override),
	// admin credentials, and the persisted IP ban list. The blocklist and admins
	// files are re-read when they change.
	blocklistPath := os.Getenv("RMGK_BLOCKLIST_PATH")
	adminsPath := envOr("RMGK_ADMINS_PATH", "admins.json")
	admins, err := moderation.LoadAdmins(adminsPath)
	if err != nil {
		slog.Error("admins.json load failed", "err", err)
		os.Exit(1)
	}
	banPath := envOr("RMGK_BANLIST_PATH", "bans.json")
	mod := moderation.NewManager(moderation.NewFilter(loadBlocklist(blocklistPath)), admins, banPath)
	slog.Info("moderation ready", "blocklist_terms", mod.FilterSize(), "admins", mod.AdminCount(), "banlist", banPath)
	if *stunHost == "" || *stunPort < 1 || *stunPort > 65535 {
		slog.Error("invalid STUN configuration", "host", *stunHost, "port", *stunPort)
		os.Exit(1)
	}
	if *turnHost != "" && (*turnPort < 1 || *turnPort > 65535) {
		slog.Error("invalid TURN port", "port", *turnPort)
		os.Exit(1)
	}

	iceConfig := protocol.ICEConfiguration{
		STUN: protocol.ICETransportServer{Host: *stunHost, Port: uint16(*stunPort)},
	}
	if *turnHost != "" {
		iceConfig.TURN = append(iceConfig.TURN, protocol.ICETransportServer{
			Host: *turnHost, Port: uint16(*turnPort),
			Username: *turnUsername, Password: *turnPassword,
		})
	}
	hub := ws.NewHub(gameState, *udpPublicAddr, iceConfig, mod, settings)

	// Failed moderator logins, counted per IP across the lobby's ADMIN_AUTH and
	// the moderator website: adminLoginMaxFailures (default 5) in a row ban
	// the IP, for adminLoginBanSeconds (default 0, for good); loopback gets a
	// lockout instead.
	guard := moderation.NewLoginGuard(mod)
	guard.SetPolicy(func() moderation.LoginPolicy {
		n, ban := settings.Get().AdminLoginPolicy()
		return moderation.LoginPolicy{MaxFailures: n, BanDuration: ban}
	})
	hub.SetLoginGuard(guard)

	// Per-IP limits on the requests that reach every player (lobby.json; see
	// README "Rate limits"). Refusals add up to an automatic timeout.
	actions := ratelimit.NewActionLimiter()
	actions.SetLimits(func() ratelimit.ActionLimits { return settings.Get().ActionLimits() })
	if limitLoopback {
		actions.SetExemptLoopback(false)
		slog.Warn("TEST BUILD (ratelimit_loopback): request limits and their automatic bans apply to loopback " +
			"addresses too. Don't deploy this binary.")
	}
	hub.SetActionLimiter(actions)

	// The moderators' log (connections with IPs, chat, games, the audit
	// trail), pruned by lobby.json's retention days. RMGK_MODLOG_DB_PATH=off
	// turns it off.
	modlogPath := envOr("RMGK_MODLOG_DB_PATH", "modlog.db")
	var modLog *modlog.Logger
	if strings.EqualFold(modlogPath, "off") {
		slog.Warn("moderator log is off (RMGK_MODLOG_DB_PATH=off)")
	} else if modLog, err = modlog.Open(modlogPath); err != nil {
		slog.Error("moderator log unavailable; running without it", "path", modlogPath, "err", err)
		modLog = nil
	} else {
		modLog.SetRetention(func() modlog.Retention {
			s := settings.Get()
			return modlog.Retention{ChatDays: s.ChatLogDays, ConnectionDays: s.ConnectionLogDays, GameDays: s.GameLogDays,
				MaxBytes: s.ModLogMaxBytes()}
		})
		// room and match ids carry on past the ones in the log, so none is reused
		maxRoom, maxMatch := modLog.MaxIDs()
		gameState.SkipIDs(maxRoom, maxMatch)
		hub.SetModLog(modLog)
		slog.Info("moderator log ready", "path", modlogPath, "next_room_after", maxRoom, "next_match_after", maxMatch,
			"chat_days", settings.Get().ChatLogDays, "connection_days", settings.Get().ConnectionLogDays,
			"game_days", settings.Get().GameLogDays)
		// the rate limit report from before the restart
		slog.Info("rate limit report loaded", "records", hub.LoadRateLimits())
	}

	udpListenAddr, err := net.ResolveUDPAddr("udp", *udpAddr)
	if err != nil {
		slog.Error("failed to resolve UDP address", "err", err)
		os.Exit(1)
	}
	udpConn, err := net.ListenUDP("udp", udpListenAddr)
	if err != nil {
		slog.Error("failed to bind UDP", "err", err)
		os.Exit(1)
	}
	defer udpConn.Close()

	anchor := udp.NewAnchor(udpConn, gameState)

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go anchor.Run(rootCtx)
	go gate.Janitor(rootCtx)
	go hub.Run(rootCtx)
	go hub.RunRateLimits(rootCtx)

	// The moderator website, on its own listener (never on the /ws port).
	var adminSrv *http.Server
	adminLoopback := false // whether it listens on loopback only
	site := modsite.New(modsite.Config{
		Lobby: hub, Mod: mod, Guard: guard, Log: modLog, RateLimits: hub,
		AllowIP: func(ip string) bool { return settings.Get().AdminIPAllowed(ip) },
	})
	if adminListen == "" {
		slog.Info("moderator site disabled (admin address is off)")
	} else if ln, err := modsite.Listen(adminListen, site.Admits); err != nil {
		// The players' lobby matters more: run without the site.
		slog.Error("moderator site can't listen; running without it", "addr", adminListen, "err", err)
		adminListen = ""
	} else {
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			adminLoopback = tcp.IP.IsLoopback()
		}
		adminSrv = modsite.NewHTTPServer(site)
		go func() {
			if err := adminSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("moderator site failed", "err", err)
			}
		}()
		slog.Info("moderator site listening", "addr", ln.Addr().String(), "loopback_only", adminLoopback,
			"allowed_ips", len(settings.Get().AdminAllowed()), "admins", mod.AdminCount(), "logging", modLog != nil)
		if mod.AdminCount() == 0 {
			slog.Warn("moderator site: admins.json has no moderators, so nobody can sign in", "path", adminsPath)
		}
	}

	warnAdminExposure := func() {
		if adminListen != "" && !adminLoopback && len(settings.Get().AdminAllowed()) == 0 {
			slog.Warn("THE MODERATOR SITE IS OPEN TO EVERY ADDRESS: it listens beyond loopback and lobby.json "+
				"adminAllowedIPs is empty. List the moderators' IPs there, firewall the port, or use the default "+
				"--admin-addr 127.0.0.1:8081 with an SSH tunnel.", "admin_addr", adminListen)
		}
	}
	warnAdminExposure()
	go config.WatchFile(rootCtx, configPath, configPollInterval, func() {
		settings.Reload()
		warnAdminExposure()
	})
	go config.WatchFile(rootCtx, adminsPath, configPollInterval, func() {
		a, err := moderation.LoadAdmins(adminsPath)
		if err != nil {
			slog.Error("admins.json reload failed; keeping previous admins", "path", adminsPath, "err", err)
			return
		}
		mod.SetAdmins(a)
		slog.Info("admins reloaded", "path", adminsPath, "admins", mod.AdminCount())
	})
	if blocklistPath != "" {
		go config.WatchFile(rootCtx, blocklistPath, configPollInterval, func() {
			mod.SetFilter(moderation.NewFilter(loadBlocklist(blocklistPath)))
			slog.Info("blocklist reloaded", "path", blocklistPath, "blocklist_terms", mod.FilterSize())
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ip := extractIP(r)
		allowed, retryAfter := gate.Allow(ip)
		if !allowed {
			w.Header().Set("Retry-After", retryAfterHeader(retryAfter))
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			slog.Debug("rate limited", "ip", ip, "retry_after", retryAfter)
			return
		}
		// ServeWS blocks for the connection's lifetime; balance the
		// active-connection count when it ends, however it ends.
		defer gate.Release(ip)
		hub.ServeWS(w, r, ip)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    *wsAddr,
		Handler: mux,
		// Only the request headers: a WebSocket is hijacked once they're read,
		// so this never cuts a long-lived connection.
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("listening", "ws", *wsAddr, "udp", *udpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "err", err)
		}
	}()

	<-sigCh
	slog.Info("shutting down")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown error", "err", err)
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("moderator site shutdown error", "err", err)
		}
	}
	// bans made since the last write, the rate limit report's last changes,
	// then (last) the log writes what's queued and marks open connections and
	// games over
	mod.FlushBans()
	hub.SaveRateLimits()
	if err := modLog.Close(); err != nil {
		slog.Warn("moderator log close error", "err", err)
	}
}

// defaultAdminAddr keeps the moderator site on this machine: reach it with
// ssh -L 8081:127.0.0.1:8081 user@server.
const defaultAdminAddr = "127.0.0.1:8081"

// resolveAdminAddr is where the moderator site listens: --admin-addr when
// given, else RMGK_ADMIN_ADDR when set, else the default; "" when off.
func resolveAdminAddr(flagValue string) string {
	addr := flagValue
	given := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "admin-addr" {
			given = true
		}
	})
	if !given {
		if v, ok := os.LookupEnv("RMGK_ADMIN_ADDR"); ok {
			addr = v
		}
	}
	addr = strings.TrimSpace(addr)
	if strings.EqualFold(addr, "off") {
		return ""
	}
	return addr
}

// loadBlocklist returns the override blocklist at path, or the embedded one
// when path is empty or unreadable.
func loadBlocklist(path string) []string {
	if path == "" {
		return moderation.DefaultTerms()
	}
	terms, err := moderation.LoadBlocklistFile(path)
	if err != nil {
		slog.Warn("blocklist override load failed; using embedded default", "path", path, "err", err)
		return moderation.DefaultTerms()
	}
	return terms
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func extractIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func retryAfterHeader(d time.Duration) string {
	seconds := int(d.Round(time.Second).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds * int(time.Second)).String()
}
