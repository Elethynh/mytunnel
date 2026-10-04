package main

import (
	"reflect"
	"testing"
)

func TestParseStartupOptions(t *testing.T) {
	tests := []struct {
		name    string
		project bool
		args    []string
		want    startupOptions
	}{
		{
			name: "single route flags in any order",
			args: []string{"--logs", "--subdomain", "Demo-1", "--open", "--qr", "--copy"},
			want: startupOptions{Subdomain: "Demo-1", Open: true, Copy: true, QR: true, Logs: true},
		},
		{
			name:    "project flags in any order",
			project: true,
			args:    []string{"--copy", "--config", "other.json", "--logs", "--open", "--qr"},
			want:    startupOptions{Config: "other.json", Open: true, Copy: true, QR: true, Logs: true},
		},
		{name: "empty", want: startupOptions{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseStartupOptions(test.args, test.project)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("options = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseStartupOptionsRejectsInvalidFlags(t *testing.T) {
	tests := []struct {
		name    string
		project bool
		args    []string
	}{
		{name: "duplicate boolean", args: []string{"--open", "--open"}},
		{name: "duplicate value", args: []string{"--subdomain", "one", "--subdomain", "two"}},
		{name: "missing value", args: []string{"--subdomain"}},
		{name: "flag where value expected", args: []string{"--subdomain", "--open"}},
		{name: "unknown", args: []string{"--wat"}},
		{name: "config on single route", args: []string{"--config", "project.json"}},
		{name: "subdomain on project", project: true, args: []string{"--subdomain", "one"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseStartupOptions(test.args, test.project); err == nil {
				t.Fatal("accepted invalid startup options")
			}
		})
	}
}
