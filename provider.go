package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Provider is one integration (Nest, Tailwind, ...). Adding a new kind of device means
// writing one of these, registering it in main, and teaching the UI to draw its Kind.
type Provider interface {
	// Devices returns what to show. It should be fast (serve from a cache or poll loop).
	Devices(ctx context.Context) ([]deviceView, error)
	// Owns reports whether a device id belongs to this provider.
	Owns(ctx context.Context, id string) bool
	// Command runs an allow-listed command. Return errNotAllowed for anything else.
	Command(ctx context.Context, id, command string, params map[string]any) (json.RawMessage, error)
}

var errNotAllowed = errors.New("command not allowed")

// nest adapts the SDM client to Provider.
type nest struct{ sdm *SDM }

func (n nest) Devices(ctx context.Context) ([]deviceView, error) {
	devs, err := n.sdm.Devices(ctx, 30*time.Second)
	if err != nil {
		return nil, err
	}
	out := make([]deviceView, 0, len(devs))
	for _, d := range devs {
		out = append(out, viewOf(d))
	}
	return out, nil
}

func (n nest) Owns(ctx context.Context, id string) bool {
	_, err := n.sdm.Device(ctx, id)
	return err == nil
}

func (n nest) Command(ctx context.Context, id, command string, params map[string]any) (json.RawMessage, error) {
	if !allowedCommands[command] {
		return nil, errNotAllowed
	}
	res, err := n.sdm.Execute(ctx, id, command, params)
	if strings.Contains(command, "Thermostat") {
		n.sdm.Invalidate()
	}
	return res, err
}
