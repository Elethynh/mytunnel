package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const helperAcknowledgementTimeout = 2 * time.Second

type helperProcess interface {
	Wait() error
}

type commandBoundary interface {
	LookPath(string) (string, error)
	Start(string, []string, io.Reader) (helperProcess, error)
}

type execCommandBoundary struct{}

func (execCommandBoundary) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (execCommandBoundary) Start(path string, args []string, stdin io.Reader) (helperProcess, error) {
	command := exec.Command(path, args...)
	command.Stdin = stdin
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command, nil
}

type nativeHelpers struct {
	goos            string
	getenv          func(string) string
	commands        commandBoundary
	acknowledgement time.Duration
	output          *routeOutput
}

func newNativeHelpers(output *routeOutput) nativeHelpers {
	return nativeHelpers{
		goos: runtime.GOOS, getenv: os.Getenv, commands: execCommandBoundary{},
		acknowledgement: helperAcknowledgementTimeout, output: output,
	}
}

func (h nativeHelpers) copy(payload string) {
	var name string
	var args []string
	switch h.goos {
	case "darwin":
		name = "pbcopy"
	case "linux":
		switch {
		case h.getenv("WAYLAND_DISPLAY") != "":
			name = "wl-copy"
		case h.getenv("DISPLAY") != "":
			name = "xclip"
			args = []string{"-selection", "clipboard"}
		default:
			h.output.enqueue("Warning: clipboard sharing requires a graphical session on Linux; the routes remain active.")
			return
		}
	default:
		h.output.enqueue("Warning: clipboard sharing is not available on %s; the routes remain active.", h.goos)
		return
	}
	h.launch("clipboard helper", name, args, bytes.NewBufferString(payload))
}

func (h nativeHelpers) open(address string) {
	var name string
	switch h.goos {
	case "darwin":
		name = "open"
	case "linux":
		if h.getenv("WAYLAND_DISPLAY") == "" && h.getenv("DISPLAY") == "" {
			h.output.enqueue("Warning: opening %s requires a graphical session on Linux; the route remains active.", address)
			return
		}
		name = "xdg-open"
	default:
		h.output.enqueue("Warning: opening a browser is not available on %s; the route remains active.", h.goos)
		return
	}
	h.launch("browser helper", name, []string{address}, nil)
}

func (h nativeHelpers) launch(action, name string, args []string, stdin io.Reader) {
	path, err := h.commands.LookPath(name)
	if err != nil {
		h.output.enqueue("Warning: %s is not available for the %s; the routes remain active.", name, action)
		return
	}
	process, err := h.commands.Start(path, args, stdin)
	if err != nil {
		h.output.enqueue("Warning: the %s failed to start: %v. The routes remain active.", action, err)
		return
	}
	waited := make(chan error, 1)
	go func() { waited <- process.Wait() }()
	go func() {
		timer := time.NewTimer(h.acknowledgement)
		defer timer.Stop()
		select {
		case err := <-waited:
			if err != nil {
				h.output.enqueue("Warning: the %s failed: %v. The routes remain active.", action, err)
			}
		case <-timer.C:
			h.output.enqueue("Warning: the %s did not acknowledge within %s; it may still complete and the routes remain active.", action, h.acknowledgement)
		}
	}()
}

type sharingActions struct {
	output  *routeOutput
	helpers nativeHelpers
	encode  func(string, bool) (string, error)
}

func newSharingActions(output *routeOutput) sharingActions {
	return sharingActions{output: output, helpers: newNativeHelpers(output), encode: encodeQRCode}
}

func (s sharingActions) afterRegistration(settings Settings, routes []routeSession, options startupOptions) {
	addresses := make([]string, len(routes))
	for index, route := range routes {
		addresses[index] = route.URL()
	}
	payload := strings.Join(addresses, "\n")
	public := settings.Domain != ""
	if options.Copy {
		if public {
			s.output.enqueue("Copying registered URLs (public confirmation pending):\n%s", payload)
		} else {
			s.output.enqueue("Copying registered URLs:\n%s", payload)
		}
		s.helpers.copy(payload)
	}
	if options.QR {
		if !public {
			for _, address := range addresses {
				s.output.enqueue("Warning: QR code not generated for local URL %s because localhost cannot be reached from a phone.", address)
			}
		} else {
			for _, address := range addresses {
				code, err := s.encode(address, s.output.terminal)
				if err != nil {
					s.output.enqueue("Warning: cannot encode QR code for %s: %v. The route remains active.", address, err)
					continue
				}
				s.output.enqueue("QR code (public confirmation pending):\n%s%s", code, address)
			}
		}
	}
	if options.Open && !public {
		for _, address := range addresses {
			s.helpers.open(address)
		}
	}
}

func (s sharingActions) confirmed(route routeSession, options startupOptions) {
	if options.Open {
		s.helpers.open(route.URL())
	}
}
