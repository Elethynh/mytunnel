package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mytunnel:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Printf(`Usage:
  mytunnel <port> [--subdomain name]  Expose a local HTTP service
  mytunnel status                    Show active routes
  mytunnel --version                 Show the installed version
  mytunnel configure --domain domain --tunnel UUID --credentials file.json

Without Cloudflare configuration, routes use <name>.localhost:%d.
Each command keeps its route active until Ctrl+C.
`, defaultRouterPort)
		return nil
	}
	switch args[0] {
	case "--version", "-v":
		if len(args) != 1 {
			return errors.New("--version does not accept arguments")
		}
		fmt.Println("mytunnel", buildVersion())
		return nil
	case "daemon":
		if len(args) != 1 {
			return errors.New("daemon does not accept arguments")
		}
		return runDaemon()
	case "configure":
		return configure(args[1:])
	case "status":
		if len(args) != 1 {
			return errors.New("status does not accept arguments")
		}
		return status()
	default:
		return route(args)
	}
}

func parseOptions(args []string, allowed ...string) (map[string]string, error) {
	result := make(map[string]string)
	valid := make(map[string]bool)
	for _, name := range allowed {
		valid[name] = true
	}
	if len(args)%2 != 0 {
		return nil, fmt.Errorf("invalid argument: %s", args[len(args)-1])
	}
	for i := 0; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		if !valid[key] || value == "" || strings.HasPrefix(value, "--") || result[key] != "" {
			return nil, fmt.Errorf("invalid argument: %s", key)
		}
		result[key] = value
	}
	return result, nil
}

func probeStatus(settings Settings, dir string) (*controlResponse, error) {
	response, conn, err := sendControl(controlAddress(dir), controlRequest{Token: settings.Token, Type: controlStatus})
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	conn.Close()
	if !response.OK {
		return nil, errors.New(response.Error)
	}
	return &response, nil
}

func configure(args []string) error {
	values, err := parseOptions(args, "--domain", "--tunnel", "--credentials")
	if err != nil {
		return err
	}
	if values["--domain"] == "" || values["--tunnel"] == "" || values["--credentials"] == "" {
		return errors.New("provide --domain, --tunnel and --credentials")
	}
	lock, err := lockSettingsDir()
	if err != nil {
		return err
	}
	defer lock.Close()
	settings, dir, err := loadSettings()
	if err != nil {
		return err
	}
	for start := time.Now(); time.Since(start) < 6*time.Second; time.Sleep(100 * time.Millisecond) {
		current, err := probeStatus(settings, dir)
		if err != nil {
			return err
		}
		if current == nil {
			break
		}
		if len(current.Routes) > 0 {
			return errors.New("stop active mytunnel commands before changing the configuration")
		}
		if time.Since(start) >= 5*time.Second {
			return errors.New("the local daemon is still running; try again shortly")
		}
	}
	credentials, err := filepath.Abs(values["--credentials"])
	if err != nil {
		return err
	}
	if _, err := os.Stat(credentials); err != nil {
		return fmt.Errorf("credentials file not found: %w", err)
	}
	settings.Domain, err = normalizeDomain(values["--domain"])
	if err != nil {
		return err
	}
	settings.Tunnel = values["--tunnel"]
	settings.Credentials = credentials
	if _, err := renderCloudflaredConfig(settings); err != nil {
		return err
	}
	if err := saveSettings(settings, dir); err != nil {
		return err
	}
	fmt.Printf("Saved configuration for *.%s.\n", settings.Domain)
	fmt.Printf("Create a proxied CNAME in Cloudflare: * → %s.cfargotunnel.com\n", settings.Tunnel)
	return nil
}

func routeURL(name, domain string, routerPort int) string {
	if domain != "" {
		return fmt.Sprintf("https://%s.%s", name, domain)
	}
	return fmt.Sprintf("http://%s.localhost:%d", name, routerPort)
}

func status() error {
	settings, dir, err := loadSettings()
	if err != nil {
		return err
	}
	current, err := probeStatus(settings, dir)
	if err != nil {
		return err
	}
	if current == nil {
		fmt.Println("The local daemon is not running.")
		return nil
	}
	if len(current.Routes) == 0 {
		fmt.Println("No active routes.")
	}
	for _, route := range current.Routes {
		address := routeURL(route.Subdomain, current.Domain, settings.RouterPort)
		fmt.Printf("%s → 127.0.0.1:%d\n", address, route.Port)
	}
	return nil
}

