package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/alexieff-io/consul-sync/internal/consul"
	"github.com/alexieff-io/consul-sync/internal/metrics"
)

const (
	fieldManager = "consul-sync"
	managedByKey = "app.kubernetes.io/managed-by"
	managedBy    = "consul-sync"
)

// Syncer creates and manages Kubernetes Services and EndpointSlices.
type Syncer struct {
	client    kubernetes.Interface
	namespace string
}

// NewSyncer creates a new Kubernetes syncer.
func NewSyncer(client kubernetes.Interface, namespace string) *Syncer {
	return &Syncer{
		client:    client,
		namespace: namespace,
	}
}

// Sync reconciles Kubernetes resources to match the given Consul service states.
func (s *Syncer) Sync(ctx context.Context, services []consul.ServiceState) error {
	desired := make(map[string]bool)
	var totalEndpoints int

	for _, svc := range services {
		name := sanitizeName(svc.Name)
		desired[name] = true

		if len(svc.Instances) == 0 {
			slog.Warn("skipping service with no healthy instances", "service", svc.Name)
			continue
		}

		port := int32(svc.Instances[0].Port)
		totalEndpoints += len(svc.Instances)

		if err := s.applyService(ctx, name, port); err != nil {
			metrics.KubernetesErrors.Inc()
			return fmt.Errorf("applying service %s: %w", name, err)
		}

		if err := s.applyEndpointSlice(ctx, name, port, svc.Instances); err != nil {
			metrics.KubernetesErrors.Inc()
			return fmt.Errorf("applying endpointslice %s: %w", name, err)
		}

		slog.Info("synced service", "service", name, "endpoints", len(svc.Instances))
	}

	// Cleanup orphaned resources
	if err := s.cleanup(ctx, desired); err != nil {
		metrics.KubernetesErrors.Inc()
		return fmt.Errorf("cleaning up orphans: %w", err)
	}

	metrics.SyncedServices.Set(float64(len(desired)))
	metrics.SyncedEndpoints.Set(float64(totalEndpoints))

	return nil
}

func (s *Syncer) applyService(ctx context.Context, name string, port int32) error {
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels: map[string]string{
				managedByKey:             managedBy,
				"app.kubernetes.io/name": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     port,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	data, err := json.Marshal(svc)
	if err != nil {
		return fmt.Errorf("marshaling service: %w", err)
	}

	_, err = s.client.CoreV1().Services(s.namespace).Patch(
		ctx, name, types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager},
	)
	return err
}

func (s *Syncer) applyEndpointSlice(ctx context.Context, name string, port int32, instances []consul.ServiceInstance) error {
	sliceName := name + "-consul"
	protocol := corev1.ProtocolTCP
	portName := "http"
	ready := true

	var endpoints []discoveryv1.Endpoint
	for _, inst := range instances {
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses: []string{inst.Address},
			Conditions: discoveryv1.EndpointConditions{
				Ready: &ready,
			},
		})
	}

	eps := &discoveryv1.EndpointSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "discovery.k8s.io/v1",
			Kind:       "EndpointSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sliceName,
			Namespace: s.namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name":                  name,
				"endpointslice.kubernetes.io/managed-by":      managedBy,
				managedByKey:                                  managedBy,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     &portName,
				Port:     &port,
				Protocol: &protocol,
			},
		},
	}

	data, err := json.Marshal(eps)
	if err != nil {
		return fmt.Errorf("marshaling endpointslice: %w", err)
	}

	_, err = s.client.DiscoveryV1().EndpointSlices(s.namespace).Patch(
		ctx, sliceName, types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager},
	)
	return err
}

func (s *Syncer) cleanup(ctx context.Context, desired map[string]bool) error {
	svcs, err := s.client.CoreV1().Services(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByKey + "=" + managedBy,
	})
	if err != nil {
		return fmt.Errorf("listing managed services: %w", err)
	}

	for _, svc := range svcs.Items {
		if desired[svc.Name] {
			continue
		}

		slog.Info("deleting orphaned service", "service", svc.Name)

		// Delete the EndpointSlice first
		sliceName := svc.Name + "-consul"
		err := s.client.DiscoveryV1().EndpointSlices(s.namespace).Delete(ctx, sliceName, metav1.DeleteOptions{})
		if err != nil {
			slog.Error("failed to delete endpointslice", "name", sliceName, "error", err)
		}

		// Delete the Service
		err = s.client.CoreV1().Services(s.namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("deleting service %s: %w", svc.Name, err)
		}
	}

	return nil
}

var invalidChars = regexp.MustCompile(`[^a-z0-9-]`)

// sanitizeName converts a Consul service name into a valid Kubernetes name.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = invalidChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
