package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func observeRegisteredRoute(t *testing.T, daemon *Daemon, settings Settings, registered controlResponse, subdomain string) *controlConnection {
	t.Helper()
	ack, observer, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlObserve, Subdomain: subdomain, RouteID: registered.RouteID,
	})
	if err != nil || !ack.OK {
		t.Fatalf("observe: %+v, %v", ack, err)
	}
	t.Cleanup(func() { observer.Close() })
	return observer
}

func readRequestLogEvent(t *testing.T, observer *controlConnection) requestLogEvent {
	t.Helper()
	observer.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := observer.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	var event requestLogEvent
	if err := json.Unmarshal(frame, &event); err != nil {
		t.Fatalf("event frame %q: %v", frame, err)
	}
	return event
}

func TestRequestLogsReportCompletionWithoutSensitiveData(t *testing.T) {
	const querySecret = "query-secret"
	const headerSecret = "header-secret"
	const bodySecret = "body-secret"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.URL.RawQuery != "token="+querySecret || r.Header.Get("X-Secret") != headerSecret || string(body) != bodySecret {
			t.Errorf("backend request changed: query=%q header=%q body=%q", r.URL.RawQuery, r.Header.Get("X-Secret"), body)
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()

	settings := Settings{Token: strings.Repeat("a", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	registered, owner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "demo", Port: serverPort(backend),
	})
	if err != nil || !registered.OK {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	defer owner.Close()
	observer := observeRegisteredRoute(t, daemon, settings, registered, "demo")

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://127.0.0.1:"+strconv.Itoa(daemon.RouterPort)+"/hello/%1Bworld?token="+querySecret,
		bytes.NewBufferString(bodySecret))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "demo.localhost"
	request.Header.Set("X-Secret", headerSecret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTeapot {
		t.Fatalf("response status = %d", response.StatusCode)
	}

	observer.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := observer.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	var event requestLogEvent
	if err := json.Unmarshal(frame, &event); err != nil {
		t.Fatalf("event frame %q: %v", frame, err)
	}
	if event.Type != requestLogEventType || event.Route != "demo" || event.Method != http.MethodPost ||
		event.Path != "/hello/%1Bworld" || event.Status != http.StatusTeapot || event.DurationMicros < 0 {
		t.Fatalf("event = %+v", event)
	}
	for _, secret := range []string{querySecret, headerSecret, bodySecret, settings.Token, registered.ReadinessProof} {
		if strings.Contains(string(frame), secret) {
			t.Fatalf("event leaked %q: %s", secret, frame)
		}
	}
	if len(frame) > controlFrameLimit {
		t.Fatalf("event frame length = %d", len(frame))
	}

	proofRequest, _ := http.NewRequest(http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(daemon.RouterPort)+registered.ReadinessPath, nil)
	proofRequest.Host = "demo.localhost"
	proofResponse, err := http.DefaultClient.Do(proofRequest)
	if err != nil {
		t.Fatal(err)
	}
	proofResponse.Body.Close()
	observer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if frame, err := observer.readFrame(); err == nil {
		t.Fatalf("readiness request produced event %q", frame)
	}
}

func TestRequestLogsCaptureFinalInformationalAndProxyErrorStatuses(t *testing.T) {
	t.Run("informational then final", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer backend.Close()
		settings := Settings{Token: strings.Repeat("b", 64)}
		daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		defer daemon.Close()
		registered, owner, err := sendControl(daemon.socketPath, controlRequest{
			Token: settings.Token, Type: controlRegister, Subdomain: "hints", Port: serverPort(backend),
		})
		if err != nil || !registered.OK {
			t.Fatalf("register: %+v, %v", registered, err)
		}
		defer owner.Close()
		observer := observeRegisteredRoute(t, daemon, settings, registered, "hints")
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/final", daemon.RouterPort), nil)
		request.Host = "hints.localhost"
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d", response.StatusCode)
		}
		if event := readRequestLogEvent(t, observer); event.Status != http.StatusNoContent || event.Path != "/final" {
			t.Fatalf("event = %+v", event)
		}
		observer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if frame, err := observer.readFrame(); err == nil {
			t.Fatalf("informational response produced extra event %q", frame)
		}
	})

	t.Run("unavailable backend", func(t *testing.T) {
		unused, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := unused.Addr().(*net.TCPAddr).Port
		unused.Close()
		settings := Settings{Token: strings.Repeat("c", 64)}
		daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		defer daemon.Close()
		registered, owner, err := sendControl(daemon.socketPath, controlRequest{
			Token: settings.Token, Type: controlRegister, Subdomain: "missing", Port: port,
		})
		if err != nil || !registered.OK {
			t.Fatalf("register: %+v, %v", registered, err)
		}
		defer owner.Close()
		observer := observeRegisteredRoute(t, daemon, settings, registered, "missing")
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/down", daemon.RouterPort), nil)
		request.Host = "missing.localhost"
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadGateway || string(body) != "The local service is not responding.\n" {
			t.Fatalf("proxy response = %d %q", response.StatusCode, body)
		}
		if event := readRequestLogEvent(t, observer); event.Status != http.StatusBadGateway {
			t.Fatalf("event = %+v", event)
		}
	})

	t.Run("aborted upstream body", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			connection, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = fmt.Fprint(buffered, "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\nshort")
			_ = buffered.Flush()
			_ = connection.Close()
		}))
		defer backend.Close()
		settings := Settings{Token: strings.Repeat("0", 64)}
		daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		defer daemon.Close()
		registered, owner, err := sendControl(daemon.socketPath, controlRequest{
			Token: settings.Token, Type: controlRegister, Subdomain: "aborted", Port: serverPort(backend),
		})
		if err != nil || !registered.OK {
			t.Fatalf("register: %+v, %v", registered, err)
		}
		defer owner.Close()
		observer := observeRegisteredRoute(t, daemon, settings, registered, "aborted")
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/partial", daemon.RouterPort), nil)
		request.Host = "aborted.localhost"
		response, requestErr := http.DefaultClient.Do(request)
		readErr := requestErr
		if response != nil {
			_, readErr = io.ReadAll(response.Body)
			response.Body.Close()
		}
		if readErr == nil {
			t.Fatal("truncated upstream body unexpectedly completed")
		}
		if event := readRequestLogEvent(t, observer); event.Status != http.StatusOK || event.Path != "/partial" {
			t.Fatalf("aborted response event = %+v", event)
		}
	})
}

