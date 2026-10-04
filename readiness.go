package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const maxReadinessProofBytes = 1024

type readinessStatus uint8

const (
	readinessConfirmed readinessStatus = iota
	readinessTimedOut
	readinessCanceled
)

type routeReadinessResult struct {
	Route  routeSession
	Status readinessStatus
}

type readinessProbe struct {
	client         *http.Client
	overallTimeout time.Duration
	requestTimeout time.Duration
	retryDelay     time.Duration
}

func newReadinessProbe() readinessProbe {
	return readinessProbe{
		client: &http.Client{
			Transport: http.DefaultTransport,
		},
		overallTimeout: 30 * time.Second,
		requestTimeout: 3 * time.Second,
		retryDelay:     time.Second,
	}
}

func probePublicRoutes(ctx context.Context, routes []routeSession, probe readinessProbe) <-chan routeReadinessResult {
	results := make(chan routeReadinessResult, len(routes))
	var wait sync.WaitGroup
	wait.Add(len(routes))
	for _, route := range routes {
		go func() {
			defer wait.Done()
			results <- routeReadinessResult{Route: route, Status: probe.check(ctx, route)}
		}()
	}
	go func() {
		wait.Wait()
		close(results)
	}()
	return results
}

func (p readinessProbe) check(ctx context.Context, route routeSession) readinessStatus {
	readiness := route.Readiness()
	target, err := url.Parse(route.URL())
	if err != nil || target.Scheme != "https" || target.Host == "" || readiness.Path == "" || readiness.Proof == "" {
		return readinessTimedOut
	}
	target.Path = readiness.Path
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""

	overall, cancel := context.WithTimeout(ctx, p.overallTimeout)
	defer cancel()
	client := *p.client
	client.Timeout = p.requestTimeout
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for {
		if p.attempt(overall, &client, target, readiness.Proof) {
			return readinessConfirmed
		}
		if overall.Err() != nil {
			if ctx.Err() != nil {
				return readinessCanceled
			}
			return readinessTimedOut
		}
		timer := time.NewTimer(p.retryDelay)
		select {
		case <-overall.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctx.Err() != nil {
				return readinessCanceled
			}
			return readinessTimedOut
		case <-timer.C:
		}
	}
}

func (p readinessProbe) attempt(ctx context.Context, client *http.Client, target *url.URL, proof string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReadinessProofBytes+1))
	if err != nil || len(body) > maxReadinessProofBytes {
		return false
	}
	return response.StatusCode == http.StatusOK && bytes.Equal(body, []byte(proof))
}
