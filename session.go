package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

var daemonCapabilities = []string{capabilityRouteInstances, capabilityReadiness}

type routeSpec struct {
	Subdomain string
	Port      int
}

type routeSession interface {
	Spec() routeSpec
	URL() string
	InstanceID() string
	Readiness() routeReadiness
	Wait() error
	Close() error
}

type routeReadiness struct {
	Path  string
	Proof string
}

type routeOutput struct {
	lines    chan string
	done     chan struct{}
	project  bool
	terminal bool
	mu       sync.RWMutex
	closed   bool
	close    sync.Once
}

const routeOutputFlushTimeout = 100 * time.Millisecond

func newRouteOutput(writer io.Writer, routes int) *routeOutput {
	output := &routeOutput{
		lines: make(chan string, routes*8+8), done: make(chan struct{}),
		terminal: writerIsTerminal(writer),
	}
	go func() {
		defer close(output.done)
		for line := range output.lines {
			_, _ = fmt.Fprintln(writer, line)
		}
	}()
	return output
}

func (o *routeOutput) enqueue(format string, values ...any) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		return
	}
	o.lines <- fmt.Sprintf(format, values...)
}

func (o *routeOutput) closeOutput() {
	o.close.Do(func() {
		o.mu.Lock()
		o.closed = true
		close(o.lines)
		o.mu.Unlock()
	})
}

func (o *routeOutput) closeAndWait() {
	o.closeOutput()
	timer := time.NewTimer(routeOutputFlushTimeout)
	defer timer.Stop()
	select {
	case <-o.done:
	case <-timer.C:
	}
}

type routeRegistrationError struct {
	spec     routeSpec
	acquired int
	err      error
}

func (e *routeRegistrationError) Error() string { return e.err.Error() }
func (e *routeRegistrationError) Unwrap() error { return e.err }

type ownedRouteSession struct {
	spec       routeSpec
	address    string
	instance   string
	readiness  routeReadiness
	connection *controlConnection
}

func (s *ownedRouteSession) Spec() routeSpec           { return s.spec }
func (s *ownedRouteSession) URL() string               { return s.address }
func (s *ownedRouteSession) InstanceID() string        { return s.instance }
func (s *ownedRouteSession) Readiness() routeReadiness { return s.readiness }
func (s *ownedRouteSession) Close() error              { return s.connection.Close() }

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
	return acquireRouteSessionsWithOutput(ctx, specs, nil, requiredCapabilities...)
}

func acquireRouteSessionsWithOutput(ctx context.Context, specs []routeSpec, output *routeOutput, requiredCapabilities ...string) (Settings, *routeSessionGroup, error) {
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
	if settings.Domain != "" {
		requiredCapabilities = append(requiredCapabilities, capabilityReadiness)
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
			registrationErr := &routeRegistrationError{spec: spec, acquired: len(group.Routes()), err: err}
			group.Close()
			return Settings{}, nil, registrationErr
		}
		if !response.OK {
			connection.Close()
			registrationErr := &routeRegistrationError{
				spec: spec, acquired: len(group.Routes()), err: errors.New(response.Error),
			}
			group.Close()
			return Settings{}, nil, registrationErr
		}
		if response.RouteID == "" || !hasCapabilities(response.Capabilities, requiredCapabilities) ||
			(settings.Domain != "" && (response.ReadinessPath == "" || response.ReadinessProof == "")) {
			connection.Close()
			registrationErr := &routeRegistrationError{
				spec: spec, acquired: len(group.Routes()), err: oldDaemonError(),
			}
			group.Close()
			return Settings{}, nil, registrationErr
		}
		session := &ownedRouteSession{
			spec: spec, address: response.URL, instance: response.RouteID,
			readiness: routeReadiness{Path: response.ReadinessPath, Proof: response.ReadinessProof}, connection: connection,
		}
		group.add(session)
		if output != nil {
			mode := "Local"
			if settings.Domain != "" {
				mode = "Public"
			}
			if output.project {
				output.enqueue("Project route registered (startup pending, %s): %s → 127.0.0.1:%d", strings.ToLower(mode), session.URL(), session.Spec().Port)
			} else {
				output.enqueue("%s route registered: %s → 127.0.0.1:%d", mode, session.URL(), session.Spec().Port)
			}
		}
	}
	return settings, group, nil
}

func runRouteSessions(ctx context.Context, specs []routeSpec, writer io.Writer, probe readinessProbe, project bool, options startupOptions) error {
	output := newRouteOutput(writer, len(specs))
	output.project = project
	defer output.closeAndWait()
	settings, group, err := acquireRouteSessionsWithOutput(ctx, specs, output)
	if err != nil {
		var registrationErr *routeRegistrationError
		if project && errors.As(err, &registrationErr) {
			if registrationErr.acquired > 0 {
				return fmt.Errorf("project route %q failed; the displayed project group was rolled back: %w", registrationErr.spec.Subdomain, registrationErr.err)
			}
			return fmt.Errorf("project route %q failed: %w", registrationErr.spec.Subdomain, registrationErr.err)
		}
		return err
	}
	defer group.Close()
	if project {
		output.enqueue("Project startup complete: all routes registered.")
	}
	return runAcquiredRouteSessions(settings, group, output, probe, options, newSharingActions(output))
}

func runAcquiredRouteSessions(settings Settings, group *routeSessionGroup, output *routeOutput, probe readinessProbe, options startupOptions, sharing sharingActions) error {
	routes := group.Routes()
	sharing.afterRegistration(settings, routes, options)
	if settings.Domain != "" {
		for _, route := range routes {
			output.enqueue("Checking public reachability: %s", route.URL())
		}
		for result := range probePublicRoutes(group.Context(), routes, probe) {
			switch result.Status {
			case readinessConfirmed:
				output.enqueue("Public reachability confirmed: %s", result.Route.URL())
				sharing.confirmed(result.Route, options)
			case readinessTimedOut:
				output.enqueue("Warning: public reachability was not confirmed for %s within 30 seconds; check the hostname's DNS destination and daemon.log. The route remains active.", result.Route.URL())
			}
		}
	}
	if group.Context().Err() == nil {
		output.enqueue("Press Ctrl+C to stop.")
	}
	return group.Wait()
}

func hasCapabilities(available, required []string) bool {
	for _, requirement := range required {
		if !slices.Contains(available, requirement) {
			return false
		}
	}
	return true
}

func oldDaemonError() error {
	return errors.New("the local daemon is from an older mytunnel version; stop active mytunnel commands, then restart this command")
}