func TestRequestLogsFailedUpgradeFinishesAsBadGateway(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = fmt.Fprint(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
		_ = buffered.Flush()
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("7", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	registered, owner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "upgrade", Port: serverPort(backend),
	})
	if err != nil || !registered.OK {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	defer owner.Close()
	observer := observeRegisteredRoute(t, daemon, settings, registered, "upgrade")
	client, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", daemon.RouterPort))
	if err != nil {
		t.Fatal(err)
	}
	client.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprint(client, "GET /broken HTTP/1.1\r\nHost: upgrade.localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	status, err := bufio.NewReader(client).ReadString('\n')
	client.Close()
	if err != nil || !strings.Contains(status, "502") {
		t.Fatalf("failed upgrade response = %q, %v", status, err)
	}
	if event := readRequestLogEvent(t, observer); event.Status != http.StatusBadGateway {
		t.Fatalf("failed upgrade event = %+v", event)
	}
}

func TestRequestLogsPreserveSSEFlushAndCompleteAfterStream(t *testing.T) {
	flushed := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(flushed)
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("d", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	registered, owner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "events", Port: serverPort(backend),
	})
	if err != nil || !registered.OK {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	defer owner.Close()
	observer := observeRegisteredRoute(t, daemon, settings, registered, "events")
	releaseStream := sync.OnceFunc(func() { close(release) })
	defer releaseStream()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/stream", daemon.RouterPort), nil)
	request.Host = "events.localhost"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "data: first\n\n" {
		t.Fatalf("first SSE event = %q, %v", first, err)
	}
	select {
	case <-flushed:
	case <-time.After(time.Second):
		t.Fatal("backend did not flush the first SSE event")
	}
	heldSince := time.Now()
	observer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if frame, err := observer.readFrame(); err == nil {
		t.Fatalf("SSE completed before stream end: %q", frame)
	} else {
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("waiting for SSE completion: %v", err)
		}
	}
	observer.SetReadDeadline(time.Time{})
	heldFor := time.Since(heldSince)
	releaseStream()
	rest, err := io.ReadAll(response.Body)
	if err != nil || string(rest) != "data: second\n\n" {
		t.Fatalf("remaining SSE data = %q, %v", rest, err)
	}
	event := readRequestLogEvent(t, observer)
	if event.Status != http.StatusOK || event.Path != "/stream" {
		t.Fatalf("event = %+v", event)
	}
	if event.DurationMicros < heldFor.Microseconds() {
		t.Fatalf("SSE duration = %d us, shorter than its %v release barrier", event.DurationMicros, heldFor)
	}
	observer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if frame, err := observer.readFrame(); err == nil {
		t.Fatalf("SSE emitted extra completion %q", frame)
	}
}

func TestRequestLogFramesBoundCopiedMetadataWhileForwardingFullPath(t *testing.T) {
	longPath := "/" + strings.Repeat("segment", 400)
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.EscapedPath() + "?" + r.URL.RawQuery
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("e", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	registered, owner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "large", Port: serverPort(backend),
	})
	if err != nil || !registered.OK {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	defer owner.Close()
	observer := observeRegisteredRoute(t, daemon, settings, registered, "large")
	request, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d%s?secret=hidden", daemon.RouterPort, longPath), nil)
	request.Host = "large.localhost"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := <-seen; got != longPath+"?secret=hidden" {
		t.Fatalf("backend target was truncated: length=%d", len(got))
	}
	observer.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := observer.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) > controlFrameLimit || strings.Contains(string(frame), "hidden") {
		t.Fatalf("unsafe frame length=%d: %s", len(frame), frame)
	}
	var event requestLogEvent
	if err := json.Unmarshal(frame, &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Path) >= len(longPath) || !strings.HasSuffix(event.Path, "...") {
		t.Fatalf("event path was not compacted: length=%d", len(event.Path))
	}
	metadata := newRequestLogMetadata("route", "GE\x1bT", "/line\nnext")
	controlFrame := marshalRequestLogEvent(requestLogEvent{Type: requestLogEventType, Route: metadata.route, Method: metadata.method, Path: metadata.path})
	if bytes.Contains(controlFrame, []byte{0x1b}) || bytes.Contains(controlFrame, []byte{'\n', 'n', 'e', 'x', 't'}) ||
		!strings.Contains(string(controlFrame), `GE\\x1bT`) || !strings.Contains(string(controlFrame), `line\\x0anext`) {
		t.Fatalf("control metadata was not escaped: %q", controlFrame)
	}
}

