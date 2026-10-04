package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRouteSession struct {
	spec   routeSpec
	wait   chan error
	closed chan struct{}
	once   sync.Once
}

func newFakeRouteSession(name string) *fakeRouteSession {
	return &fakeRouteSession{
		spec:   routeSpec{Subdomain: name, Port: 3000},
		wait:   make(chan error, 1),
		closed: make(chan struct{}),
	}
}

func (s *fakeRouteSession) Spec() routeSpec    { return s.spec }
func (s *fakeRouteSession) URL() string        { return "http://" + s.spec.Subdomain + ".localhost" }
func (s *fakeRouteSession) InstanceID() string { return s.spec.Subdomain + "-id" }
func (s *fakeRouteSession) Wait() error        { return <-s.wait }
func (s *fakeRouteSession) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.wait <- net.ErrClosed
	})
	return nil
}

func TestRouteSessionGroupCancellationClosesOnlyOwnedSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	group := newRouteSessionGroup(ctx)
	owned := newFakeRouteSession("owned")
	unrelated := newFakeRouteSession("unrelated")
	group.add(owned)

	cancel()
	if err := group.Wait(); err != nil {
		t.Fatalf("wait after cancellation: %v", err)
	}
	select {
	case <-owned.closed:
	case <-time.After(time.Second):
		t.Fatal("owned session was not closed")
	}
	select {
	case <-unrelated.closed:
		t.Fatal("unrelated session was closed")
	default:
	}
	unrelated.Close()
}

func TestRouteSessionGroupOwnershipLossClosesTheGroup(t *testing.T) {
	group := newRouteSessionGroup(context.Background())
	first := newFakeRouteSession("first")
	second := newFakeRouteSession("second")
	group.add(first)
	group.add(second)

	first.wait <- errors.New("lost owner")
	if err := group.Wait(); err == nil || !strings.Contains(err.Error(), "lost owner") {
		t.Fatalf("wait error = %v", err)
	}
	select {
	case <-second.closed:
	case <-time.After(time.Second):
		t.Fatal("sibling session was not closed")
	}
}

func TestLockSettingsDirContextCanBeCancelled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTUNNEL_HOME", dir)
	lock, err := lockSettingsDir()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		other, err := lockSettingsDirContext(ctx)
		if other != nil {
			other.Close()
		}
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lock error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lock acquisition ignored cancellation")
	}
}

