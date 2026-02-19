package consul

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Watcher watches Consul for service changes using blocking queries.
type Watcher struct {
	addr   string
	token  string
	tag    string
	client *http.Client
}

// NewWatcher creates a new Consul watcher.
func NewWatcher(addr, token, tag string) *Watcher {
	return &Watcher{
		addr:  addr,
		token: token,
		tag:   tag,
		client: &http.Client{
			Timeout: 6 * time.Minute, // longer than Consul's max wait (5m)
		},
	}
}

// catalogServicesResponse is the JSON response from /v1/catalog/services.
// It maps service name → list of tags.
type catalogServicesResponse map[string][]string

// healthServiceEntry is a single entry from /v1/health/service/<name>.
type healthServiceEntry struct {
	Node    healthNode    `json:"Node"`
	Service healthService `json:"Service"`
}

type healthNode struct {
	Address string `json:"Address"`
}

type healthService struct {
	Service string   `json:"Service"`
	Address string   `json:"Address"`
	Port    int      `json:"Port"`
	Tags    []string `json:"Tags"`
}

// ListServices returns the list of service names matching the configured tag,
// along with the Consul index for blocking queries.
func (w *Watcher) ListServices(ctx context.Context, waitIndex uint64) ([]string, uint64, error) {
	url := fmt.Sprintf("%s/v1/catalog/services?tag=%s&index=%d&wait=5m", w.addr, w.tag, waitIndex)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}
	if w.token != "" {
		req.Header.Set("X-Consul-Token", w.token)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("querying consul: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, 0, fmt.Errorf("consul returned %d: %s", resp.StatusCode, string(body))
	}

	newIndex, err := strconv.ParseUint(resp.Header.Get("X-Consul-Index"), 10, 64)
	if err != nil || newIndex == 0 {
		// Consul indexes start at 1; using 0 would make the next blocking
		// query return immediately, causing a tight loop.
		newIndex = 1
	}

	var catalog catalogServicesResponse
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, 0, fmt.Errorf("decoding response: %w", err)
	}

	var names []string
	for name := range catalog {
		// Skip the built-in "consul" service
		if name == "consul" {
			continue
		}
		names = append(names, name)
	}

	return names, newIndex, nil
}

// GetServiceInstances returns healthy instances for a named service.
func (w *Watcher) GetServiceInstances(ctx context.Context, serviceName string) ([]ServiceInstance, error) {
	url := fmt.Sprintf("%s/v1/health/service/%s?passing=true", w.addr, serviceName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if w.token != "" {
		req.Header.Set("X-Consul-Token", w.token)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying consul: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("consul returned %d: %s", resp.StatusCode, string(body))
	}

	var entries []healthServiceEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	var instances []ServiceInstance
	for _, e := range entries {
		addr := e.Service.Address
		if addr == "" {
			addr = e.Node.Address
		}
		instances = append(instances, ServiceInstance{
			ServiceName: e.Service.Service,
			Address:     addr,
			Port:        e.Service.Port,
			Tags:        e.Service.Tags,
		})
	}

	return instances, nil
}

// WatchServices starts watching Consul for service changes and sends full
// state snapshots on the returned channel whenever changes are detected.
func (w *Watcher) WatchServices(ctx context.Context) (<-chan []ServiceState, error) {
	ch := make(chan []ServiceState, 1)

	go func() {
		defer close(ch)

		var waitIndex uint64
		backoff := time.Second

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			names, newIndex, err := w.ListServices(ctx, waitIndex)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Error("failed to list consul services", "error", err, "backoff", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			backoff = time.Second

			// Only fetch instances if index changed (or first poll)
			if newIndex == waitIndex && waitIndex != 0 {
				continue
			}
			waitIndex = newIndex

			slog.Info("consul services changed", "services", names, "index", newIndex)

			var states []ServiceState
			for _, name := range names {
				instances, err := w.GetServiceInstances(ctx, name)
				if err != nil {
					slog.Error("failed to get service instances", "service", name, "error", err)
					// Include the service with nil instances so the syncer
					// still sees it in the desired set and won't orphan-delete it.
					states = append(states, ServiceState{
						Name:      name,
						Instances: nil,
					})
					continue
				}
				states = append(states, ServiceState{
					Name:      name,
					Instances: instances,
				})
			}

			select {
			case ch <- states:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// FetchAllServices does a single non-blocking fetch of all tagged services and their instances.
func (w *Watcher) FetchAllServices(ctx context.Context) ([]ServiceState, error) {
	names, _, err := w.ListServices(ctx, 0)
	if err != nil {
		return nil, err
	}

	var states []ServiceState
	for _, name := range names {
		instances, err := w.GetServiceInstances(ctx, name)
		if err != nil {
			slog.Error("failed to get service instances during resync", "service", name, "error", err)
			// Include the service with nil instances so the syncer
			// still sees it in the desired set and won't orphan-delete it.
			states = append(states, ServiceState{
				Name:      name,
				Instances: nil,
			})
			continue
		}
		states = append(states, ServiceState{
			Name:      name,
			Instances: instances,
		})
	}
	return states, nil
}
