package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type helperCall struct {
	Path  string   `json:"path"`
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

type helperProcessFunc func() error

func (f helperProcessFunc) Wait() error { return f() }

type fakeCommandBoundary struct {
	mu        sync.Mutex
	paths     map[string]string
	calls     []helperCall
	processes []helperProcess
	startErr  error
	events    chan string
}

func (b *fakeCommandBoundary) LookPath(name string) (string, error) {
	if path := b.paths[name]; path != "" {
		return path, nil
	}
	return "", errors.New("not found")
}

func (b *fakeCommandBoundary) Start(path string, args []string, stdin io.Reader) (helperProcess, error) {
	if b.startErr != nil {
		return nil, b.startErr
	}
	var input string
	if stdin != nil {
		data, _ := io.ReadAll(stdin)
		input = string(data)
	}
	b.mu.Lock()
	b.calls = append(b.calls, helperCall{Path: path, Args: append([]string(nil), args...), Stdin: input})
	var process helperProcess = helperProcessFunc(func() error { return nil })
	if len(b.processes) > 0 {
		process = b.processes[0]
		b.processes = b.processes[1:]
	}
	b.mu.Unlock()
	if b.events != nil {
		b.events <- "helper:" + filepath.Base(path)
	}
	return process, nil
}

func (b *fakeCommandBoundary) Calls() []helperCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]helperCall(nil), b.calls...)
}

type sharingRoute struct {
	spec      routeSpec
	address   string
	readiness routeReadiness
	wait      chan error
	closed    chan struct{}
	once      sync.Once
}

func newSharingRoute(name, address, proof string) *sharingRoute {
	return &sharingRoute{
		spec: routeSpec{Subdomain: name, Port: 3000}, address: address,
		readiness: routeReadiness{Path: "/.mytunnel/readiness/" + name, Proof: proof},
		wait:      make(chan error, 1), closed: make(chan struct{}),
	}
}

func (r *sharingRoute) Spec() routeSpec           { return r.spec }
func (r *sharingRoute) URL() string               { return r.address }
func (r *sharingRoute) InstanceID() string        { return r.spec.Subdomain + "-id" }
func (r *sharingRoute) Readiness() routeReadiness { return r.readiness }
func (r *sharingRoute) Wait() error               { return <-r.wait }
func (r *sharingRoute) Close() error {
	r.once.Do(func() {
		close(r.closed)
		r.wait <- netClosedForSharingTest{}
	})
	return nil
}

type netClosedForSharingTest struct{}

func (netClosedForSharingTest) Error() string { return "closed" }

type eventWriter struct{ events chan string }

func (w eventWriter) Write(data []byte) (int, error) {
	w.events <- "output:" + string(data)
	return len(data), nil
}

func TestProjectSharingCopiesAndPrintsPendingURLsBeforeOpeningConfirmedRoutes(t *testing.T) {
	events := make(chan string, 32)
	commands := &fakeCommandBoundary{
		paths:  map[string]string{"pbcopy": "/usr/bin/pbcopy", "open": "/usr/bin/open"},
		events: events,
	}
	output := newRouteOutput(eventWriter{events: events}, 2)
	output.terminal = false
	sharing := sharingActions{
		output: output,
		helpers: nativeHelpers{
			goos: "darwin", getenv: func(string) string { return "" }, commands: commands,
			acknowledgement: 20 * time.Millisecond, output: output,
		},
		encode: func(address string, terminal bool) (string, error) {
			if terminal {
				t.Fatal("nonterminal writer was treated as a terminal")
			}
			return "QR:" + address + "\n", nil
		},
	}
	first := newSharingRoute("first", "https://first.example.com", "first-proof")
	second := newSharingRoute("second", "https://second.example.com", "second-proof")
	ctx, cancel := context.WithCancel(context.Background())
	group := newRouteSessionGroup(ctx)
	group.add(first)
	group.add(second)
	var firstProbe sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "first") {
			firstProbe.Do(func() { events <- "probe:first" })
			_, _ = io.WriteString(writer, "first-proof")
			return
		}
		_, _ = io.WriteString(writer, "wrong-proof")
	}))
	defer server.Close()
	first.address = server.URL + "/first-url"
	second.address = server.URL + "/second-url"
	probe := readinessProbe{
		client:         server.Client(),
		overallTimeout: 500 * time.Millisecond, requestTimeout: 250 * time.Millisecond, retryDelay: 10 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		done <- runAcquiredRouteSessions(Settings{Domain: "example.com"}, group, output, probe,
			startupOptions{Open: true, Copy: true, QR: true}, sharing)
	}()

	var observed []string
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			observed = append(observed, event)
			if strings.Contains(event, "public reachability was not confirmed") {
				cancel()
				goto stopped
			}
		case <-deadline:
			t.Fatal("sharing flow did not finish readiness")
		}
	}

