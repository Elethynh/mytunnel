package main

import (
	"fmt"
	"strings"
)

type startupOptions struct {
	Subdomain string
	Config    string
	Open      bool
	Copy      bool
	QR        bool
	Logs      bool
}

func parseStartupOptions(args []string, project bool) (startupOptions, error) {
	var options startupOptions
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		flag := args[index]
		if seen[flag] {
			return startupOptions{}, fmt.Errorf("invalid argument: %s", flag)
		}
		seen[flag] = true
		switch flag {
		case "--subdomain":
			if project {
				return startupOptions{}, fmt.Errorf("invalid argument: %s", flag)
			}
			value, next, err := startupOptionValue(args, index)
			if err != nil {
				return startupOptions{}, err
			}
			options.Subdomain = value
			index = next
		case "--config":
			if !project {
				return startupOptions{}, fmt.Errorf("invalid argument: %s", flag)
			}
			value, next, err := startupOptionValue(args, index)
			if err != nil {
				return startupOptions{}, err
			}
			options.Config = value
			index = next
		case "--open":
			options.Open = true
		case "--copy":
			options.Copy = true
		case "--qr":
			options.QR = true
		case "--logs":
			options.Logs = true
		default:
			return startupOptions{}, fmt.Errorf("invalid argument: %s", flag)
		}
	}
	return options, nil
}

func startupOptionValue(args []string, index int) (string, int, error) {
	if index+1 >= len(args) || args[index+1] == "" || strings.HasPrefix(args[index+1], "--") {
		return "", index, fmt.Errorf("invalid argument: %s", args[index])
	}
	return args[index+1], index + 1, nil
}
