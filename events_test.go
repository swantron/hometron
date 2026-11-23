package main

import "testing"

func TestParseEvent(t *testing.T) {
	cases := []struct {
		name, payload string
		ok            bool
		kind, image   string
		preview       string
	}{
		{
			name: "doorbell ring with person beats motion",
			payload: `{"eventId":"e1","timestamp":"2026-11-22T20:00:00Z","eventThreadId":"th1",
			  "resourceUpdate":{"name":"enterprises/p/devices/DEV123456789","events":{
			    "sdm.devices.events.CameraMotion.Motion":{"eventId":"m1"},
			    "sdm.devices.events.DoorbellChime.Chime":{"eventId":"c1"},
			    "sdm.devices.events.CameraPerson.Person":{"eventId":"p1"}}}}`,
			ok: true, kind: "chime", image: "c1",
		},
		{
			name: "clip preview only",
			payload: `{"timestamp":"2026-11-22T20:00:05Z","eventThreadId":"th1",
			  "resourceUpdate":{"name":"enterprises/p/devices/DEV1","events":{
			    "sdm.devices.events.CameraClipPreview.ClipPreview":{"previewUrl":"https://example/clip"}}}}`,
			ok: true, preview: "https://example/clip",
		},
		{
			name:    "thermostat trait update has no events",
			payload: `{"timestamp":"2026-11-22T20:00:00Z","resourceUpdate":{"name":"enterprises/p/devices/T","traits":{"sdm.devices.traits.Temperature":{"ambientTemperatureCelsius":21}}}}`,
			ok:      false,
		},
		{name: "garbage", payload: `not json`, ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, ok := parseEvent([]byte(c.payload))
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if p.kind != c.kind || p.imageEvent != c.image || p.previewURL != c.preview {
				t.Fatalf("got kind=%q image=%q preview=%q", p.kind, p.imageEvent, p.previewURL)
			}
		})
	}
}

func TestToF(t *testing.T) {
	c := 21.509995
	if f := *toF(&c); f != 70.7 {
		t.Fatalf("toF = %v, want 70.7", f)
	}
	if toF(nil) != nil {
		t.Fatal("toF(nil) should be nil")
	}
}
