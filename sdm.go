package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sdmBase = "https://smartdevicemanagement.googleapis.com/v1/"

// Device is one SDM device as returned by the API, plus a friendly label.
type Device struct {
	Name            string                     `json:"name"` // enterprises/<p>/devices/<id>
	Type            string                     `json:"type"`
	Traits          map[string]json.RawMessage `json:"traits"`
	ParentRelations []struct {
		DisplayName string `json:"displayName"`
	} `json:"parentRelations"`
}

// ID is the short device id (last path segment).
func (d Device) ID() string { return d.Name[strings.LastIndex(d.Name, "/")+1:] }

// Kind is "thermostat", "camera" or "doorbell".
func (d Device) Kind() string {
	return strings.ToLower(strings.TrimPrefix(d.Type, "sdm.devices.types."))
}

// Label prefers the custom name, falling back to the room.
func (d Device) Label() string {
	var info struct {
		CustomName string `json:"customName"`
	}
	_ = json.Unmarshal(d.Traits["sdm.devices.traits.Info"], &info)
	room := ""
	if len(d.ParentRelations) > 0 {
		room = d.ParentRelations[0].DisplayName
	}
	switch {
	case info.CustomName != "":
		return info.CustomName
	case room != "":
		return room + " " + d.Kind()
	default:
		return d.Kind()
	}
}

// EventImages reports whether GenerateImage can work. Google only serves event
// snapshots for RTSP cameras; WebRTC-only models get a 400, so don't bother asking.
func (d Device) EventImages() bool {
	var ls struct {
		SupportedProtocols []string `json:"supportedProtocols"`
	}
	_ = json.Unmarshal(d.Traits["sdm.devices.traits.CameraLiveStream"], &ls)
	for _, p := range ls.SupportedProtocols {
		if p == "RTSP" {
			return true
		}
	}
	return false
}

func (d Device) Has(trait string) bool {
	_, ok := d.Traits["sdm.devices.traits."+trait]
	return ok
}

// SDM wraps the Smart Device Management API with a short device-list cache.
type SDM struct {
	g *Google

	mu      sync.Mutex
	devices []Device
	fetched time.Time
}

func (s *SDM) enterprise() string { return "enterprises/" + s.g.creds.SDMProjectID }

// Devices returns the device list, cached for maxAge.
func (s *SDM) Devices(ctx context.Context, maxAge time.Duration) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.devices != nil && time.Since(s.fetched) < maxAge {
		return s.devices, nil
	}
	var out struct {
		Devices []Device `json:"devices"`
	}
	if err := s.g.Do(ctx, "GET", sdmBase+s.enterprise()+"/devices", nil, &out); err != nil {
		return nil, err
	}
	s.devices, s.fetched = out.Devices, time.Now()
	return s.devices, nil
}

// Device looks up one device by short id.
func (s *SDM) Device(ctx context.Context, id string) (Device, error) {
	devs, err := s.Devices(ctx, time.Minute)
	if err != nil {
		return Device{}, err
	}
	for _, d := range devs {
		if d.ID() == id {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("no device %q", id)
}

// Invalidate forces the next Devices call to refetch (after a command changes state).
func (s *SDM) Invalidate() {
	s.mu.Lock()
	s.fetched = time.Time{}
	s.mu.Unlock()
}

// Execute runs an SDM command (e.g. "sdm.devices.commands.ThermostatMode.SetMode") on a device.
func (s *SDM) Execute(ctx context.Context, deviceID, command string, params map[string]any) (json.RawMessage, error) {
	var out struct {
		Results json.RawMessage `json:"results"`
	}
	body := map[string]any{"command": command, "params": params}
	err := s.g.Do(ctx, "POST", sdmBase+s.enterprise()+"/devices/"+deviceID+":executeCommand", body, &out)
	return out.Results, err
}

// allowedCommands is the set of commands the web UI may send. Anything else is rejected.
var allowedCommands = map[string]bool{
	"sdm.devices.commands.ThermostatMode.SetMode":                 true,
	"sdm.devices.commands.ThermostatEco.SetMode":                  true,
	"sdm.devices.commands.ThermostatTemperatureSetpoint.SetHeat":  true,
	"sdm.devices.commands.ThermostatTemperatureSetpoint.SetCool":  true,
	"sdm.devices.commands.ThermostatTemperatureSetpoint.SetRange": true,
	"sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream":  true,
	"sdm.devices.commands.CameraLiveStream.ExtendWebRtcStream":    true,
	"sdm.devices.commands.CameraLiveStream.StopWebRtcStream":      true,
}

// FetchEventImage downloads the snapshot for a camera event. It must be called
// within ~30 seconds of the event, after which Google discards the image.
func (s *SDM) FetchEventImage(ctx context.Context, deviceID, eventID string) ([]byte, error) {
	res, err := s.Execute(ctx, deviceID, "sdm.devices.commands.CameraEventImage.GenerateImage", map[string]any{"eventId": eventID})
	if err != nil {
		return nil, err
	}
	var img struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(res, &img); err != nil || img.URL == "" {
		return nil, fmt.Errorf("GenerateImage returned no url: %s", res)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", img.URL+"?width=640", nil)
	req.Header.Set("Authorization", "Basic "+img.Token)
	return s.download(req)
}

// FetchClipPreview downloads a short doorbell clip from a ClipPreview event URL.
func (s *SDM) FetchClipPreview(ctx context.Context, previewURL string) ([]byte, error) {
	tok, err := s.g.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", previewURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return s.download(req)
}

func (s *SDM) download(req *http.Request) ([]byte, error) {
	resp, err := s.g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download %s: %s", req.URL.Host, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}
