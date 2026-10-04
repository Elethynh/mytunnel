package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestSettingsDirectoryPreparation(t *testing.T) {
	operations := map[string]func() error{
		"load": func() error {
			_, _, err := loadSettings()
			return err
		},
		"lock": func() error {
			lock, err := lockSettingsDir()
			if err != nil {
				return err
			}
			return lock.Close()
		},
	}
	for name, operation := range operations {
		for _, state := range []string{"missing", "existing", "file"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "settings")
				if state == "missing" {
					dir = filepath.Join(dir, "nested")
				}
				t.Setenv("MYTUNNEL_HOME", dir)
				switch state {
				case "existing":
					if err := os.Mkdir(dir, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(dir, 0755); err != nil {
						t.Fatal(err)
					}
				case "file":
					if err := os.WriteFile(dir, []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				err := operation()
				if state == "file" {
					var pathError *os.PathError
					if !errors.As(err, &pathError) || pathError.Op != "mkdir" || pathError.Path != dir {
						t.Fatalf("directory error = %v", err)
					}
					data, readErr := os.ReadFile(dir)
					if readErr != nil || string(data) != "keep" {
						t.Fatalf("existing file changed: %q, %v", data, readErr)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(dir)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("directory permissions: %v, %v", info, err)
				}
			})
		}
	}
}

func TestRouteHostMatching(t *testing.T) {
	for _, domain := range []string{"localhost", "example.com"} {
		t.Run(domain, func(t *testing.T) {
			target := &routeTarget{port: 5173}
			daemon := &Daemon{routes: map[string]*routeTarget{"local-app": target}}
			if domain != "localhost" {
				daemon.settings.Domain = domain
			}
			for _, host := range []string{
				"local-app." + domain,
				strings.ToUpper("local-app." + domain),
				"local-app." + domain + ":5173",
				"local-app." + domain + ":",
				"local-app." + domain + ":text",
			} {
				if got, found := daemon.routeFor(host); !found || got != target {
					t.Errorf("host %q did not resolve to its route", host)
				}
			}
			for _, host := range []string{
				domain, "." + domain, "unknown." + domain,
				"nested.local-app." + domain, "local-app" + domain,
				"local-app." + domain + ".", "local-app." + domain + ".other",
				"local-app." + domain + ":80:90", "local-app.other",
			} {
				if got, found := daemon.routeFor(host); found || got != nil {
					t.Errorf("unexpected route for host %q", host)
				}
			}
		})
	}
}

func captureStdout(t *testing.T, action func() error) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous }()
	if err := action(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestStatusURLs(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	port := serverPort(backend)
	for _, scenario := range []struct {
		name   string
		domain string
		routes bool
	}{
		{name: "local", routes: true},
		{name: "public", domain: "example.com", routes: true},
		{name: "empty", domain: "example.com"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := socketTestDir(t)
			t.Setenv("MYTUNNEL_HOME", dir)
			settings := Settings{Token: strings.Repeat("c", 64), Domain: scenario.domain}
			daemon, err := startDaemon(settings, dir, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			defer daemon.Close()
			persisted := Settings{Token: settings.Token, RouterPort: defaultRouterPort, Domain: "stored.example"}
			if err := saveSettings(persisted, dir); err != nil {
				t.Fatal(err)
			}
			want := "No active routes.\n"
			if scenario.routes {
				for _, name := range []string{"zeta", "alpha"} {
					response, conn, err := sendControl(controlAddress(dir), controlRequest{
						Token: settings.Token, Type: controlRegister, Subdomain: name, Port: port,
					})
					if err != nil || !response.OK {
						t.Fatalf("register %s: %+v, %v", name, response, err)
					}
					defer conn.Close()
				}
				want = fmt.Sprintf("http://alpha.localhost:43187 → 127.0.0.1:%d\nhttp://zeta.localhost:43187 → 127.0.0.1:%d\n", port, port)
				if scenario.domain != "" {
					want = fmt.Sprintf("https://alpha.example.com → 127.0.0.1:%d\nhttps://zeta.example.com → 127.0.0.1:%d\n", port, port)
				}
			}
			if got := captureStdout(t, status); got != want {
				t.Fatalf("status output = %q, want %q", got, want)
			}
		})
	}
}

func TestWebSocketUpgradePassesThrough(t *testing.T) {
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprintf("logs=%t", logs), func(t *testing.T) {
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
			settings := Settings{Token: strings.Repeat("e", 64)}
			daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			defer daemon.Close()
			response, route, err := sendControl(daemon.socketPath, controlRequest{
				Token: settings.Token, Type: controlRegister, Subdomain: "socket", Port: serverPort(backend),
			})
			if err != nil || !response.OK {
				t.Fatalf("route: %+v, %v", response, err)
			}
			defer route.Close()
			var observer *controlConnection
			if logs {
				observer = observeRegisteredRoute(t, daemon, settings, response, "socket")
			}
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
			client.Close()
			if logs {
				event := readRequestLogEvent(t, observer)
				if event.Status != http.StatusSwitchingProtocols || event.Path != "/" {
					t.Fatalf("WebSocket event = %+v", event)
				}
				observer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
				if frame, err := observer.readFrame(); err == nil {
					t.Fatalf("WebSocket emitted extra completion %q", frame)
				}
			}
		})
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

func TestDaemonServesRouteScopedReadinessWithoutBackendTraffic(t *testing.T) {
	var backendRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendRequests.Add(1)
		fmt.Fprintf(w, "%s:%s", r.Method, r.URL.Path)
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("d", 64), Domain: "example.com"}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	registered, owner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "proof", Port: serverPort(backend),
	})
	if err != nil || !registered.OK {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	if registered.ReadinessPath == "" || registered.ReadinessProof == "" || registered.ReadinessPath == registered.ReadinessProof {
		t.Fatalf("readiness challenge = %q, proof = %q", registered.ReadinessPath, registered.ReadinessProof)
	}
	challenge := strings.TrimPrefix(registered.ReadinessPath, "/.mytunnel/readiness/")
	if len(challenge) != 32 || len(registered.ReadinessProof) != 32 {
		t.Fatalf("readiness entropy lengths = %d and %d", len(challenge), len(registered.ReadinessProof))
	}
	if _, err := hex.DecodeString(challenge); err != nil {
		t.Fatalf("challenge encoding: %v", err)
	}
	if _, err := hex.DecodeString(registered.ReadinessProof); err != nil {
		t.Fatalf("proof encoding: %v", err)
	}

	request := func(method, host, path string) (int, string, http.Header) {
		req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", daemon.RouterPort, path), nil)
		req.Host = host
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body), response.Header
	}
	code, body, headers := request(http.MethodGet, "proof.example.com", registered.ReadinessPath)
	if code != http.StatusOK || body != registered.ReadinessProof {
		t.Fatalf("proof response = %d %q", code, body)
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", headers.Get("Cache-Control"))
	}
	if got := backendRequests.Load(); got != 0 {
		t.Fatalf("backend readiness requests = %d", got)
	}

	if code, body, _ := request(http.MethodHead, "proof.example.com", registered.ReadinessPath); code != 200 || body != "" {
		t.Fatalf("ordinary HEAD = %d %q", code, body)
	}
	if code, body, _ := request(http.MethodGet, "proof.example.com", "/ordinary"); code != 200 || body != "GET:/ordinary" {
		t.Fatalf("ordinary GET = %d %q", code, body)
	}
	if got := backendRequests.Load(); got != 2 {
		t.Fatalf("backend ordinary requests = %d", got)
	}
	if code, _, _ := request(http.MethodGet, "other.example.com", registered.ReadinessPath); code != http.StatusNotFound {
		t.Fatalf("other host status = %d", code)
	}

	owner.Close()
	deadline := time.Now().Add(time.Second)
	for {
		if code, _, _ := request(http.MethodGet, "proof.example.com", registered.ReadinessPath); code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired route instance still served readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	replacement, replacementOwner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "proof", Port: serverPort(backend),
	})
	if err != nil || !replacement.OK {
		t.Fatalf("replacement register: %+v, %v", replacement, err)
	}
	defer replacementOwner.Close()
	if replacement.ReadinessPath == registered.ReadinessPath || replacement.ReadinessProof == registered.ReadinessProof {
		t.Fatal("replacement route reused the old readiness challenge")
	}
	if code, body, _ := request(http.MethodGet, "proof.example.com", registered.ReadinessPath); code != http.StatusOK || body == registered.ReadinessProof {
		t.Fatalf("stale readiness response = %d %q", code, body)
	}
	if code, body, _ := request(http.MethodGet, "proof.example.com", replacement.ReadinessPath); code != http.StatusOK || body != replacement.ReadinessProof {
		t.Fatalf("replacement readiness response = %d %q", code, body)
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
