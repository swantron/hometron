package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Outdoor weather from the US National Weather Service (api.weather.gov): free, no key.
//
// Config: <state>/weather.json   {"lat": 45.68, "lon": -111.04}
//
// Keep it to two decimals (about 1 km); NWS doesn't need the exact house.
type Weather struct {
	lat, lon float64
	http     *http.Client

	mu         sync.Mutex
	grid       *nwsPoint
	now        WeatherNow
	forecast   []WeatherPeriod
	hourly     []WeatherHour
	updated    time.Time
	forecastAt time.Time
	err        string
}

type nwsPoint struct {
	Forecast, Hourly, Stations, City string
	Station                          string
}

type WeatherNow struct {
	TempF   *float64  `json:"tempF,omitempty"`
	Text    string    `json:"text,omitempty"`
	Hum     *float64  `json:"rh,omitempty"`
	WindMph *float64  `json:"windMph,omitempty"`
	At      time.Time `json:"at"`
}

type WeatherPeriod struct {
	Name   string    `json:"name"` // "Tonight", "Tuesday"
	Start  time.Time `json:"start"`
	Day    bool      `json:"day"`
	TempF  float64   `json:"tempF"`
	Text   string    `json:"text"`
	Precip *float64  `json:"precip,omitempty"` // chance, percent
}

type WeatherHour struct {
	T     time.Time `json:"t"`
	TempF float64   `json:"tempF"`
}

type WeatherView struct {
	City    string          `json:"city,omitempty"`
	Now     WeatherNow      `json:"now"`
	Periods []WeatherPeriod `json:"periods"`
	Hourly  []WeatherHour   `json:"hourly,omitempty"`
	Error   string          `json:"error,omitempty"`
	Updated time.Time       `json:"updated"`
}

const nwsBase = "https://api.weather.gov/"