func TestOldDaemonIsRejectedBeforeRegistration(t *testing.T) {
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	settings := Settings{Token: strings.Repeat("f", 64), RouterPort: defaultRouterPort}
	if err := saveSettings(settings, dir); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", controlAddress(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan controlRequest, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request controlRequest
			_ = json.NewDecoder(conn).Decode(&request)
			requests <- request
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: true})
			conn.Close()
		}
	}()

	_, _, err = acquireRouteSessions(context.Background(), []routeSpec{{Subdomain: "demo", Port: 3000}})
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("acquire error = %v", err)
	}
	select {
	case request := <-requests:
		if request.Type != controlStatus {
			t.Fatalf("first request = %s", request.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not receive capability check")
	}
	select {
	case request := <-requests:
		t.Fatalf("old daemon received feature registration: %s", request.Type)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRegistrationCanBeCancelled(t *testing.T) {
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	settings := Settings{Token: strings.Repeat("2", 64), RouterPort: defaultRouterPort}
	if err := saveSettings(settings, dir); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", controlAddress(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	registrationStarted := make(chan struct{})
	go func() {
		status, err := listener.Accept()
		if err != nil {
			return
		}
		_ = json.NewDecoder(status).Decode(&controlRequest{})
		_ = json.NewEncoder(status).Encode(controlResponse{OK: true, Capabilities: daemonCapabilities})
		status.Close()

		registration, err := listener.Accept()
		if err != nil {
			return
		}
		defer registration.Close()
		_ = json.NewDecoder(registration).Decode(&controlRequest{})
		close(registrationStarted)
		_, _ = bufio.NewReader(registration).ReadByte()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := acquireRouteSessions(ctx, []routeSpec{{Subdomain: "demo", Port: 3000}})
		done <- err
	}()
	select {
	case <-registrationStarted:
	case <-time.After(time.Second):
		t.Fatal("registration did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("registration ignored cancellation")
	}
}

func TestAcquisitionFailureReleasesEarlierRoutes(t *testing.T) {
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	settings := Settings{Token: strings.Repeat("4", 64), RouterPort: defaultRouterPort}
	if err := saveSettings(settings, dir); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", controlAddress(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	firstReleased := make(chan struct{})
	go func() {
		status, err := listener.Accept()
		if err != nil {
			return
		}
		_ = json.NewDecoder(status).Decode(&controlRequest{})
		_ = json.NewEncoder(status).Encode(controlResponse{OK: true, Capabilities: daemonCapabilities})
		status.Close()

		first, err := listener.Accept()
		if err != nil {
			return
		}
		_ = json.NewDecoder(first).Decode(&controlRequest{})
		_ = json.NewEncoder(first).Encode(controlResponse{
			OK: true, URL: "http://first.localhost", RouteID: "first-id", Capabilities: daemonCapabilities,
		})
		go func() {
			defer first.Close()
			_, _ = bufio.NewReader(first).ReadByte()
			close(firstReleased)
		}()

		second, err := listener.Accept()
		if err != nil {
			return
		}
		defer second.Close()
		_ = json.NewDecoder(second).Decode(&controlRequest{})
		_ = json.NewEncoder(second).Encode(controlResponse{Error: "Subdomain second is already in use."})
	}()

	_, _, err = acquireRouteSessions(context.Background(), []routeSpec{
		{Subdomain: "first", Port: 3000},
		{Subdomain: "second", Port: 3001},
	})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("acquire error = %v", err)
	}
	select {
	case <-firstReleased:
	case <-time.After(time.Second):
		t.Fatal("earlier route was not released")
	}
}

func TestAcquisitionReleasesSettingsLockWhileSessionRuns(t *testing.T) {
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	settings := Settings{Token: strings.Repeat("3", 64), RouterPort: defaultRouterPort}
	if err := saveSettings(settings, dir); err != nil {
		t.Fatal(err)
	}
	daemonSettings := settings
	daemonSettings.RouterPort = 0
	daemon, err := startDaemon(daemonSettings, dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	_, group, err := acquireRouteSessions(context.Background(), []routeSpec{{Subdomain: "demo", Port: 3000}})
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := lockSettingsDirContext(ctx)
	if err != nil {
		t.Fatalf("settings lock remained held: %v", err)
	}
	lock.Close()
}

func TestControlConnectionRetainsCoalescedFrames(t *testing.T) {
	dir := socketTestDir(t)
	listener, err := net.Listen("unix", filepath.Join(dir, "frames.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte("{\"ok\":true}\n{\"event\":\"next\"}\n"))
	}()

	response, connection, err := sendControlContext(context.Background(), listener.Addr().String(), controlRequest{Type: controlStatus})
	if err != nil || !response.OK {
		t.Fatalf("response = %+v, %v", response, err)
	}
	defer connection.Close()
	frame, err := connection.readFrame()
	if err != nil || string(frame) != "{\"event\":\"next\"}\n" {
		t.Fatalf("next frame = %q, %v", frame, err)
	}
}

func TestControlFrameLimitAppliesPerFrame(t *testing.T) {
	valid := strings.Repeat("x", 4095) + "\n"
	reader := bufio.NewReader(strings.NewReader(valid + strings.Repeat("x", 4096) + "\n"))
	first, err := readControlFrame(reader)
	if err != nil || string(first) != valid {
		t.Fatalf("valid frame length = %d, %v", len(first), err)
	}
	if _, err := readControlFrame(reader); err == nil {
		t.Fatal("accepted oversized second frame")
	}
}

func TestNewDaemonSupportsLegacyControlRequests(t *testing.T) {
	settings := Settings{Token: strings.Repeat("1", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	type legacyResponse struct {
		OK     bool          `json:"ok"`
		Error  string        `json:"error,omitempty"`
		URL    string        `json:"url,omitempty"`
		Domain string        `json:"domain,omitempty"`
		Retry  bool          `json:"retry,omitempty"`
		Routes []routeStatus `json:"routes,omitempty"`
	}
	legacyRequest := func(request controlRequest) (legacyResponse, net.Conn, error) {
		connection, err := net.Dial("unix", daemon.socketPath)
		if err != nil {
			return legacyResponse{}, nil, err
		}
		if err := json.NewEncoder(connection).Encode(request); err != nil {
			connection.Close()
			return legacyResponse{}, nil, err
		}
		var response legacyResponse
		if err := json.NewDecoder(connection).Decode(&response); err != nil {
			connection.Close()
			return legacyResponse{}, nil, err
		}
		return response, connection, nil
	}

	status, statusConn, err := legacyRequest(controlRequest{Token: settings.Token, Type: controlStatus})
	if err != nil || !status.OK {
		t.Fatalf("legacy status: %+v, %v", status, err)
	}
	statusConn.Close()
	registered, route, err := legacyRequest(controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "legacy", Port: 3000,
	})
	if err != nil || !registered.OK || registered.URL == "" {
		t.Fatalf("legacy register: %+v, %v", registered, err)
	}
	route.Close()
}
