package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const defaultProjectFile = ".mytunnel.json"

type projectFile struct {
	Version *int              `json:"version"`
	Routes  []json.RawMessage `json:"routes"`
}

type projectFileRoute struct {
	Port      *int    `json:"port"`
	Subdomain *string `json:"subdomain"`
}

func selectedProjectPath(config string) string {
	if config != "" {
		return config
	}
	return defaultProjectFile
}

func loadProjectRoutes(path string) ([]routeSpec, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var project projectFile
	if err := decoder.Decode(&project); err != nil {
		return nil, fmt.Errorf("%s: invalid project file: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected JSON value")
		}
		return nil, fmt.Errorf("%s: trailing data: %w", path, err)
	}
	if project.Version == nil {
		return nil, fmt.Errorf("%s: version is required", path)
	}
	if *project.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported project version %d", path, *project.Version)
	}
	if len(project.Routes) == 0 {
		return nil, fmt.Errorf("%s: routes must be a nonempty array", path)
	}

	routes := make([]routeSpec, 0, len(project.Routes))
	names := make(map[string]struct{}, len(project.Routes))
	for index, encodedRoute := range project.Routes {
		var route projectFileRoute
		routeDecoder := json.NewDecoder(bytes.NewReader(encodedRoute))
		routeDecoder.DisallowUnknownFields()
		if err := routeDecoder.Decode(&route); err != nil {
			return nil, fmt.Errorf("%s: route %d: invalid route: %w", path, index+1, err)
		}
		if route.Port == nil {
			return nil, fmt.Errorf("%s: route %d: port is required", path, index+1)
		}
		if *route.Port < 1 || *route.Port > 65535 {
			return nil, fmt.Errorf("%s: route %d: the port must be an integer from 1 to 65535", path, index+1)
		}
		if route.Subdomain == nil {
			return nil, fmt.Errorf("%s: route %d: subdomain is required", path, index+1)
		}
		name, err := normalizeSubdomain(*route.Subdomain)
		if err != nil {
			return nil, fmt.Errorf("%s: route %d: %w", path, index+1, err)
		}
		if _, duplicate := names[name]; duplicate {
			return nil, fmt.Errorf("%s: route %d: duplicate subdomain %q", path, index+1, name)
		}
		names[name] = struct{}{}
		routes = append(routes, routeSpec{Subdomain: name, Port: *route.Port})
	}
	return routes, nil
}

func runProject(ctx context.Context, args []string, writer io.Writer, probe readinessProbe) error {
	options, err := parseStartupOptions(args, true)
	if err != nil {
		return err
	}
	routes, err := loadProjectRoutes(selectedProjectPath(options.Config))
	if err != nil {
		return err
	}
	return runRouteSessions(ctx, routes, writer, probe, true, options)
}
