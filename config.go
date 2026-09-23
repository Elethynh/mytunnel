package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

type Settings struct {
	Token       string `json:"token"`
	RouterPort  int    `json:"routerPort"`
	Domain      string `json:"domain,omitempty"`
	Tunnel      string `json:"tunnel,omitempty"`
	Credentials string `json:"credentials,omitempty"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var tunnelUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const defaultRouterPort = 43187

func normalizeSubdomain(value string) (string, error) {
	value = strings.ToLower(value)
	if !dnsLabel.MatchString(value) {
		return "", errors.New("subdomena musi być pojedynczą etykietą DNS (litery, cyfry, myślniki)")
	}
	return value, nil
}

func normalizeDomain(value string) (string, error) {
	value = strings.ToLower(value)
	if len(value) > 253 || strings.ContainsAny(value, " \t\n\r/:*") {
		return "", errors.New("niepoprawna domena")
	}
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return "", errors.New("niepoprawna domena")
	}
	for _, part := range parts {
		if !dnsLabel.MatchString(part) {
			return "", errors.New("niepoprawna domena; użyj nazwy ASCII lub punycode")
		}
	}
	return value, nil
}

func parsePort(value string) (int, error) {
	if value == "" {
		return 0, errors.New("port musi być liczbą od 1 do 65535")
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, errors.New("port musi być liczbą od 1 do 65535")
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("port musi być liczbą od 1 do 65535")
	}
	return port, nil
}

func settingsDir() (string, error) {
	if home := os.Getenv("MYTUNNEL_HOME"); home != "" {
		return filepath.Abs(home)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mytunnel"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mytunnel"), nil
}

func loadSettings() (Settings, string, error) {
	dir, err := settingsDir()
	if err != nil {
		return Settings{}, "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Settings{}, "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return Settings{}, "", err
	}
	file := filepath.Join(dir, "settings.json")
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return Settings{}, "", err
		}
		initial := Settings{Token: hex.EncodeToString(secret), RouterPort: defaultRouterPort}
		initialData, _ := json.MarshalIndent(initial, "", "  ")
		var created *os.File
		created, err = os.CreateTemp(dir, "settings-init-*.tmp")
		if err != nil {
			return Settings{}, "", err
		}
		defer os.Remove(created.Name())
		if err = created.Chmod(0600); err == nil {
			_, err = created.Write(append(initialData, '\n'))
		}
		closeErr := created.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Link(created.Name(), file)
		}
		if err != nil && !errors.Is(err, os.ErrExist) {
			return Settings{}, "", err
		}
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return Settings{}, "", err
	}
	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return Settings{}, "", err
	}
	_, tokenErr := hex.DecodeString(settings.Token)
	if len(settings.Token) != 64 || tokenErr != nil || settings.RouterPort < 1 || settings.RouterPort > 65535 {
		return Settings{}, "", errors.New("uszkodzony plik ustawień")
	}
	if settings.Domain != "" {
		settings.Domain, err = normalizeDomain(settings.Domain)
		if err != nil {
			return Settings{}, "", err
		}
	}
	return settings, dir, nil
}

func lockSettingsDir() (*os.File, error) {
	dir, err := settingsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "settings.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func saveSettings(settings Settings, dir string) error {
	file, err := os.CreateTemp(dir, "settings-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), filepath.Join(dir, "settings.json"))
}

func renderCloudflaredConfig(settings Settings) (string, error) {
	domain, err := normalizeDomain(settings.Domain)
	if err != nil {
		return "", err
	}
	if !tunnelUUID.MatchString(settings.Tunnel) {
		return "", errors.New("tunel musi mieć poprawny UUID")
	}
	if !filepath.IsAbs(settings.Credentials) {
		return "", errors.New("ścieżka credentials musi być absolutna")
	}
	if settings.RouterPort < 1 || settings.RouterPort > 65535 {
		return "", errors.New("niepoprawny port routera")
	}
	return fmt.Sprintf("tunnel: %s\ncredentials-file: %s\ningress:\n  - hostname: %s\n    service: %s\n  - service: http_status:404\n",
		strconv.Quote(settings.Tunnel), strconv.Quote(settings.Credentials),
		strconv.Quote("*."+domain), strconv.Quote(fmt.Sprintf("http://127.0.0.1:%d", settings.RouterPort))), nil
}
