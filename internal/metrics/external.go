//go:build windows

package metrics

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultExternalURL is where LibreHardwareMonitor's "Remote Web Server" listens.
const DefaultExternalURL = "http://127.0.0.1:8085/data.json"

// ExtSensor is one leaf reading from an external sensor program.
type ExtSensor struct {
	Hardware string  `json:"hardware"` // e.g. "AMD Ryzen 9 7950X3D"
	Group    string  `json:"group"`    // e.g. "Temperatures"
	Name     string  `json:"name"`     // e.g. "Core (Tctl/Tdie)"
	Type     string  `json:"type"`     // Temperature, Power, Clock, Load… (when the program says)
	SensorID string  `json:"sensorId"` // e.g. /amdcpu/0/temperature/2
	Value    string  `json:"value"`    // as displayed, with unit
	Min      string  `json:"min"`
	Max      string  `json:"max"`
	Num      float64 `json:"num"` // Value parsed to a number (0 if not numeric)
	HasNum   bool    `json:"hasNum"`
}

// lhmNode mirrors one node of LibreHardwareMonitor's data.json tree.
type lhmNode struct {
	Text     string    `json:"Text"`
	Value    string    `json:"Value"`
	Min      string    `json:"Min"`
	Max      string    `json:"Max"`
	Type     string    `json:"Type"`
	SensorID string    `json:"SensorId"`
	Children []lhmNode `json:"Children"`
}

// ExternalSource polls an optional, user-run sensor program over loopback.
// It never starts anything and never touches hardware itself: if the program is
// not running, it simply reports "not connected".
type ExternalSource struct {
	url string

	mu      sync.RWMutex
	sensors []ExtSensor
	ok      bool
	source  string
	lastErr error

	cancel context.CancelFunc
}

// NewExternalSource returns a poller for rawURL (loopback only).
func NewExternalSource(rawURL string) *ExternalSource {
	return &ExternalSource{url: rawURL}
}

// Start begins polling until Stop. Failed polls back off so a missing program
// costs almost nothing.
func (e *ExternalSource) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() {
		wait := time.Second
		for {
			if err := e.PollOnce(ctx); err != nil {
				wait = 15 * time.Second
			} else {
				wait = 2 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

// Stop ends polling.
func (e *ExternalSource) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
}

// Connected reports whether the last poll succeeded.
func (e *ExternalSource) Connected() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ok
}

// Sensors returns a copy of the latest readings.
func (e *ExternalSource) Sensors() []ExtSensor {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]ExtSensor(nil), e.sensors...)
}

// loopbackOnly rejects anything that is not a plain http URL to this machine,
// so this reader cannot be turned into a way to fetch from elsewhere.
func loopbackOnly(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PollOnce fetches and parses the sensor tree.
func (e *ExternalSource) PollOnce(ctx context.Context) error {
	if !loopbackOnly(e.url) {
		return errNotLoopback
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url, nil)
	if err != nil {
		return e.fail(err)
	}
	resp, err := lhmClient.Do(req)
	if err != nil {
		return e.fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return e.fail(errStatus(resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // data, not instructions: bounded
	if err != nil {
		return e.fail(err)
	}
	var root lhmNode
	if err := json.Unmarshal(body, &root); err != nil {
		return e.fail(err)
	}
	sensors := flattenLHM(root)

	e.mu.Lock()
	e.sensors, e.ok, e.lastErr, e.source = sensors, true, nil, "LibreHardwareMonitor"
	e.mu.Unlock()
	return nil
}

func (e *ExternalSource) fail(err error) error {
	e.mu.Lock()
	e.ok, e.lastErr = false, err
	e.sensors = nil
	e.mu.Unlock()
	return err
}

type pollError string

func (p pollError) Error() string { return string(p) }

const errNotLoopback = pollError("external sensor URL must be http on loopback")

func errStatus(code int) error { return pollError("external sensors: HTTP " + strconv.Itoa(code)) }

// flattenLHM walks Computer > Hardware > Group > Sensor and returns the leaves.
func flattenLHM(root lhmNode) []ExtSensor {
	var out []ExtSensor
	// root is "Sensor"; its single child is the computer; below that, hardware.
	var walk func(n lhmNode, path []string)
	walk = func(n lhmNode, path []string) {
		if len(n.Children) == 0 {
			if n.Value == "" || len(path) < 2 {
				return
			}
			s := ExtSensor{Name: n.Text, Type: n.Type, SensorID: n.SensorID, Value: n.Value, Min: n.Min, Max: n.Max}
			// path = [computer, hardware, (sub-hardware...), group]
			if len(path) >= 3 {
				s.Hardware, s.Group = path[len(path)-2], path[len(path)-1]
			} else {
				s.Hardware, s.Group = path[len(path)-1], ""
			}
			s.Num, s.HasNum = parseExtNumber(n.Value)
			out = append(out, s)
			return
		}
		next := path
		if n.Text != "" && n.Text != "Sensor" {
			next = append(append([]string(nil), path...), n.Text)
		}
		for _, c := range n.Children {
			walk(c, next)
		}
	}
	walk(root, nil)
	return out
}

// parseExtNumber reads "52.4 °C", "48,1 W", "4.72 GHz" into a float.
func parseExtNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == ',' || c == '-' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.Replace(s[:end], ",", ".", 1), 64)
	return v, err == nil
}

// CPUTemperature picks the best CPU temperature reading: AMD Tctl/Tdie or
// the package sensor, falling back to the hottest core.
func CPUTemperature(sensors []ExtSensor) (float64, string, bool) {
	best, bestName, rank := 0.0, "", 99
	for _, s := range sensors {
		if !s.HasNum || !(strings.EqualFold(s.Type, "Temperature") || strings.Contains(s.Value, "°C")) {
			continue
		}
		id := strings.ToLower(s.SensorID)
		name := strings.ToLower(s.Name)
		// CPU sensors only: LibreHardwareMonitor IDs start /amdcpu/ or /intelcpu/.
		// Matching "amd"/"intel" in the hardware name would pick up a Radeon,
		// an Arc GPU or an Intel SSD when no CPU temperature is available.
		hw := strings.ToLower(s.Hardware)
		cpu := strings.HasPrefix(id, "/amdcpu/") || strings.HasPrefix(id, "/intelcpu/") ||
			(id == "" && (strings.Contains(hw, "ryzen") || strings.Contains(hw, "core(tm)") || strings.Contains(hw, "xeon") || strings.Contains(hw, "threadripper")))
		if !cpu {
			continue
		}
		r := 50
		switch {
		case strings.Contains(name, "tctl") || strings.Contains(name, "tdie"):
			r = 0
		case strings.Contains(name, "package"):
			r = 1
		case strings.Contains(name, "cpu") && !strings.Contains(name, "core"):
			r = 2
		case strings.Contains(name, "max"):
			r = 3
		}
		if r < rank {
			best, bestName, rank = s.Num, s.Name, r
		}
	}
	return best, bestName, rank < 99
}

// lhmClient talks to 127.0.0.1 only. It never follows a redirect (a local
// program could otherwise bounce gotm to any host) and never uses a proxy.
var lhmClient = &http.Client{
	Timeout:       3 * time.Second,
	Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: false, MaxIdleConns: 1},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}
