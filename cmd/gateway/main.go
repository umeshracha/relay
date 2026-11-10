package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/catalog"
	"api-telemetry-gateway/internal/jobs"
	"api-telemetry-gateway/internal/portal"
	"api-telemetry-gateway/internal/proxy"
	"api-telemetry-gateway/internal/telemetry"
)

func main() {
	address := flag.String("listen", "127.0.0.1:8080", "gateway listen address")
	adminAddress := flag.String("admin-listen", "127.0.0.1:8081", "admin portal listen address")
	database := flag.String("db", "gateway.db", "SQLite database path")
	trustedProxies := flag.String("trusted-proxies", "", "comma-separated CIDRs of trusted forwarding proxies")
	tlsCert := flag.String("tls-cert", "", "PEM certificate for both listeners")
	tlsKey := flag.String("tls-key", "", "PEM private key for both listeners")
	flag.Parse()
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("set both -tls-cert and -tls-key")
	}
	trusted, err := proxy.ParseTrustedProxies(*trustedProxies)
	if err != nil {
		log.Fatal(err)
	}

	if err := run(*address, *adminAddress, *database, trusted, *tlsCert, *tlsKey); err != nil {
		log.Fatal(err)
	}
}

func run(address, adminAddress, database string, trusted []netip.Prefix, certFile, keyFile string) error {
	password := os.Getenv("GATEWAY_ADMIN_PASSWORD")
	if len(password) < 16 {
		return errors.New("set GATEWAY_ADMIN_PASSWORD to at least 16 characters")
	}
	scheme := "http"
	var tlsConfig *tls.Config
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		scheme = "https"
	}
	store, err := catalog.Open(database)
	if err != nil {
		return err
	}
	defer store.Close()

	gatewayListener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer gatewayListener.Close()
	adminListener, err := net.Listen("tcp", adminAddress)
	if err != nil {
		return err
	}
	defer adminListener.Close()
	recorder, err := telemetry.Open(database + ".telemetry")
	if err != nil {
		return err
	}
	defer recorder.Close()
	keys, err := access.Open(database + ".access")
	if err != nil {
		return err
	}
	defer keys.Close()
	queue, err := jobs.Open(database + ".jobs")
	if err != nil {
		return err
	}
	defer queue.Close()
	workerContext, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		queue.Run(workerContext, store, recorder, keys)
	}()
	defer func() {
		stopWorker()
		<-workerDone
	}()
	gateway := newServer(proxy.Handler(store, recorder, keys, trusted))
	admin := newServer(portal.Handler(store, recorder, keys, queue, password))
	serve := func(server *http.Server, listener net.Listener) error {
		if tlsConfig != nil {
			server.TLSConfig = tlsConfig.Clone()
			return server.ServeTLS(listener, "", "")
		}
		return server.Serve(listener)
	}
	failure := make(chan error, 2)
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { failure <- serve(gateway, gatewayListener) }()
	go func() { failure <- serve(admin, adminListener) }()
	log.Printf("Gateway %s://%s · Admin portal %s://%s", scheme, gatewayListener.Addr(), scheme, adminListener.Addr())
	var serveErr error
	select {
	case <-signals.Done():
	case serveErr = <-failure:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range []*http.Server{gateway, admin} {
		if err := server.Shutdown(ctx); err != nil {
			server.Close()
		}
	}
	return serveErr
}

func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
