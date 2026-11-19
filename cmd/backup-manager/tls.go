package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// HTTPS without a separate web server:
//   BM_TLS_DOMAIN=backups.example.com   a free certificate from Let's Encrypt, renewed by itself.
//     Needs ports 80 and 443 free on this machine and the domain pointing here.
//   BM_TLS_CERT=/path/fullchain.pem BM_TLS_KEY=/path/key.pem   your own certificate.
// BM_HTTPS_LISTEN (default :443) and BM_HTTP_LISTEN (default :80, for the certificate check and
// the redirect to https) set the ports. The plain panel on BM_LISTEN stays for this machine.

type tlsSetup struct {
	server   *http.Server
	redirect *http.Server
	certFile string
	keyFile  string
	describe string
}

func setupTLS(dataDir string, handler http.Handler) (*tlsSetup, error) {
	domain := strings.TrimSpace(os.Getenv("BM_TLS_DOMAIN"))
	cert, key := os.Getenv("BM_TLS_CERT"), os.Getenv("BM_TLS_KEY")
	if domain == "" && cert == "" {
		return nil, nil
	}
	addr := env("BM_HTTPS_LISTEN", ":443")
	t := &tlsSetup{server: &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 15 * time.Second}}
	httpAddr := env("BM_HTTP_LISTEN", ":80")
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if _, port, _ := net.SplitHostPort(addr); port != "" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	switch {
	case cert != "" || key != "":
		if cert == "" || key == "" {
			return nil, errors.New("set both BM_TLS_CERT and BM_TLS_KEY")
		}
		if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
			return nil, fmt.Errorf("the certificate files cannot be used: %w", err)
		}
		t.certFile, t.keyFile, t.describe = cert, key, "own certificate "+cert
		if httpAddr != "off" {
			t.redirect = &http.Server{Addr: httpAddr, Handler: redirect, ReadHeaderTimeout: 15 * time.Second}
		}
	default:
		if strings.ContainsAny(domain, " /:") || !strings.Contains(domain, ".") {
			return nil, fmt.Errorf("BM_TLS_DOMAIN %q is not a domain name", domain)
		}
		dir := filepath.Join(dataDir, "tls")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		m := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(domain), Cache: autocert.DirCache(dir), Email: os.Getenv("BM_TLS_EMAIL")}
		t.server.TLSConfig = m.TLSConfig()
		t.describe = "Let's Encrypt certificate for " + domain
		// Port 80 must answer the certificate authority's check; everything else is sent to https.
		t.redirect = &http.Server{Addr: httpAddr, Handler: m.HTTPHandler(redirect), ReadHeaderTimeout: 15 * time.Second}
	}
	return t, nil
}

func (t *tlsSetup) run(ctx context.Context, errs chan<- error) {
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t.server.Shutdown(c)
		if t.redirect != nil {
			t.redirect.Shutdown(c)
		}
	}()
	if t.redirect != nil {
		go func() {
			if err := t.redirect.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("http %s: %w (is another web server using it?)", t.redirect.Addr, err)
			}
		}()
	}
	go func() {
		log.Printf("HTTPS on %s with %s", t.server.Addr, t.describe)
		if err := t.server.ListenAndServeTLS(t.certFile, t.keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("https %s: %w (is another web server using it?)", t.server.Addr, err)
		}
	}()
}