func loadWeather(stateDir string) (*Weather, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "weather.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c struct{ Lat, Lon float64 }
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("weather.json: %w", err)
	}
	if c.Lat == 0 || c.Lon == 0 || math.Abs(c.Lat) > 90 || math.Abs(c.Lon) > 180 {
		return nil, errors.New("weather.json: lat/lon missing or out of range")
	}
	return &Weather{lat: math.Round(c.Lat*100) / 100, lon: math.Round(c.Lon*100) / 100, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

func (w *Weather) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	// NWS asks every client to identify itself with a User-Agent.
	req.Header.Set("User-Agent", "hometron (https://github.com/swantron/hometron)")
	req.Header.Set("Accept", "application/geo+json")
	res, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("NWS %s: HTTP %d %s", url, res.StatusCode, b)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

// Run refreshes current conditions every 10 minutes and the forecast every hour.
func (w *Weather) Run(ctx context.Context) {
	for {
		if err := w.refresh(ctx); err != nil {
			log.Printf("weather: %v", err)
			w.mu.Lock()
			w.err = err.Error()
			w.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

func (w *Weather) refresh(ctx context.Context) error {
	if w.grid == nil {
		var p struct {
			Properties struct {
				Forecast            string `json:"forecast"`
				ForecastHourly      string `json:"forecastHourly"`
				ObservationStations string `json:"observationStations"`
				RelativeLocation    struct {
					Properties struct{ City, State string } `json:"properties"`
				} `json:"relativeLocation"`
			} `json:"properties"`
		}
		if err := w.get(ctx, fmt.Sprintf("%spoints/%.2f,%.2f", nwsBase, w.lat, w.lon), &p); err != nil {
			return err
		}
		g := &nwsPoint{Forecast: p.Properties.Forecast, Hourly: p.Properties.ForecastHourly, Stations: p.Properties.ObservationStations,
			City: p.Properties.RelativeLocation.Properties.City}
		var st struct {
			Features []struct {
				Properties struct {
					StationIdentifier string `json:"stationIdentifier"`
				} `json:"properties"`
			} `json:"features"`
		}
		if err := w.get(ctx, g.Stations, &st); err != nil {
			return err
		}
		if len(st.Features) > 0 {
			g.Station = st.Features[0].Properties.StationIdentifier
		}
		w.grid = g
	}

	now := WeatherNow{At: time.Now()}
	if w.grid.Station != "" {
		var o struct {
			Properties struct {
				Timestamp        time.Time `json:"timestamp"`
				TextDescription  string    `json:"textDescription"`
				Temperature      nwsValue  `json:"temperature"`
				RelativeHumidity nwsValue  `json:"relativeHumidity"`
				WindSpeed        nwsValue  `json:"windSpeed"`
			} `json:"properties"`
		}
		if err := w.get(ctx, nwsBase+"stations/"+w.grid.Station+"/observations/latest", &o); err == nil &&
			time.Since(o.Properties.Timestamp) < 3*time.Hour {
			now = WeatherNow{TempF: cToF(o.Properties.Temperature.Value), Text: o.Properties.TextDescription,
				Hum: round1(o.Properties.RelativeHumidity.Value), WindMph: kmhToMph(o.Properties.WindSpeed.Value), At: o.Properties.Timestamp}
		}
	}

	w.mu.Lock()
	needForecast := time.Since(w.forecastAt) > time.Hour
	w.mu.Unlock()
	var periods []WeatherPeriod
	var hours []WeatherHour
	if needForecast {
		var f nwsForecast
		if err := w.get(ctx, w.grid.Forecast, &f); err != nil {
			return err
		}
		for _, p := range f.Properties.Periods {
			periods = append(periods, WeatherPeriod{Name: p.Name, Start: p.StartTime, Day: p.IsDaytime, TempF: toFUnit(p.Temperature, p.TemperatureUnit),
				Text: p.ShortForecast, Precip: p.ProbabilityOfPrecipitation.Value})
		}
		var hf nwsForecast
		if err := w.get(ctx, w.grid.Hourly, &hf); err == nil {
			for i, p := range hf.Properties.Periods {
				if i >= 36 {
					break
				}
				hours = append(hours, WeatherHour{T: p.StartTime, TempF: toFUnit(p.Temperature, p.TemperatureUnit)})
			}
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if now.TempF == nil { // station silent: use the current hour's forecast
		src := hours
		if len(src) == 0 {
			src = w.hourly
		}
		for _, hr := range src {
			if hr.T.Add(time.Hour).After(time.Now()) {
				t := hr.TempF
				now.TempF = &t
				break
			}
		}
	}
	w.now, w.updated, w.err = now, time.Now(), ""
	if needForecast {
		w.forecast, w.forecastAt = periods, time.Now()
		if len(hours) > 0 {
			w.hourly = hours
		}
	}
	return nil
}

type nwsValue struct {
	Value *float64 `json:"value"`
}

type nwsForecast struct {
	Properties struct {
		Periods []struct {
			Name                       string    `json:"name"`
			StartTime                  time.Time `json:"startTime"`
			IsDaytime                  bool      `json:"isDaytime"`
			Temperature                float64   `json:"temperature"`
			TemperatureUnit            string    `json:"temperatureUnit"`
			ShortForecast              string    `json:"shortForecast"`
			ProbabilityOfPrecipitation nwsValue  `json:"probabilityOfPrecipitation"`
		} `json:"periods"`
	} `json:"properties"`
}

func (w *Weather) View() WeatherView {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := WeatherView{Now: w.now, Periods: w.forecast, Hourly: w.hourly, Error: w.err, Updated: w.updated}
	if w.grid != nil {
		v.City = w.grid.City
	}
	if len(v.Periods) > 6 {
		v.Periods = v.Periods[:6]
	}
	return v
}

// OutdoorF is the current outside temperature, for the thermostat history.
func (w *Weather) OutdoorF() *float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.now.TempF == nil || time.Since(w.updated) > time.Hour {
		return nil
	}
	t := *w.now.TempF
	return &t
}

func cToF(c *float64) *float64 {
	if c == nil {
		return nil
	}
	f := math.Round(*c*9/5 + 32)
	return &f
}

func kmhToMph(k *float64) *float64 {
	if k == nil {
		return nil
	}
	m := math.Round(*k / 1.609)
	return &m
}

func round1(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := math.Round(*v)
	return &r
}

func toFUnit(t float64, unit string) float64 {
	if unit == "C" {
		return math.Round(t*9/5 + 32)
	}
	return t
}
