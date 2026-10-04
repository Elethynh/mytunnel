package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readinessTestRoute struct {
	address string
	path    string
	proof   string
	closed  atomic.Bool
}

func (r *readinessTestRoute) Spec() routeSpec    { return routeSpec{Subdomain: "ready", Port: 3000} }
func (r *readinessTestRoute) URL() string        { return r.address }
func (r *readinessTestRoute) InstanceID() string { return "ready-id" }
func (r *readinessTestRoute) Readiness() routeReadiness {
	return routeReadiness{Path: r.path, Proof: r.proof}
}
func (r *readinessTestRoute) Wait() error  { select {} }
func (r *readinessTestRoute) Close() error { r.closed.Store(true); return nil }

func testReadinessProbe(client *http.Client) readinessProbe {
	return readinessProbe{
		client: client, overallTimeout: 250 * time.Millisecond,
		requestTimeout: 100 * time.Millisecond, retryDelay: 10 * time.Millisecond,
	}
}

func TestReadinessRetriesTransientFailuresAndConfirmsOnce(t *testing.T) {
	const proof = "current-proof"
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) < 3 {
			http.Error(w, "not yet", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, proof)
	}))
	defer server.Close()
	route := &readinessTestRoute{address: server.URL, path: "/.mytunnel/ready", proof: proof}

	results := collectReadinessResults(probePublicRoutes(context.Background(), []routeSession{route}, testReadinessProbe(server.Client())))
	if len(results) != 1 || results[0].Status != readinessConfirmed {
		t.Fatalf("results = %+v", results)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestReadinessRequiresExactSecureResponse(t *testing.T) {
	const proof = "expected-proof"
	var redirects atomic.Int32
	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirects.Add(1)
		fmt.Fprint(w, proof)
	}))
	defer redirectTarget.Close()

	tests := []struct {
		name    string
		handler http.Handler
		client  func(*httptest.Server) *http.Client
	}{
		{name: "wrong proof", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "stale-proof") })},
		{name: "proof with trailing bytes", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, proof+"x") })},
		{name: "redirect", handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
		})},
		{name: "other host", handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host == "ready.example" {
				fmt.Fprint(w, proof)
				return
			}
			fmt.Fprint(w, "other-host")
		})},
		{name: "invalid certificate", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, proof) }), client: func(*httptest.Server) *http.Client { return http.DefaultClient }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tt.handler)
			defer server.Close()
			client := server.Client()
			if tt.client != nil {
				client = tt.client(server)
			}
			route := &readinessTestRoute{address: server.URL, path: "/.mytunnel/ready", proof: proof}
			result := <-probePublicRoutes(context.Background(), []routeSession{route}, testReadinessProbe(client))
			if result.Status == readinessConfirmed {
				t.Fatal("unexpected readiness confirmation")
			}
		})
	}
	if got := redirects.Load(); got != 0 {
		t.Fatalf("followed %d redirects", got)
	}
}

func TestReadinessUsesEnvironmentProxyWithoutWeakeningTLS(t *testing.T) {
	if os.Getenv("MYTUNNEL_PROXY_TEST_CHILD") == "1" {
		runProxyReadinessChild(t)
		return
	}
	const proof = "proxied-proof"
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || host != "example.com" {
			t.Errorf("origin host = %q", r.Host)
		}
		fmt.Fprint(w, proof)
	}))
	defer origin.Close()

	proxy, proxyUsed := startConnectProxy(t, origin.Listener.Addr().String())
	defer proxy.Close()
	_, port, err := net.SplitHostPort(origin.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestReadinessUsesEnvironmentProxyWithoutWeakeningTLS$")
	command.Env = append(os.Environ(),
		"MYTUNNEL_PROXY_TEST_CHILD=1",
		"HTTPS_PROXY="+proxy.URL,
		"https_proxy="+proxy.URL,
		"NO_PROXY=",
		"no_proxy=",
		"MYTUNNEL_PROXY_TEST_URL=https://example.com:"+port,
		"MYTUNNEL_PROXY_TEST_CERT="+base64.StdEncoding.EncodeToString(origin.Certificate().Raw),
		"MYTUNNEL_PROXY_TEST_PROOF="+proof,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("proxy child: %v\n%s", err, output)
	}
	if !proxyUsed.Load() {
		t.Fatal("environment proxy was not used")
	}
}

func runProxyReadinessChild(t *testing.T) {
	certificate, err := base64.StdEncoding.DecodeString(os.Getenv("MYTUNNEL_PROXY_TEST_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(certificate)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(parsed)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	client := &http.Client{Transport: transport}
	route := &readinessTestRoute{
		address: os.Getenv("MYTUNNEL_PROXY_TEST_URL"),
		path:    "/.mytunnel/ready", proof: os.Getenv("MYTUNNEL_PROXY_TEST_PROOF"),
	}
	result := <-probePublicRoutes(context.Background(), []routeSession{route}, testReadinessProbe(client))
	if result.Status != readinessConfirmed {
		t.Fatalf("status = %v", result.Status)
	}
}

func TestReadinessCancellationIsPromptAndTimeoutKeepsRouteOpen(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(time.Second)
		fmt.Fprint(w, "proof")
	}))
	defer server.Close()
	route := &readinessTestRoute{address: server.URL, path: "/.mytunnel/ready", proof: "proof"}
	probe := testReadinessProbe(server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	results := probePublicRoutes(ctx, []routeSession{route}, probe)
	cancel()
	if result := <-results; result.Status != readinessCanceled {
		t.Fatalf("cancelled status = %v", result.Status)
	}
	if time.Since(started) > 200*time.Millisecond {
		t.Fatal("cancellation was not prompt")
	}

	result := <-probePublicRoutes(context.Background(), []routeSession{route}, readinessProbe{
		client: server.Client(), overallTimeout: 40 * time.Millisecond,
		requestTimeout: 20 * time.Millisecond, retryDelay: 5 * time.Millisecond,
	})
	if result.Status != readinessTimedOut {
		t.Fatalf("timeout status = %v", result.Status)
	}
	if route.closed.Load() {
		t.Fatal("readiness timeout closed the route")
	}
}

func TestReadinessProbesRoutesConcurrently(t *testing.T) {
	const proof = "proof"
	var active atomic.Int32
	bothStarted := make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if active.Add(1) == 2 {
			once.Do(func() { close(bothStarted) })
		}
		select {
		case <-bothStarted:
			fmt.Fprint(w, proof)
		case <-time.After(time.Second):
			http.Error(w, "probes were serialized", http.StatusGatewayTimeout)
		}
	}))
	defer server.Close()
	routes := []routeSession{
		&readinessTestRoute{address: server.URL, path: "/first", proof: proof},
		&readinessTestRoute{address: server.URL, path: "/second", proof: proof},
	}

	results := collectReadinessResults(probePublicRoutes(context.Background(), routes, testReadinessProbe(server.Client())))
	if len(results) != 2 || results[0].Status != readinessConfirmed || results[1].Status != readinessConfirmed {
		t.Fatalf("results = %+v", results)
	}
}

func collectReadinessResults(results <-chan routeReadinessResult) []routeReadinessResult {
	var collected []routeReadinessResult
	for result := range results {
		collected = append(collected, result)
	}
	return collected
}

func startConnectProxy(t *testing.T, target string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	used := &atomic.Bool{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		used.Store(true)
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			t.Error(err)
			return
		}
		go io.Copy(upstream, client)
		_, _ = io.Copy(client, upstream)
	}))
	return proxy, used
}