func launchDaemon(dir string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	command := exec.Command(executable, "daemon")
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func register(settings Settings, dir, name string, port int) (controlResponse, net.Conn, error) {
	request := controlRequest{Token: settings.Token, Type: controlRegister, Subdomain: name, Port: port}
	address := controlAddress(dir)
	if settings.Domain != "" {
		if _, err := os.Stat(settings.Credentials); err != nil {
			return controlResponse{}, nil, fmt.Errorf("credentials file not found: %w", err)
		}
		if _, err := exec.LookPath("cloudflared"); err != nil {
			return controlResponse{}, nil, errors.New("install cloudflared and add it to PATH")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	var lastLaunch time.Time
	for time.Now().Before(deadline) {
		response, conn, err := sendControl(address, request)
		if err == nil {
			if !response.Retry {
				return response, conn, nil
			}
			conn.Close()
		} else if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			if time.Since(lastLaunch) >= time.Second {
				if err := launchDaemon(dir); err != nil {
					return controlResponse{}, nil, err
				}
				lastLaunch = time.Now()
			}
		} else {
			return controlResponse{}, nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return controlResponse{}, nil, fmt.Errorf("the daemon did not start; check %s", filepath.Join(dir, "daemon.log"))
}

func route(args []string) error {
	port, err := parsePort(args[0])
	if err != nil {
		return err
	}
	values, err := parseOptions(args[1:], "--subdomain")
	if err != nil {
		return err
	}
	name := values["--subdomain"]
	if name == "" {
		random := make([]byte, 4)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		name = hex.EncodeToString(random)
	}
	name, err = normalizeSubdomain(name)
	if err != nil {
		return err
	}
	lock, err := lockSettingsDir()
	if err != nil {
		return err
	}
	settings, dir, err := loadSettings()
	if err != nil {
		lock.Close()
		return err
	}
	response, conn, err := register(settings, dir, name, port)
	lock.Close()
	if err != nil {
		return err
	}
	defer conn.Close()
	if !response.OK {
		return errors.New(response.Error)
	}
	fmt.Printf("%s → 127.0.0.1:%d\n", response.URL, port)
	if settings.Domain != "" {
		fmt.Println("Cloudflare may need a moment to establish the connection.")
	}
	fmt.Println("Press Ctrl+C to stop this route.")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	disconnected := make(chan struct{}, 1)
	go func() { io.Copy(io.Discard, conn); disconnected <- struct{}{} }()
	select {
	case <-signals:
		return nil
	case <-disconnected:
		return errors.New("the connection to the local daemon was interrupted")
	}
}

func runDaemon() error {
	settings, dir, err := loadSettings()
	if err != nil {
		return err
	}
	daemon, err := startDaemon(settings, dir, 5*time.Second)
	if err != nil {
		return err
	}
	defer daemon.Close()
	var cloudflared *exec.Cmd
	var cloudflaredDone chan error
	if settings.Domain != "" {
		config, err := renderCloudflaredConfig(settings)
		if err != nil {
			return err
		}
		file, err := os.CreateTemp(dir, "cloudflared-*.yml")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := file.WriteString(config); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		cloudflared = exec.Command("cloudflared", "tunnel", "--config", file.Name(), "run", settings.Tunnel)
		cloudflared.Stdout = os.Stdout
		cloudflared.Stderr = os.Stderr
		if err := cloudflared.Start(); err != nil {
			return err
		}
		cloudflaredDone = make(chan error, 1)
		go func() { cloudflaredDone <- cloudflared.Wait() }()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	var result error
	select {
	case <-signals:
	case <-daemon.stopCh:
	case err := <-cloudflaredDone:
		if err != nil {
			result = fmt.Errorf("cloudflared exited: %w", err)
		} else {
			result = errors.New("cloudflared exited")
		}
		cloudflaredDone = nil
	}
	if cloudflaredDone != nil {
		cloudflared.Process.Signal(syscall.SIGTERM)
		select {
		case <-cloudflaredDone:
		case <-time.After(2 * time.Second):
			cloudflared.Process.Kill()
			<-cloudflaredDone
		}
	}
	return result
}