func TestRequestLogObserverReportsDropsWhenDeliveryResumes(t *testing.T) {
	server, client := net.Pipe()
	observer := newRequestLogObserver(server, "burst")
	done := make(chan struct{})
	go func() {
		observer.writeEvents()
		close(done)
	}()
	frame := marshalRequestLogEvent(requestLogEvent{Type: requestLogEventType, Route: "burst", Method: "GET", Path: "/"})
	started := time.Now()
	for index := 0; index < requestLogQueueCapacity*4; index++ {
		observer.publish(frame)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("non-reading observer blocked publishers for %v", elapsed)
	}
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(client)
	if _, err := readControlFrame(reader); err != nil {
		t.Fatal(err)
	}
	droppedFrame, err := readControlFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var dropped requestLogEvent
	if json.Unmarshal(droppedFrame, &dropped) != nil || dropped.Type != requestLogDroppedType || dropped.Dropped == 0 {
		t.Fatalf("drop event after resume = %q", droppedFrame)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observer writer did not stop")
	}
}

func TestStalledObserverClosesWithoutSlowingRequestsOrRemovingOwner(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("8", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	registered, owner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "stall", Port: serverPort(backend),
	})
	if err != nil || !registered.OK {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	defer owner.Close()
	target, found := daemon.routeFor("stall.localhost")
	if !found {
		t.Fatal("registered route was not active")
	}
	server, client := net.Pipe()
	observer := newRequestLogObserver(server, "stall")
	target.addObserver(observer)
	writerDone := make(chan struct{})
	go func() {
		observer.writeEvents()
		close(writerDone)
	}()
	defer client.Close()

	requestURL := fmt.Sprintf("http://127.0.0.1:%d/%s", daemon.RouterPort, strings.Repeat("x", 1600))
	started := time.Now()
	for index := 0; index < requestLogQueueCapacity*2; index++ {
		request, _ := http.NewRequest(http.MethodGet, requestURL, nil)
		request.Host = "stall.localhost"
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response.Body.Close()
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("stalled observer delayed HTTP responses for %v", elapsed)
	}
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled observer socket was not closed")
	}
	target.removeObserver(observer)
	request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/still-owned", daemon.RouterPort), nil)
	request.Host = "stall.localhost"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("owner route after observer stall = %d", response.StatusCode)
	}
}

type blockingCaptureWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	buffer  bytes.Buffer
}

func (w *blockingCaptureWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(data)
}

func (w *blockingCaptureWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func TestBlockedTerminalLogFloodLeavesStartupAndCancellationResponsive(t *testing.T) {
	writer := &blockingCaptureWriter{started: make(chan struct{}), release: make(chan struct{})}
	output := newRouteOutput(writer, 1)
	output.enqueue("Local route registered")
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not block")
	}
	display := newRequestLogDisplay(context.Background(), output)
	for index := 0; index < requestLogQueueCapacity*8; index++ {
		display.enqueue(fmt.Sprintf("[app] GET /%d 200 1ms", index))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		output.mu.RLock()
		dropped := output.logDrops
		output.mu.RUnlock()
		if dropped > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log burst did not reach bounded queue: dropped=%d", dropped)
		}
		time.Sleep(time.Millisecond)
	}
	display.Close()
	startupQueued := make(chan struct{})
	go func() {
		output.enqueue("Checking public reachability")
		output.enqueue("Press Ctrl+C to stop")
		close(startupQueued)
	}()
	select {
	case <-startupQueued:
	case <-time.After(time.Second):
		t.Fatal("log flood blocked startup messages")
	}
	started := time.Now()
	output.closeAndWait()
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("blocked terminal delayed cancellation for %v", elapsed)
	}
	close(writer.release)
	select {
	case <-output.done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not drain after release")
	}
	text := writer.String()
	registration := strings.Index(text, "Local route registered")
	checking := strings.Index(text, "Checking public reachability")
	stop := strings.Index(text, "Press Ctrl+C to stop")
	logs := strings.Index(text, "[app]")
	if registration < 0 || checking < registration || stop < checking || logs < stop {
		t.Fatalf("serialized output order = %q", text)
	}
	if !strings.Contains(text, "Warning: dropped ") {
		t.Fatalf("resumed terminal did not report drops: %q", text)
	}
}

func TestBlockedTerminalLoggingKeepsProxyAndOwnershipResponsive(t *testing.T) {
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("6", 64), RouterPort: defaultRouterPort}
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
	writer := &blockingCaptureWriter{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runRouteSessions(ctx, []routeSpec{{Subdomain: "blocked", Port: serverPort(backend)}},
			writer, newReadinessProbe(), false, startupOptions{Logs: true})
	}()
	select {
	case <-writer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer did not block")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		target, found := daemon.routeFor("blocked.localhost")
		if found && target.hasObservers() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI did not attach its route observer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	requestURL := fmt.Sprintf("http://127.0.0.1:%d/%s", daemon.RouterPort, strings.Repeat("z", 1600))
	started := time.Now()
	for index := 0; index < requestLogQueueCapacity*2; index++ {
		request, _ := http.NewRequest(http.MethodGet, requestURL, nil)
		request.Host = "blocked.localhost"
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("response %d status = %d", index, response.StatusCode)
		}
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("blocked terminal delayed proxy traffic for %v", elapsed)
	}
	if _, found := daemon.routeFor("blocked.localhost"); !found {
		t.Fatal("blocked terminal removed the owned route")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked terminal delayed runner cancellation")
	}
	close(writer.release)
}

