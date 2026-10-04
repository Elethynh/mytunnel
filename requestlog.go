package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	requestLogEventType     = "request"
	requestLogDroppedType   = "dropped"
	requestLogQueueCapacity = 128
	requestLogMethodLimit   = 128
	requestLogPathLimit     = 1500
	requestLogWriteTimeout  = 500 * time.Millisecond
)

type requestLogEvent struct {
	Type           string `json:"type"`
	Route          string `json:"route,omitempty"`
	Method         string `json:"method,omitempty"`
	Path           string `json:"path,omitempty"`
	Status         int    `json:"status"`
	DurationMicros int64  `json:"durationMicros"`
	Dropped        int    `json:"dropped,omitempty"`
}

type requestLogMetadata struct {
	route  string
	method string
	path   string
}

type requestLogState struct {
	status atomic.Int64
}

type requestLogContextKey struct{}

func newRequestLogMetadata(route, method, path string) requestLogMetadata {
	return requestLogMetadata{
		route:  terminalSafeMetadata(route, requestLogMethodLimit),
		method: terminalSafeMetadata(method, requestLogMethodLimit),
		path:   terminalSafeMetadata(path, requestLogPathLimit),
	}
}

func terminalSafeMetadata(value string, limit int) string {
	result := make([]byte, 0, min(len(value), limit))
	truncated := false
	for index := 0; index < len(value); index++ {
		byteValue := value[index]
		encoded := []byte{byteValue}
		if byteValue < 0x20 || byteValue > 0x7e {
			const hex = "0123456789abcdef"
			encoded = []byte{'\\', 'x', hex[byteValue>>4], hex[byteValue&0x0f]}
		}
		if len(result)+len(encoded) > limit {
			truncated = true
			break
		}
		result = append(result, encoded...)
	}
	if truncated && limit >= 3 {
		if len(result) > limit-3 {
			result = result[:limit-3]
		}
		result = append(result, '.', '.', '.')
	}
	return string(result)
}

func (s *requestLogState) recordStatus(status int) {
	if status == http.StatusSwitchingProtocols || status >= http.StatusOK {
		s.status.Store(int64(status))
	}
}

func requestLogStateFrom(request *http.Request) *requestLogState {
	state, _ := request.Context().Value(requestLogContextKey{}).(*requestLogState)
	return state
}

func marshalRequestLogEvent(event requestLogEvent) []byte {
	frame, err := json.Marshal(event)
	if err != nil {
		return nil
	}
	frame = append(frame, '\n')
	if len(frame) <= controlFrameLimit {
		return frame
	}
	fallback, _ := json.Marshal(requestLogEvent{
		Type: event.Type, Route: event.Route, Method: event.Method,
		Path: "...", Status: event.Status, DurationMicros: event.DurationMicros, Dropped: event.Dropped,
	})
	return append(fallback, '\n')
}

type requestLogObserver struct {
	connection net.Conn
	route      string
	queue      chan []byte
	mu         sync.Mutex
	dropped    int
	closed     bool
	close      sync.Once
}

func newRequestLogObserver(connection net.Conn, route string) *requestLogObserver {
	return &requestLogObserver{
		connection: connection,
		route:      route,
		queue:      make(chan []byte, requestLogQueueCapacity),
	}
}

func (o *requestLogObserver) publish(frame []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	select {
	case o.queue <- frame:
	default:
		o.dropped++
	}
}

func (o *requestLogObserver) takeDroppedFrame() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dropped == 0 {
		return nil
	}
	frame := marshalRequestLogEvent(requestLogEvent{Type: requestLogDroppedType, Route: o.route, Dropped: o.dropped})
	o.dropped = 0
	return frame
}

func (o *requestLogObserver) writeEvents() {
	defer o.Close()
	for frame := range o.queue {
		if !o.writeFrame(frame) {
			return
		}
		if dropped := o.takeDroppedFrame(); dropped != nil && !o.writeFrame(dropped) {
			return
		}
	}
}

