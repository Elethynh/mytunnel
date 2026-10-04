package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"
)

type controlRequestType string

const (
	controlStatus   controlRequestType = "status"
	controlRegister controlRequestType = "register"
)

const capabilityRouteInstances = "route-instances-v1"

type controlRequest struct {
	Token     string             `json:"token"`
	Type      controlRequestType `json:"type"`
	Subdomain string             `json:"subdomain,omitempty"`
	Port      int                `json:"port,omitempty"`
}

type routeStatus struct {
	Subdomain string `json:"subdomain"`
	Port      int    `json:"port"`
}

type controlResponse struct {
	OK           bool          `json:"ok"`
	Error        string        `json:"error,omitempty"`
	URL          string        `json:"url,omitempty"`
	Domain       string        `json:"domain,omitempty"`
	Retry        bool          `json:"retry,omitempty"`
	Routes       []routeStatus `json:"routes,omitempty"`
	Capabilities []string      `json:"capabilities,omitempty"`
	RouteID      string        `json:"routeId,omitempty"`
}

type controlConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *controlConnection) Read(data []byte) (int, error) { return c.reader.Read(data) }

func (c *controlConnection) readFrame() ([]byte, error) { return readControlFrame(c.reader) }

func controlAddress(dir string) string { return filepath.Join(dir, "control.sock") }

func sendControl(address string, request controlRequest) (controlResponse, *controlConnection, error) {
	return sendControlContext(context.Background(), address, request)
}

func sendControlContext(ctx context.Context, address string, request controlRequest) (controlResponse, *controlConnection, error) {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "unix", address)
	if err != nil {
		return controlResponse{}, nil, err
	}
	stream := &controlConnection{Conn: conn, reader: bufio.NewReaderSize(conn, 4096)}
	stopCancellation := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancellation()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return controlResponse{}, nil, ctx.Err()
		}
		return controlResponse{}, nil, err
	}
	line, err := stream.readFrame()
	if err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return controlResponse{}, nil, ctx.Err()
		}
		return controlResponse{}, nil, err
	}
	var response controlResponse
	if err := json.Unmarshal(line, &response); err != nil {
		conn.Close()
		return controlResponse{}, nil, fmt.Errorf("invalid daemon response: %w", err)
	}
	conn.SetDeadline(time.Time{})
	return response, stream, nil
}

func readControlFrame(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > 4096 {
		return nil, errors.New("daemon response is too long")
	}
	if err != nil {
		return nil, err
	}
	return line, nil
}