stopped:
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	output.closeAndWait()
	calls := commands.Calls()
	if len(calls) != 2 {
		t.Fatalf("helper calls = %#v", calls)
	}
	wantPayload := first.URL() + "\n" + second.URL()
	if calls[0].Path != "/usr/bin/pbcopy" || calls[0].Stdin != wantPayload || len(calls[0].Args) != 0 {
		t.Fatalf("clipboard call = %#v, want payload %q", calls[0], wantPayload)
	}
	if calls[1].Path != "/usr/bin/open" || !reflect.DeepEqual(calls[1].Args, []string{first.URL()}) || calls[1].Stdin != "" {
		t.Fatalf("browser call = %#v", calls[1])
	}
	joined := strings.Join(observed, "\n")
	qrFirst := strings.Index(joined, "QR:"+first.URL())
	qrSecond := strings.Index(joined, "QR:"+second.URL())
	confirmed := strings.Index(joined, "Public reachability confirmed: "+first.URL())
	proof := strings.Index(joined, "probe:first")
	opened := strings.Index(joined, "helper:open")
	if qrFirst < 0 || qrSecond < qrFirst || confirmed < qrSecond || proof < 0 || opened < proof {
		t.Fatalf("event order = %q", joined)
	}
}

func TestNativeHelperSelectionAndWarnings(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		env       map[string]string
		copyPath  string
		copyArgs  []string
		openPath  string
		wantCalls int
		warning   string
	}{
		{name: "macOS", goos: "darwin", copyPath: "pbcopy", openPath: "open", wantCalls: 2},
		{name: "Wayland", goos: "linux", env: map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, copyPath: "wl-copy", openPath: "xdg-open", wantCalls: 2},
		{name: "X11", goos: "linux", env: map[string]string{"DISPLAY": ":1"}, copyPath: "xclip", copyArgs: []string{"-selection", "clipboard"}, openPath: "xdg-open", wantCalls: 2},
		{name: "headless Linux", goos: "linux", wantCalls: 0, warning: "graphical session"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			output := newRouteOutput(&buffer, 1)
			paths := map[string]string{}
			for _, name := range []string{"pbcopy", "open", "wl-copy", "xclip", "xdg-open"} {
				paths[name] = "/helpers/" + name
			}
			commands := &fakeCommandBoundary{paths: paths}
			helpers := nativeHelpers{
				goos:     test.goos,
				getenv:   func(name string) string { return test.env[name] },
				commands: commands, acknowledgement: time.Second, output: output,
			}
			helpers.copy("https://literal.example/path?a=1&b=2")
			helpers.open("https://literal.example/path?a=1&b=2")
			waitForHelperCalls(t, commands, test.wantCalls)
			output.closeAndWait()
			calls := commands.Calls()
			if len(calls) != test.wantCalls {
				t.Fatalf("calls = %#v", calls)
			}
			if test.wantCalls > 0 {
				if filepath.Base(calls[0].Path) != test.copyPath || !reflect.DeepEqual(calls[0].Args, test.copyArgs) {
					t.Fatalf("copy call = %#v", calls[0])
				}
				if filepath.Base(calls[1].Path) != test.openPath || !reflect.DeepEqual(calls[1].Args, []string{"https://literal.example/path?a=1&b=2"}) {
					t.Fatalf("open call = %#v", calls[1])
				}
			}
			if test.warning != "" && !strings.Contains(buffer.String(), test.warning) {
				t.Fatalf("warning = %q", buffer.String())
			}
		})
	}
}

func TestLocalSharingCombinesClipboardBrowserAndQRCodeLimitation(t *testing.T) {
	var buffer bytes.Buffer
	output := newRouteOutput(&buffer, 1)
	commands := &fakeCommandBoundary{paths: map[string]string{
		"pbcopy": "/usr/bin/pbcopy",
		"open":   "/usr/bin/open",
	}}
	encoded := false
	sharing := sharingActions{
		output: output,
		helpers: nativeHelpers{
			goos: "darwin", getenv: func(string) string { return "" }, commands: commands,
			acknowledgement: time.Second, output: output,
		},
		encode: func(string, bool) (string, error) {
			encoded = true
			return "", nil
		},
	}
	route := newSharingRoute("local", "http://local.localhost:43187", "proof")
	sharing.afterRegistration(Settings{}, []routeSession{route}, startupOptions{Open: true, Copy: true, QR: true})
	waitForHelperCalls(t, commands, 2)
	output.closeAndWait()
	calls := commands.Calls()
	if len(calls) != 2 || calls[0].Path != "/usr/bin/pbcopy" || calls[0].Stdin != route.URL() ||
		calls[1].Path != "/usr/bin/open" || !reflect.DeepEqual(calls[1].Args, []string{route.URL()}) {
		t.Fatalf("local helper calls = %#v", calls)
	}
	if encoded {
		t.Fatal("local mode encoded a QR code")
	}
	if !strings.Contains(buffer.String(), "cannot be reached from a phone") || !strings.Contains(buffer.String(), route.URL()) {
		t.Fatalf("local sharing output = %q", buffer.String())
	}
}