func (o *requestLogObserver) writeFrame(frame []byte) bool {
	if err := o.connection.SetWriteDeadline(time.Now().Add(requestLogWriteTimeout)); err != nil {
		return false
	}
	for len(frame) > 0 {
		written, err := o.connection.Write(frame)
		if err != nil || written == 0 {
			return false
		}
		frame = frame[written:]
	}
	return true
}

func (o *requestLogObserver) Close() {
	o.close.Do(func() {
		o.mu.Lock()
		o.closed = true
		close(o.queue)
		o.mu.Unlock()
		_ = o.connection.Close()
	})
}

type requestLogDisplay struct {
	context context.Context
	cancel  context.CancelFunc
	output  *routeOutput
	mu      sync.Mutex
	closed  bool
	conns   []net.Conn
	wait    sync.WaitGroup
	close   sync.Once
}

func newRequestLogDisplay(parent context.Context, output *routeOutput) *requestLogDisplay {
	ctx, cancel := context.WithCancel(parent)
	display := &requestLogDisplay{
		context: ctx,
		cancel:  cancel,
		output:  output,
	}
	return display
}

func (d *requestLogDisplay) enqueue(line string) {
	d.output.tryEnqueueLog("%s", line)
}

func (d *requestLogDisplay) observe(address string, settings Settings, route routeSession) {
	response, connection, err := sendControlContext(d.context, address, controlRequest{
		Token: settings.Token, Type: controlObserve, Subdomain: route.Spec().Subdomain, RouteID: route.InstanceID(),
	})
	if err != nil {
		d.output.enqueue("Warning: request logs for %s could not start: %v", route.Spec().Subdomain, err)
		return
	}
	if !response.OK {
		connection.Close()
		d.output.enqueue("Warning: request logs for %s could not start: %s", route.Spec().Subdomain, response.Error)
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		connection.Close()
		return
	}
	d.conns = append(d.conns, connection)
	d.mu.Unlock()
	d.wait.Add(1)
	go d.readEvents(route.Spec().Subdomain, connection)
}

func (d *requestLogDisplay) readEvents(route string, connection *controlConnection) {
	defer d.wait.Done()
	defer connection.Close()
	stop := context.AfterFunc(d.context, func() { connection.Close() })
	defer stop()
	for {
		frame, err := connection.readFrame()
		if err != nil {
			if d.context.Err() == nil && !errors.Is(err, net.ErrClosed) {
				d.enqueue(fmt.Sprintf("Warning: request logs for %s stopped: %v", route, err))
			}
			return
		}
		var event requestLogEvent
		if json.Unmarshal(frame, &event) != nil {
			d.enqueue(fmt.Sprintf("Warning: request logs for %s stopped: invalid daemon event", route))
			return
		}
		switch event.Type {
		case requestLogEventType:
			d.enqueue(fmt.Sprintf("[%s] %s %s %d %s", event.Route, event.Method, event.Path,
				event.Status, (time.Duration(event.DurationMicros) * time.Microsecond).String()))
		case requestLogDroppedType:
			d.enqueue(fmt.Sprintf("Warning: daemon dropped %d request log events for %s.", event.Dropped, route))
		default:
			d.enqueue(fmt.Sprintf("Warning: request logs for %s stopped: invalid daemon event", route))
			return
		}
	}
}

func (d *requestLogDisplay) Close() {
	d.close.Do(func() {
		d.cancel()
		d.mu.Lock()
		d.closed = true
		connections := append([]net.Conn(nil), d.conns...)
		d.mu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
		d.wait.Wait()
	})
}

func startRequestLogs(ctx context.Context, settings Settings, routes []routeSession, output *routeOutput) *requestLogDisplay {
	display := newRequestLogDisplay(ctx, output)
	dir, err := settingsDir()
	if err != nil {
		output.enqueue("Warning: request logs could not start: %v", err)
		return display
	}
	address := controlAddress(dir)
	for _, route := range routes {
		display.observe(address, settings, route)
	}
	return display
}
