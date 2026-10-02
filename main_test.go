package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func serverPort(server *httptest.Server) int {
	return server.Listener.Addr().(*net.TCPAddr).Port
}

func socketTestDir(t *testing.T) string {
	t.Helper()
	// macOS temporary paths can exceed the Unix socket path length limit.
	dir, err := os.MkdirTemp("/tmp", "mytunnel-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func TestSettingsFirstUse(t *testing.T) {
	t.Setenv("MYTUNNEL_HOME", t.TempDir())
	first, dir, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := loadSettings()
	if err != nil || second.Token != first.Token {
		t.Fatalf("settings changed: %+v, %v", second, err)
	}
	info, err := os.Stat(filepath.Join(dir, "settings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("settings permissions: %v, %v", info, err)
	}
}

func TestWebSocketUpgradePassesThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		fmt.Fprint(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		buffered.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(buffered, payload); err != nil {
			t.Error(err)
			return
		}
		connection.Write(payload)
	}))
	defer backend.Close()
	port := serverPort(backend)
	settings := Settings{Token: strings.Repeat("e", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	response, route, err := sendControl(daemon.socketPath, controlRequest{Token: settings.Token, Type: controlRegister, Subdomain: "socket", Port: port})
	if err != nil || !response.OK {
		t.Fatalf("route: %+v, %v", response, err)
	}
	defer route.Close()
	client, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", daemon.RouterPort))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(client, "GET / HTTP/1.1\r\nHost: socket.localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade: %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	client.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q", got)
	}
}

func TestValidationAndTunnelConfig(t *testing.T) {
	if got, err := normalizeSubdomain("Demo-1"); err != nil || got != "demo-1" {
		t.Fatalf("subdomain = %q, %v", got, err)
	}
	for _, bad := range []string{"a.b", "-bad", "bad-", strings.Repeat("a", 64), "a_b"} {
		if _, err := normalizeSubdomain(bad); err == nil {
			t.Errorf("accepted invalid subdomain %q", bad)
		}
	}
	if port, err := parsePort("3000"); err != nil || port != 3000 {
		t.Fatalf("port = %d, %v", port, err)
	}
	for _, bad := range []string{"0", "65536", "3.5", "3abc"} {
		if _, err := parsePort(bad); err == nil {
			t.Errorf("accepted invalid port %q", bad)
		}
	}
	config, err := renderCloudflaredConfig(Settings{
		Domain: "example.com", Tunnel: "6ff42ae2-765d-4adf-8112-31c55c1551ef",
		Credentials: "/tmp/tunnel.json", RouterPort: 43187,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`hostname: "*.example.com"`, `service: "http://127.0.0.1:43187"`, "- service: http_status:404"} {
		if !strings.Contains(config, part) {
			t.Errorf("config missing %q: %s", part, config)
		}
	}
}

func TestDaemonRoutesAndReleasesSessions(t *testing.T) {
	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "one:%s:%s", r.URL.Path, r.Host)
	}))
	defer one.Close()
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "two")
	}))
	defer two.Close()
	settings := Settings{Token: strings.Repeat("a", 64), RouterPort: 0}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	controlAddr := daemon.socketPath
	first, firstConn, err := sendControl(controlAddr, controlRequest{Token: settings.Token, Type: controlRegister, Subdomain: "one", Port: serverPort(one)})
	if err != nil || !first.OK {
		t.Fatalf("first route: %+v, %v", first, err)
	}
	defer firstConn.Close()
	if first.URL != fmt.Sprintf("http://one.localhost:%d", daemon.RouterPort) {
		t.Fatalf("url = %s", first.URL)
	}
	second, secondConn, err := sendControl(controlAddr, controlRequest{Token: settings.Token, Type: controlRegister, Subdomain: "two", Port: serverPort(two)})
	if err != nil || !second.OK {
		t.Fatalf("second route: %+v, %v", second, err)
	}
	defer secondConn.Close()
	duplicate, dupConn, err := sendControl(controlAddr, controlRequest{Token: settings.Token, Type: controlRegister, Subdomain: "one", Port: serverPort(two)})
	if err != nil || duplicate.OK {
		t.Fatalf("duplicate: %+v, %v", duplicate, err)
	}
	dupConn.Close()
	wrong, wrongConn, err := sendControl(controlAddr, controlRequest{Token: "wrong", Type: controlStatus})
	if err != nil || wrong.OK {
		t.Fatalf("wrong token: %+v, %v", wrong, err)
	}
	wrongConn.Close()

	get := func(host string) (int, string) {
		request, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/hello", daemon.RouterPort), nil)
		request.Host = host
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	if code, body := get("one.localhost"); code != 200 || body != "one:/hello:one.localhost" {
		t.Fatalf("one: %d %q", code, body)
	}
	if code, body := get("two.localhost"); code != 200 || body != "two" {
		t.Fatalf("two: %d %q", code, body)
	}
	if code, _ := get("unknown.localhost"); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	firstConn.Close()
	deadline := time.Now().Add(time.Second)
	for {
		if code, _ := get("one.localhost"); code == 404 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("route was not removed after CLI disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, body := get("two.localhost"); code != 200 || body != "two" {
		t.Fatalf("two after first closed: %d %q", code, body)
	}
}

func TestPublicHostAndIdleShutdown(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("b", 64), Domain: "example.com"}
	daemon, err := startDaemon(settings, socketTestDir(t), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	response, route, err := sendControl(daemon.socketPath, controlRequest{Token: settings.Token, Type: controlRegister, Subdomain: "demo", Port: serverPort(backend)})
	if err != nil || !response.OK || response.URL != "https://demo.example.com" {
		t.Fatalf("public route: %+v, %v", response, err)
	}
	request := func(host string) int {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", daemon.RouterPort), nil)
		req.Host = host
		result, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result.Body.Close()
		return result.StatusCode
	}
	if got := request("demo.example.com"); got != 200 {
		t.Fatalf("configured host: %d", got)
	}
	for _, host := range []string{"demo.other.com", "other.example.com", "nested.demo.example.com"} {
		if got := request(host); got != 404 {
			t.Errorf("host %s: %d", host, got)
		}
	}
	self, selfConn, err := sendControl(daemon.socketPath, controlRequest{Token: settings.Token, Type: controlRegister, Subdomain: "self", Port: daemon.RouterPort})
	if err != nil || self.OK {
		t.Fatalf("self-route: %+v, %v", self, err)
	}
	selfConn.Close()
	select {
	case <-daemon.stopCh:
		t.Fatal("stopped while a route was active")
	case <-time.After(100 * time.Millisecond):
	}
	route.Close()
	select {
	case <-daemon.stopCh:
	case <-time.After(time.Second):
		t.Fatal("did not stop after final route")
	}
}
