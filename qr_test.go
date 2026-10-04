package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestQRCodeRetainsQuietZoneAtShortAndMaximumSupportedRouteURLs(t *testing.T) {
	short := "https://app.example.com"
	maximumDomain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	maximum := "https://" + strings.Repeat("s", 63) + "." + maximumDomain
	for _, address := range []string{short, maximum} {
		code, err := encodeQRCode(address, false)
		if err != nil {
			t.Fatalf("encode length %d: %v", len(address), err)
		}
		if strings.Contains(code, "\x1b[") {
			t.Fatalf("nonterminal QR contains terminal control sequences")
		}
		lines := strings.Split(strings.TrimSuffix(code, "\n"), "\n")
		if len(lines) < 4 || strings.TrimSpace(lines[0]) != "" || strings.TrimSpace(lines[1]) != "" {
			t.Fatalf("top quiet zone missing for length %d", len(address))
		}
		if strings.TrimSpace(lines[len(lines)-1]) != "" || strings.TrimSpace(lines[len(lines)-2]) != "" {
			t.Fatalf("bottom quiet zone missing for length %d", len(address))
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "    ") || !strings.HasSuffix(line, "    ") {
				t.Fatalf("horizontal quiet zone missing for length %d: %q", len(address), line)
			}
		}
	}
	tooLong := "https://example.com/" + strings.Repeat("a", 3000)
	if _, err := encodeQRCode(tooLong, false); err == nil {
		t.Fatal("encoded content beyond medium error-correction capacity")
	}
}

func TestTerminalQRCodeUsesExplicitContrastAndRestoresColors(t *testing.T) {
	code, err := encodeQRCode("https://app.example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "\x1b[30;47m") || !strings.HasSuffix(code, "\x1b[0m") {
		t.Fatalf("terminal QR colors = %q...%q", code[:10], code[len(code)-10:])
	}
}

type recordingWriter struct {
	mu     sync.Mutex
	writes []string
}

func (w *recordingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, string(data))
	w.mu.Unlock()
	return len(data), nil
}

func TestQRCodeBlockIsSerializedAsOneOutputWrite(t *testing.T) {
	writer := &recordingWriter{}
	output := newRouteOutput(writer, 1)
	sharing := sharingActions{
		output:  output,
		helpers: nativeHelpers{output: output},
		encode: func(address string, terminal bool) (string, error) {
			return "quiet\nQR for " + address + "\nquiet\n", nil
		},
	}
	route := newSharingRoute("app", "https://app.example.com", "proof")
	sharing.afterRegistration(Settings{Domain: "example.com"}, []routeSession{route}, startupOptions{QR: true})
	output.closeAndWait()
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.writes) != 1 || !strings.Contains(writer.writes[0], "public confirmation pending") || !strings.Contains(writer.writes[0], route.URL()) {
		t.Fatalf("writes = %#v", writer.writes)
	}
}

func TestLocalQRCodeWarningAndEncoderFailurePreserveURLs(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings Settings
		want     string
	}{
		{name: "local limitation", want: "cannot be reached from a phone"},
		{name: "encoder failure", settings: Settings{Domain: "example.com"}, want: "cannot encode QR code"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			output := newRouteOutput(&buffer, 1)
			sharing := sharingActions{
				output:  output,
				helpers: nativeHelpers{output: output},
				encode:  func(string, bool) (string, error) { return "", errors.New("encoder failed") },
			}
			route := newSharingRoute("app", "http://app.localhost:8080", "proof")
			sharing.afterRegistration(test.settings, []routeSession{route}, startupOptions{QR: true})
			output.closeAndWait()
			if !strings.Contains(buffer.String(), test.want) {
				t.Fatalf("output = %q", buffer.String())
			}
			if strings.Count(buffer.String(), route.URL()) == 0 {
				t.Fatalf("URL missing after QR error: %q", buffer.String())
			}
		})
	}
}
