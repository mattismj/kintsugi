package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"kintsugi/internal/rpc"
)

func cmdRPC(ws string, args []string) {
	fs := flag.NewFlagSet("rpc", flag.ExitOnError)
	mode := fs.String("mode", "stdio", "transport: stdio|tcp|http")
	addr := fs.String("addr", "127.0.0.1:9876", "TCP/HTTP listen address (loopback only)")
	auth := fs.String("auth", "", "shared secret for TCP/HTTP authentication")
	fs.Parse(args)

	app := newApp(ws)
	srv := rpc.NewServer()
	mgr := rpc.NewSessionManager(app)
	mgr.RegisterHandlers(srv)

	if *auth != "" {
		srv.SetAuth(*auth)
	}

	// Loopback is a bind-time guard, not just a per-request one: the API carries
	// the operator's tool authority, so a network-reachable listener is a leak
	// even if every request from it would be refused.
	if *mode == "tcp" || *mode == "http" {
		if !rpc.IsLoopbackBindAddr(*addr) {
			fail("rpc: --addr must be loopback (127.0.0.1 or ::1), got %s", *addr)
		}
		*addr = rpc.BindAddr(*addr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	switch *mode {
	case "stdio":
		if err := srv.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil {
			fail("rpc stdio: %v", err)
		}

	case "tcp":
		fmt.Fprintf(os.Stderr, "kintsugi rpc: listening on %s (loopback only)\n", *addr)
		if err := srv.ServeTCP(ctx, *addr); err != nil {
			fail("rpc tcp: %v", err)
		}

	case "http":
		fmt.Fprintf(os.Stderr, "kintsugi rpc: HTTP server on %s (loopback only)\n", *addr)
		httpSrv := &http.Server{
			Addr:    *addr,
			Handler: srv.HTTPHandler(),
		}
		go func() {
			<-ctx.Done()
			httpSrv.Close()
		}()
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fail("rpc http: %v", err)
		}

	default:
		fail("unknown mode %q (want stdio|tcp|http)", *mode)
	}
}
