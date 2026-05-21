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
	"syscall"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
	"github.com/jay-day/rmgk-lobby/internal/udp"
	"github.com/jay-day/rmgk-lobby/internal/ws"
)

func main() {
	var (
		wsAddr        = flag.String("ws-addr", ":8080", "WebSocket listen address")
		udpAddr       = flag.String("udp-addr", ":6364", "UDP anchor listen address")
		udpPublicAddr = flag.String("udp-public-addr", "127.0.0.1:6364", "Public host:port of the UDP anchor advertised to clients")
		logJSON       = flag.Bool("log-json", false, "Emit logs as JSON")
	)
	flag.Parse()

	var handler slog.Handler
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	slog.SetDefault(slog.New(handler))

	gameState := state.New()
	gate := ratelimit.NewConnectGate()
	hub := ws.NewHub(gameState, *udpPublicAddr)

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
		hub.ServeWS(w, r, ip)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    *wsAddr,
		Handler: mux,
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
