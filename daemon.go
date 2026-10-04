package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Daemon struct {
	RouterPort int
	socketPath string
	settings   Settings
	routes     map[string]*routeTarget
	clients    map[net.Conn]struct{}
	mu         sync.RWMutex
	idle       *time.Timer
	idleAfter  time.Duration
	server     *http.Server
	control    net.Listener
	stopCh     chan struct{}
	stopOnce   sync.Once
	closed     bool
	stopping   bool
}

type routeTarget struct {
	id    string
	port  int
	proxy *httputil.ReverseProxy
}

func newRouteTarget(id string, port int) *routeTarget {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "The local service is not responding.", http.StatusBadGateway)
	}
	return &routeTarget{id: id, port: port, proxy: proxy}
}

func startDaemon(settings Settings, dir string, idleAfter time.Duration) (*Daemon, error) {
	d := &Daemon{
		settings: settings, routes: make(map[string]*routeTarget), clients: make(map[net.Conn]struct{}),
		idleAfter: idleAfter, stopCh: make(chan struct{}), socketPath: controlAddress(dir),
	}
	d.server = &http.Server{Handler: http.HandlerFunc(d.proxy), ReadHeaderTimeout: 5 * time.Second}
	router, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", settings.RouterPort))
	if err != nil {
		return nil, err
	}
	d.RouterPort = router.Addr().(*net.TCPAddr).Port
	d.control, err = listenControl(d.socketPath)
	if err != nil {
		router.Close()
		return nil, err
	}
	d.mu.Lock()
	d.resetIdleLocked()
	d.mu.Unlock()
	go func() {
		if err := d.server.Serve(router); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("router: %v", err)
			d.signalStop()
		}
	}()
	go d.acceptControl()
	return d, nil
}

func listenControl(path string) (net.Listener, error) {
	listener, err := net.Listen("unix", path)
	if errors.Is(err, syscall.EADDRINUSE) {
		probe, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			probe.Close()
			return nil, errors.New("the local daemon is already running")
		}
		if errors.Is(dialErr, syscall.ECONNREFUSED) {
			if removeErr := os.Remove(path); removeErr != nil {
				return nil, removeErr
			}
			listener, err = net.Listen("unix", path)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func (d *Daemon) signalStop() { d.stopOnce.Do(func() { close(d.stopCh) }) }

func (d *Daemon) resetIdleLocked() {
	if d.idle != nil {
		d.idle.Stop()
	}
	if len(d.routes) == 0 && !d.closed {
		d.idle = time.AfterFunc(d.idleAfter, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.routes) == 0 && !d.closed {
				d.stopping = true
				d.signalStop()
			}
		})
	}
}

func (d *Daemon) routeFor(host string) (*routeTarget, bool) {
	if strings.Contains(host, ":") {
		var err error
		host, _, err = net.SplitHostPort(host)
		if err != nil {
			return nil, false
		}
	}
	host = strings.ToLower(host)
	suffix := ".localhost"
	if d.settings.Domain != "" {
		suffix = "." + d.settings.Domain
	}
	label, matches := strings.CutSuffix(host, suffix)
	if !matches {
		return nil, false
	}
	if label == "" || strings.Contains(label, ".") {
		return nil, false
	}
	d.mu.RLock()
	target, found := d.routes[label]
	d.mu.RUnlock()
	return target, found
}

func (d *Daemon) proxy(w http.ResponseWriter, r *http.Request) {
	target, found := d.routeFor(r.Host)
	if !found {
		http.Error(w, "This subdomain is not active.", http.StatusNotFound)
		return
	}
	target.proxy.ServeHTTP(w, r)
}

func (d *Daemon) acceptControl() {
	for {
		conn, err := d.control.Accept()
		if err != nil {
			d.mu.RLock()
			closed := d.closed
			d.mu.RUnlock()
			if !closed {
				log.Printf("control: %v", err)
				d.signalStop()
			}
			return
		}
		d.mu.Lock()
		d.clients[conn] = struct{}{}
		d.mu.Unlock()
		go d.handleControl(conn)
	}
}

func (d *Daemon) handleControl(conn net.Conn) {
	var owned string
	defer func() {
		conn.Close()
		d.mu.Lock()
		delete(d.clients, conn)
		if owned != "" {
			delete(d.routes, owned)
			d.resetIdleLocked()
		}
		d.mu.Unlock()
	}()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readControlFrame(reader)
	if err != nil {
		d.reply(conn, controlResponse{Error: "Invalid command."})
		return
	}
	var request controlRequest
	if json.Unmarshal(line, &request) != nil || subtle.ConstantTimeCompare([]byte(request.Token), []byte(d.settings.Token)) != 1 {
		d.reply(conn, controlResponse{Error: "Access to the local daemon was denied."})
		return
	}
	if request.Type == controlStatus {
		d.mu.RLock()
		response := controlResponse{OK: true, Domain: d.settings.Domain, Capabilities: daemonCapabilities}
		for name, target := range d.routes {
			response.Routes = append(response.Routes, routeStatus{name, target.port})
		}
		d.mu.RUnlock()
		sort.Slice(response.Routes, func(i, j int) bool { return response.Routes[i].Subdomain < response.Routes[j].Subdomain })
		d.reply(conn, response)
		return
	}
	if request.Type != controlRegister {
		d.reply(conn, controlResponse{Error: "Unknown command."})
		return
	}
	name, err := normalizeSubdomain(request.Subdomain)
	if err == nil && (request.Port < 1 || request.Port > 65535) {
		err = errors.New("invalid port")
	}
	if err != nil {
		d.reply(conn, controlResponse{Error: err.Error()})
		return
	}
	routeIDBytes := make([]byte, 16)
	if _, err := rand.Read(routeIDBytes); err != nil {
		d.reply(conn, controlResponse{Error: "Could not create a route instance."})
		return
	}
	routeID := hex.EncodeToString(routeIDBytes)
	d.mu.Lock()
	if d.stopping || d.closed {
		d.mu.Unlock()
		d.reply(conn, controlResponse{Error: "The daemon is shutting down.", Retry: true})
		return
	}
	if _, exists := d.routes[name]; exists {
		d.mu.Unlock()
		d.reply(conn, controlResponse{Error: "Subdomain " + name + " is already in use."})
		return
	}
	if request.Port == d.RouterPort {
		d.mu.Unlock()
		d.reply(conn, controlResponse{Error: "Cannot forward the router port to itself."})
		return
	}
	d.routes[name] = newRouteTarget(routeID, request.Port)
	owned = name
	d.resetIdleLocked()
	d.mu.Unlock()
	address := routeURL(name, d.settings.Domain, d.RouterPort)
	if err := d.reply(conn, controlResponse{
		OK: true, URL: address, RouteID: routeID, Capabilities: daemonCapabilities,
	}); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	io.Copy(io.Discard, reader)
}

func (d *Daemon) reply(conn net.Conn, response controlResponse) error {
	return json.NewEncoder(conn).Encode(response)
}

func (d *Daemon) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	if d.idle != nil {
		d.idle.Stop()
	}
	clients := make([]net.Conn, 0, len(d.clients))
	for client := range d.clients {
		clients = append(clients, client)
	}
	d.mu.Unlock()
	d.signalStop()
	for _, client := range clients {
		client.Close()
	}
	d.control.Close()
	context, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return d.server.Shutdown(context)
}
