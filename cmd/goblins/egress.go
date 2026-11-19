package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ungarscool1/goblins/internal/egress"
	"github.com/ungarscool1/goblins/internal/observability"
)

// egressProxy runs the allow-listing proxy that agent workloads reach the internet through.
// The list comes from flags or GOBLINS_EGRESS_ALLOW, comma-separated.
func egressProxy(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "0.0.0.0:3128", "address to listen on")
	allow := fs.String("allow", os.Getenv("GOBLINS_EGRESS_ALLOW"),
		"hosts to allow, comma-separated; *.example.com allows subdomains")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := observability.NewLogger(stderr, observability.LogOptions{})
	if err != nil {
		return err
	}
	var hosts []string
	for _, h := range strings.Split(*allow, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		log.Warn("the allow-list is empty: every tunnel will be refused")
	}
	p, err := egress.New(egress.Options{Allow: hosts, Log: log})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("egress proxy listening", "listen", ln.Addr().String(), "allow", hosts)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
