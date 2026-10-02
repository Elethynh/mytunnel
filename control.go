package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"
)

type controlRequestType string

const (
	controlStatus   controlRequestType = "status"
	controlRegister controlRequestType = "register"
)

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
	OK     bool          `json:"ok"`
	Error  string        `json:"error,omitempty"`
	URL    string        `json:"url,omitempty"`
	Domain string        `json:"domain,omitempty"`
	Retry  bool          `json:"retry,omitempty"`
	Routes []routeStatus `json:"routes,omitempty"`
}

func controlAddress(dir string) string { return filepath.Join(dir, "control.sock") }

func sendControl(address string, request controlRequest) (controlResponse, net.Conn, error) {
	conn, err := net.DialTimeout("unix", address, time.Second)
	if err != nil {
		return controlResponse{}, nil, err
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		conn.Close()
		return controlResponse{}, nil, err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 4097)).ReadBytes('\n')
	if err != nil || len(line) > 4096 {
		conn.Close()
		if err == nil {
			err = errors.New("daemon response is too long")
		}
		return controlResponse{}, nil, err
	}
	var response controlResponse
	if err := json.Unmarshal(line, &response); err != nil {
		conn.Close()
		return controlResponse{}, nil, fmt.Errorf("invalid daemon response: %w", err)
	}
	conn.SetDeadline(time.Time{})
	return response, conn, nil
}
