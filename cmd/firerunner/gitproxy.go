package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/gitproxy"
)

// gitProxyDrain bounds the wait for responses still being stored at shutdown.
// It stays under the unit's TimeoutStopSec, and short: until this process ends,
// git is still pointed at a closed port.
const gitProxyDrain = 3 * time.Second

// runGitProxy serves the loopback proxy git is pointed at on an EC2 instance.
func runGitProxy(args []string) error {
	fs := flag.NewFlagSet("git-proxy", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:3129", "loopback address to listen on")
	cert := fs.String("cert", "", "certificate for github.com (required)")
	key := fs.String("key", "", "the certificate's key (required)")
	cacheURL := fs.String("cache-url", "http://127.0.0.1", "the instance's cache proxy")
	tempDir := fs.String("temp-dir", "", "directory for responses on their way (default: the system's)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *cert == "" || *key == "" {
		return errors.New("--cert and --key are required")
	}
	if err := requireLoopback(*listen); err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		return fmt.Errorf("loading the certificate: %w", err)
	}
	log := config.NewLogger("info", "text")
	if err := gitproxy.ClearTemp(*tempDir); err != nil {
		log.Warn("git-proxy: clearing old temporary files", "err", err)
	}
	proxy := gitproxy.New(gitproxy.Config{
		Certificate: pair,
		Cache:       &gitproxy.CacheServer{URL: *cacheURL},
		TempDir:     *tempDir,
		Log:         log,
	})
	srv := &http.Server{Addr: *listen, Handler: proxy, ReadHeaderTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Info("git-proxy listening", "addr", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	drain, cancel := context.WithTimeout(context.Background(), gitProxyDrain)
	defer cancel()
	proxy.Wait(drain)
	slog.Info("git-proxy stopped")
	return nil
}

// requireLoopback refuses any other address: cache-server knows a job by this
// VM's address, so a proxy others could reach would act for this VM's job.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--listen %s is not a loopback address", addr)
	}
	return nil
}