func TestMissingAndFailedHelpersWarnWithoutReturningAnError(t *testing.T) {
	for _, test := range []struct {
		name     string
		commands *fakeCommandBoundary
		want     string
	}{
		{name: "missing", commands: &fakeCommandBoundary{paths: map[string]string{}}, want: "not available"},
		{name: "failed", commands: &fakeCommandBoundary{paths: map[string]string{"open": "/helpers/open"}, startErr: errors.New("boom")}, want: "boom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			output := newRouteOutput(&buffer, 1)
			helpers := nativeHelpers{
				goos: "darwin", getenv: func(string) string { return "" }, commands: test.commands,
				acknowledgement: time.Second, output: output,
			}
			helpers.open("https://app.example.com")
			output.closeAndWait()
			if !strings.Contains(buffer.String(), "Warning:") || !strings.Contains(buffer.String(), test.want) {
				t.Fatalf("warning = %q", buffer.String())
			}
		})
	}
}

func TestClipboardTimeoutDoesNotDelayReadinessOrCancellationAndProcessIsReaped(t *testing.T) {
	releaseProcess := make(chan struct{})
	waitStarted := make(chan struct{})
	waitReturned := make(chan struct{})
	var waitCalls atomic.Int32
	commands := &fakeCommandBoundary{
		paths: map[string]string{"pbcopy": "/helpers/pbcopy"},
		processes: []helperProcess{helperProcessFunc(func() error {
			waitCalls.Add(1)
			close(waitStarted)
			<-releaseProcess
			close(waitReturned)
			return nil
		})},
	}
	events := make(chan string, 32)
	output := newRouteOutput(eventWriter{events: events}, 1)
	sharing := sharingActions{output: output, helpers: nativeHelpers{
		goos: "darwin", getenv: func(string) string { return "" }, commands: commands,
		acknowledgement: time.Second, output: output,
	}, encode: encodeQRCode}
	var probeOnce sync.Once
	probeSeen := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		probeOnce.Do(func() { close(probeSeen) })
		_, _ = io.WriteString(writer, "proof")
	}))
	defer server.Close()
	route := newSharingRoute("app", server.URL, "proof")
	ctx, cancel := context.WithCancel(context.Background())
	group := newRouteSessionGroup(ctx)
	group.add(route)
	done := make(chan error, 1)
	go func() {
		done <- runAcquiredRouteSessions(Settings{Domain: "example.com"}, group, output, readinessProbe{
			client: server.Client(), overallTimeout: time.Second, requestTimeout: time.Second, retryDelay: time.Millisecond,
		}, startupOptions{Copy: true}, sharing)
	}()
	select {
	case <-waitStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("helper was not reaped asynchronously")
	}
	select {
	case <-probeSeen:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("clipboard acknowledgement delayed readiness")
	}
	confirmed := false
	timedOut := false
	deadline := time.After(3 * time.Second)
	for !confirmed || !timedOut {
		select {
		case event := <-events:
			confirmed = confirmed || strings.Contains(event, "Public reachability confirmed")
			timedOut = timedOut || strings.Contains(event, "did not acknowledge")
		case <-deadline:
			t.Fatalf("events did not show proof and helper timeout, confirmed=%t timeout=%t", confirmed, timedOut)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner cancellation: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("blocked clipboard helper delayed runner cancellation")
	}
	select {
	case <-waitReturned:
		t.Fatal("clipboard process ended before it was released")
	default:
	}
	if len(commands.Calls()) != 1 {
		t.Fatalf("timeout started a fallback: %#v", commands.Calls())
	}
	close(releaseProcess)
	select {
	case <-waitReturned:
	case <-time.After(time.Second):
		t.Fatal("clipboard process was not reaped after exit")
	}
	if waitCalls.Load() != 1 {
		t.Fatalf("Wait calls = %d", waitCalls.Load())
	}
	output.closeAndWait()
}

func TestExecCommandBoundaryPassesLiteralArgumentsAndStdin(t *testing.T) {
	if os.Getenv("MYTUNNEL_HELPER_FIXTURE") == "1" {
		helperFixtureProcess(t)
		return
	}
	resultPath := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("MYTUNNEL_HELPER_FIXTURE", "1")
	t.Setenv("MYTUNNEL_HELPER_RESULT", resultPath)
	literal := "https://example.com/$(touch nope);`echo nope`?a=one two&b='three'"
	process, err := (execCommandBoundary{}).Start(os.Args[0], []string{"-test.run=TestExecCommandBoundaryPassesLiteralArgumentsAndStdin", "--", literal}, strings.NewReader(literal))
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var got helperCall
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, []string{literal}) || got.Stdin != literal {
		t.Fatalf("fixture received %#v", got)
	}
}

func helperFixtureProcess(t *testing.T) {
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		t.Fatal("missing fixture argument separator")
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(helperCall{Args: os.Args[separator+1:], Stdin: string(input)})
	if err := os.WriteFile(os.Getenv("MYTUNNEL_HELPER_RESULT"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func waitForHelperCalls(t *testing.T, commands *fakeCommandBoundary, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(commands.Calls()) < count && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}
