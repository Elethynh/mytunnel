package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

var daemonCapabilities = []string{capabilityRouteInstances}

type routeSpec struct {
	Subdomain string
	Port      int
}

type routeSession interface {
	Spec() routeSpec
	URL() string
	InstanceID() string
	Wait() error
	Close() error
}

type ownedRouteSession struct {
	spec       routeSpec
	address    string
	instance   string
	connection *controlConnection
}

func (s *ownedRouteSession) Spec() routeSpec    { return s.spec }
func (s *ownedRouteSession) URL() string        { return s.address }
func (s *ownedRouteSession) InstanceID() string { return s.instance }
func (s *ownedRouteSession) Close() error       { return s.connection.Close() }

func (s *ownedRouteSession) Wait() error {
	_, err := io.Copy(io.Discard, s.connection)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("the connection to the local daemon was interrupted: %w", err)
	}
	return errors.New("the connection to the local daemon was interrupted")
}

type routeSessionGroup struct {
	context  context.Context
	cancel   context.CancelCauseFunc
	mu       sync.Mutex
	sessions []routeSession
	close    sync.Once
}

func newRouteSessionGroup(parent context.Context) *routeSessionGroup {
	ctx, cancel := context.WithCancelCause(parent)
	return &routeSessionGroup{context: ctx, cancel: cancel}
}

func (g *routeSessionGroup) Context() context.Context { return g.context }

func (g *routeSessionGroup) add(session routeSession) {
	g.mu.Lock()
	g.sessions = append(g.sessions, session)
	g.mu.Unlock()
	go func() {
		if err := session.Wait(); err != nil {
			g.cancel(err)
		}
	}()
}

func (g *routeSessionGroup) Routes() []routeSession {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]routeSession(nil), g.sessions...)
}

func (g *routeSessionGroup) Close() {
	g.close.Do(func() {
		g.cancel(context.Canceled)
		for _, session := range g.Routes() {
			_ = session.Close()
		}
	})
}

func (g *routeSessionGroup) Wait() error {
	<-g.context.Done()
	cause := context.Cause(g.context)
	g.Close()
	if errors.Is(cause, context.Canceled) {
		return nil
	}
	return cause
}

func acquireRouteSessions(ctx context.Context, specs []routeSpec, requiredCapabilities ...string) (Settings, *routeSessionGroup, error) {
	requiredCapabilities = append([]string{capabilityRouteInstances}, requiredCapabilities...)
	lock, err := lockSettingsDirContext(ctx)
	if err != nil {
		return Settings{}, nil, err
	}
	defer lock.Close()
	settings, dir, err := loadSettings()
	if err != nil {
		return Settings{}, nil, err
	}
	current, err := probeStatusContext(ctx, settings, dir)
	if err != nil {
		return Settings{}, nil, err
	}
	if current != nil && !hasCapabilities(current.Capabilities, requiredCapabilities) {
		return Settings{}, nil, oldDaemonError()
	}
	group := newRouteSessionGroup(ctx)
	for _, spec := range specs {
		response, connection, err := register(group.Context(), settings, dir, spec.Subdomain, spec.Port)
		if err != nil {
			if cause := context.Cause(group.Context()); cause != nil && !errors.Is(cause, context.Canceled) {
				err = cause
			}
			group.Close()
			return Settings{}, nil, err
		}
		if !response.OK {
			connection.Close()
			group.Close()
			return Settings{}, nil, errors.New(response.Error)
		}
		if response.RouteID == "" || !hasCapabilities(response.Capabilities, requiredCapabilities) {
			connection.Close()
			group.Close()
			return Settings{}, nil, oldDaemonError()
		}
		group.add(&ownedRouteSession{
			spec: spec, address: response.URL, instance: response.RouteID, connection: connection,
		})
	}
	return settings, group, nil
}

func hasCapabilities(available, required []string) bool {
	for _, requirement := range required {
		found := false
		for _, capability := range available {
			if capability == requirement {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func oldDaemonError() error {
	return errors.New("the local daemon is from an older mytunnel version; stop active mytunnel commands, then restart this command")
}