func TestRequestLogObservationRequiresAuthAndExactInstance(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("f", 64)}
	daemon, err := startDaemon(settings, socketTestDir(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	registered, owner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "bound", Port: serverPort(backend),
	})
	if err != nil || !registered.OK {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	for name, request := range map[string]controlRequest{
		"wrong token":    {Token: "wrong", Type: controlObserve, Subdomain: "bound", RouteID: registered.RouteID},
		"wrong instance": {Token: settings.Token, Type: controlObserve, Subdomain: "bound", RouteID: "wrong"},
	} {
		t.Run(name, func(t *testing.T) {
			response, connection, err := sendControl(daemon.socketPath, request)
			if err != nil || response.OK {
				t.Fatalf("response = %+v, %v", response, err)
			}
			connection.Close()
		})
	}
	observer := observeRegisteredRoute(t, daemon, settings, registered, "bound")
	owner.Close()
	observer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := observer.readFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("observer after route removal: want EOF, got %v", err)
	}

	var replacement controlResponse
	var replacementOwner *controlConnection
	deadline := time.Now().Add(2 * time.Second)
	for {
		replacement, replacementOwner, err = sendControl(daemon.socketPath, controlRequest{
			Token: settings.Token, Type: controlRegister, Subdomain: "bound", Port: serverPort(backend),
		})
		if err == nil && replacement.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement register: %+v, %v", replacement, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer replacementOwner.Close()
	if replacement.RouteID == registered.RouteID {
		t.Fatal("replacement reused route instance id")
	}
	stale, staleConnection, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlObserve, Subdomain: "bound", RouteID: registered.RouteID,
	})
	if err != nil || stale.OK {
		t.Fatalf("stale observer = %+v, %v", stale, err)
	}
	staleConnection.Close()
}

func TestCLIRequestLogsIncludeOnlyOwnedRoutes(t *testing.T) {
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	settings := Settings{Token: strings.Repeat("9", 64), RouterPort: defaultRouterPort}
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
	unrelated, unrelatedOwner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "other", Port: serverPort(backend),
	})
	if err != nil || !unrelated.OK {
		t.Fatalf("unrelated register: %+v, %v", unrelated, err)
	}
	defer unrelatedOwner.Close()

	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan string, 64)
	done := make(chan error, 1)
	go func() {
		done <- runRouteSessions(ctx, []routeSpec{{Subdomain: "owned", Port: serverPort(backend)}},
			signalingWriter{lines: lines}, newReadinessProbe(), false, startupOptions{Logs: true})
	}()
	var output strings.Builder
	deadline := time.After(3 * time.Second)
	for !strings.Contains(output.String(), "Press Ctrl+C to stop") {
		select {
		case line := <-lines:
			output.WriteString(line)
		case <-deadline:
			t.Fatalf("runner did not start logging: %q", output.String())
		}
	}
	for _, route := range []string{"owned", "other"} {
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/%s", daemon.RouterPort, route), nil)
		request.Host = route + ".localhost"
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response.Body.Close()
	}
	deadline = time.After(2 * time.Second)
	for !strings.Contains(output.String(), "[owned] GET /owned 200") {
		select {
		case line := <-lines:
			output.WriteString(line)
		case <-deadline:
			t.Fatalf("owned request was not displayed: %q", output.String())
		}
	}
	time.Sleep(150 * time.Millisecond)
	for {
		select {
		case line := <-lines:
			output.WriteString(line)
		default:
			goto drained
		}
	}
drained:
	if strings.Contains(output.String(), "[other]") {
		t.Fatalf("unrelated request leaked into invocation logs: %q", output.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("logging runner did not stop")
	}
}
