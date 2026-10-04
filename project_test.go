package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadProjectRoutesValidatesStrictSchema(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []routeSpec
	}{
		{
			name: "ordered routes with shared backend port",
			json: `{"version":1,"routes":[{"port":5173,"subdomain":"Local-App"},{"port":5173,"subdomain":"api"}]}`,
			want: []routeSpec{{Port: 5173, Subdomain: "local-app"}, {Port: 5173, Subdomain: "api"}},
		},
		{
			name: "standard repeated key semantics",
			json: `{"version":2,"version":1,"routes":[{"port":1,"port":8080,"subdomain":"api"}]}`,
			want: []routeSpec{{Port: 8080, Subdomain: "api"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "project.json")
			if err := os.WriteFile(path, []byte(test.json), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := loadProjectRoutes(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("routes = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestLoadProjectRoutesRejectsInvalidProjectBeforeSettingsAccess(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		wantRoute string
	}{
		{name: "missing version", json: `{"routes":[{"port":5173,"subdomain":"app"}]}`},
		{name: "unsupported version", json: `{"version":2,"routes":[{"port":5173,"subdomain":"app"}]}`},
		{name: "missing routes", json: `{"version":1}`},
		{name: "empty routes", json: `{"version":1,"routes":[]}`},
		{name: "missing port", json: `{"version":1,"routes":[{"subdomain":"app"}]}`, wantRoute: "route 1"},
		{name: "zero port", json: `{"version":1,"routes":[{"port":0,"subdomain":"app"}]}`, wantRoute: "route 1"},
		{name: "large port", json: `{"version":1,"routes":[{"port":65536,"subdomain":"app"}]}`, wantRoute: "route 1"},
		{name: "noninteger port", json: `{"version":1,"routes":[{"port":5173.5,"subdomain":"app"}]}`, wantRoute: "route 1"},
		{name: "missing subdomain", json: `{"version":1,"routes":[{"port":5173}]}`, wantRoute: "route 1"},
		{name: "invalid subdomain", json: `{"version":1,"routes":[{"port":5173,"subdomain":"app.example"}]}`, wantRoute: "route 1"},
		{name: "normalized duplicate", json: `{"version":1,"routes":[{"port":5173,"subdomain":"APP"},{"port":8080,"subdomain":"app"}]}`, wantRoute: "route 2"},
		{name: "unknown project field", json: `{"version":1,"routes":[{"port":5173,"subdomain":"app"}],"domain":"example.com"}`},
		{name: "unknown route field", json: `{"version":1,"routes":[{"port":5173,"subdomain":"app","open":true}]}`, wantRoute: "route 1"},
		{name: "trailing JSON", json: `{"version":1,"routes":[{"port":5173,"subdomain":"app"}]} {}`},
		{name: "trailing null", json: `{"version":1,"routes":[{"port":5173,"subdomain":"app"}]} null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			settingsDir := filepath.Join(root, "settings")
			t.Setenv("MYTUNNEL_HOME", settingsDir)
			path := filepath.Join(root, "project.json")
			if err := os.WriteFile(path, []byte(test.json), 0600); err != nil {
				t.Fatal(err)
			}

			err := runProject(context.Background(), []string{"--config", path}, &bytes.Buffer{}, newReadinessProbe())
			if err == nil {
				t.Fatal("accepted invalid project")
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error lacks project path: %v", err)
			}
			if test.wantRoute != "" && !strings.Contains(err.Error(), test.wantRoute) {
				t.Fatalf("error lacks route context %q: %v", test.wantRoute, err)
			}
			if _, statErr := os.Stat(settingsDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("settings accessed before validation: %v", statErr)
			}
		})
	}
}

func TestProjectConfigSelectionUsesCurrentDirectoryWithoutChangingIt(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	cwd := t.TempDir()
	other := t.TempDir()
	writeProject := func(path, name string) {
		t.Helper()
		data := `{"version":1,"routes":[{"port":5173,"subdomain":"` + name + `"}]}`
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProject(filepath.Join(cwd, ".mytunnel.json"), "default")
	writeProject(filepath.Join(cwd, "relative.json"), "relative")
	absolute := filepath.Join(other, "absolute.json")
	writeProject(absolute, "absolute")
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		path string
		name string
	}{{name: "default"}, {path: "relative.json", name: "relative"}, {path: absolute, name: "absolute"}} {
		routes, err := loadProjectRoutes(selectedProjectPath(test.path))
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) != 1 || routes[0].Subdomain != test.name {
			t.Fatalf("config %q routes = %#v", test.path, routes)
		}
		if current, err := os.Getwd(); err != nil || current != cwd {
			t.Fatalf("working directory changed to %q, %v", current, err)
		}
	}
}

func TestRunDispatchesProjectDefaultBeforeSettingsAccess(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	cwd := t.TempDir()
	settingsDir := filepath.Join(t.TempDir(), "settings")
	t.Setenv("MYTUNNEL_HOME", settingsDir)
	if err := os.WriteFile(filepath.Join(cwd, defaultProjectFile), []byte(`{"version":1,"routes":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}

	err = run([]string{"up"})
	if err == nil || !strings.Contains(err.Error(), defaultProjectFile) {
		t.Fatalf("up error = %v", err)
	}
	if _, statErr := os.Stat(settingsDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("settings accessed before default project validation: %v", statErr)
	}
}

func TestProjectStartsTwoRoutesAndCancellationPreservesUnrelatedRoute(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, "shared:"+request.Host)
	}))
	defer backend.Close()
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	settings := Settings{Token: strings.Repeat("a", 64), RouterPort: defaultRouterPort}
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
		Token: settings.Token, Type: controlRegister, Subdomain: "unrelated", Port: 9000,
	})
	if err != nil || !unrelated.OK {
		t.Fatalf("unrelated route = %+v, %v", unrelated, err)
	}
	defer unrelatedOwner.Close()
	projectPath := filepath.Join(t.TempDir(), "project.json")
	projectJSON := fmt.Sprintf(`{"version":1,"routes":[{"port":%d,"subdomain":"app"},{"port":%d,"subdomain":"api"}]}`, serverPort(backend), serverPort(backend))
	if err := os.WriteFile(projectPath, []byte(projectJSON), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := signalingWriter{lines: make(chan string, 8)}
	done := make(chan error, 1)
	go func() {
		done <- runProject(ctx, []string{"--config", projectPath}, output, newReadinessProbe())
	}()
	waitForDaemonRoutes(t, daemon, "app", "api", "unrelated")
	var outputLines []string
	startupComplete := false
	for !startupComplete {
		select {
		case line := <-output.lines:
			outputLines = append(outputLines, line)
			startupComplete = strings.Contains(line, "Project startup complete: all routes registered.")
		case <-time.After(time.Second):
			t.Fatal("project startup did not complete")
		}
	}
	for _, name := range []string{"app", "api"} {
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", daemon.RouterPort), nil)
		request.Host = name + ".localhost"
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "shared:"+request.Host {
			t.Fatalf("%s route = %d %q, %v", name, response.StatusCode, body, readErr)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("project did not stop after cancellation")
	}
	waitForDaemonRoutes(t, daemon, "unrelated")
	for {
		select {
		case line := <-output.lines:
			outputLines = append(outputLines, line)
		default:
			goto outputDrained
		}
	}

outputDrained:
	text := strings.Join(outputLines, "")
	if !strings.Contains(text, "Project route registered (startup pending, local): http://app.localhost:") ||
		!strings.Contains(text, "Project route registered (startup pending, local): http://api.localhost:") ||
		!strings.Contains(text, "Project startup complete: all routes registered.") {
		t.Fatalf("project output = %q", text)
	}
}

func TestProjectCollisionRollsBackNewRoutesAndKeepsExistingOwner(t *testing.T) {
	dir := socketTestDir(t)
	t.Setenv("MYTUNNEL_HOME", dir)
	settings := Settings{Token: strings.Repeat("b", 64), RouterPort: defaultRouterPort}
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
	existing, existingOwner, err := sendControl(daemon.socketPath, controlRequest{
		Token: settings.Token, Type: controlRegister, Subdomain: "taken", Port: 9000,
	})
	if err != nil || !existing.OK {
		t.Fatalf("existing route = %+v, %v", existing, err)
	}
	defer existingOwner.Close()
	projectPath := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(projectPath, []byte(`{"version":1,"routes":[{"port":5173,"subdomain":"first"},{"port":8080,"subdomain":"taken"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	var output bytes.Buffer
	err = runProject(context.Background(), []string{"--config", projectPath, "--open", "--copy", "--qr"}, &output, newReadinessProbe())
	if err == nil || !strings.Contains(err.Error(), `route "taken"`) || !strings.Contains(err.Error(), "displayed project group was rolled back") {
		t.Fatalf("project error = %v", err)
	}
	waitForDaemonRoutes(t, daemon, "taken")
	if !strings.Contains(output.String(), "Project route registered (startup pending, local): http://first.localhost:") {
		t.Fatalf("rollback output = %q", output.String())
	}
	if strings.Contains(output.String(), "Copying registered URLs") || strings.Contains(output.String(), "QR code") || strings.Contains(output.String(), "phone") {
		t.Fatalf("sharing ran for a rolled-back group: %q", output.String())
	}
}

func waitForDaemonRoutes(t *testing.T, daemon *Daemon, names ...string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		daemon.mu.RLock()
		got := make([]string, 0, len(daemon.routes))
		for name := range daemon.routes {
			got = append(got, name)
		}
		daemon.mu.RUnlock()
		if len(got) == len(names) {
			allPresent := true
			for _, name := range names {
				found := slices.Contains(got, name)
				allPresent = allPresent && found
			}
			if allPresent {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon routes = %v, want %v", got, names)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
