package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/alexieff-io/consul-sync/internal/consul"
	"github.com/alexieff-io/consul-sync/internal/metrics"
)

const (
	fieldManager = "consul-sync"
	managedByKey = "app.kubernetes.io/managed-by"
	managedBy    = "consul-sync"
)

var httpRouteGVR = schema.GroupVersionResource{
	Group:    "gateway.networking.k8s.io",
	Version:  "v1",
	Resource: "httproutes",
}

// HTTPRouteConfig holds configuration for auto-generated HTTPRoute resources.
type HTTPRouteConfig struct {
	Enabled          bool
	DomainSuffix     string
	InternalGateway  string
	ExternalGateway  string
	GatewayNamespace string
	GatewayListener  string
	InternalTag      string
	ExternalTag      string
}

// Syncer creates and manages Kubernetes Services and EndpointSlices.
type Syncer struct {
	client    kubernetes.Interface
	dynClient dynamic.Interface
	namespace string
	routeCfg  HTTPRouteConfig
}

// NewSyncer creates a new Kubernetes syncer.
func NewSyncer(client kubernetes.Interface, dynClient dynamic.Interface, namespace string, routeCfg HTTPRouteConfig) *Syncer {
	return &Syncer{
		client:    client,
		dynClient: dynClient,
		namespace: namespace,
		routeCfg:  routeCfg,
	}
}

// Sync reconciles Kubernetes resources to match the given Consul service states.
func (s *Syncer) Sync(ctx context.Context, services []consul.ServiceState) error {
	desired := make(map[string]bool)
	desiredRoutes := make(map[string]bool)
	var totalEndpoints int
	var routeCount int
	var syncErrors []error

	for _, svc := range services {
		name := sanitizeName(svc.Name)
		desired[name] = true

		if len(svc.Instances) == 0 {
			slog.Warn("skipping service with no healthy instances", "service", svc.Name)
			continue
		}

		port := int32(svc.Instances[0].Port)
		if port < 1 || port > 65535 {
			slog.Warn("skipping service with invalid port", "service", svc.Name, "port", port)
			continue
		}
		totalEndpoints += len(svc.Instances)

		if err := s.applyService(ctx, name, port); err != nil {
			metrics.KubernetesErrors.Inc()
			slog.Error("failed to apply service, skipping", "service", name, "error", err)
			syncErrors = append(syncErrors, fmt.Errorf("applying service %s: %w", name, err))
			continue
		}

		if err := s.applyEndpointSlice(ctx, name, port, svc.Instances); err != nil {
			metrics.KubernetesErrors.Inc()
			slog.Error("failed to apply endpointslice, skipping", "service", name, "error", err)
			syncErrors = append(syncErrors, fmt.Errorf("applying endpointslice %s: %w", name, err))
			continue
		}

		// Create HTTPRoutes based on service tags
		if s.routeCfg.Enabled {
			if hasTag(svc.Tags, s.routeCfg.InternalTag) {
				routeName := name + "-" + s.routeCfg.InternalGateway
				desiredRoutes[routeName] = true
				if err := s.applyHTTPRoute(ctx, name, port, s.routeCfg.InternalGateway); err != nil {
					metrics.KubernetesErrors.Inc()
					slog.Error("failed to apply httproute, skipping", "service", name, "gateway", s.routeCfg.InternalGateway, "error", err)
					syncErrors = append(syncErrors, fmt.Errorf("applying httproute %s: %w", routeName, err))
				} else {
					routeCount++
				}
			}
			if hasTag(svc.Tags, s.routeCfg.ExternalTag) {
				routeName := name + "-" + s.routeCfg.ExternalGateway
				desiredRoutes[routeName] = true
				if err := s.applyHTTPRoute(ctx, name, port, s.routeCfg.ExternalGateway); err != nil {
					metrics.KubernetesErrors.Inc()
					slog.Error("failed to apply httproute, skipping", "service", name, "gateway", s.routeCfg.ExternalGateway, "error", err)
					syncErrors = append(syncErrors, fmt.Errorf("applying httproute %s: %w", routeName, err))
				} else {
					routeCount++
				}
			}
		}

		slog.Info("synced service", "service", name, "endpoints", len(svc.Instances))
	}

	// Cleanup orphaned resources
	if err := s.cleanup(ctx, desired); err != nil {
		metrics.KubernetesErrors.Inc()
		syncErrors = append(syncErrors, fmt.Errorf("cleaning up orphans: %w", err))
	}

	if s.routeCfg.Enabled {
		if err := s.cleanupHTTPRoutes(ctx, desiredRoutes); err != nil {
			metrics.KubernetesErrors.Inc()
			syncErrors = append(syncErrors, fmt.Errorf("cleaning up orphan httproutes: %w", err))
		}
		metrics.SyncedHTTPRoutes.Set(float64(routeCount))
	}

	metrics.SyncedServices.Set(float64(len(desired)))
	metrics.SyncedEndpoints.Set(float64(totalEndpoints))

	return errors.Join(syncErrors...)
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
				"kubernetes.io/service-name":             name,
				"endpointslice.kubernetes.io/managed-by": managedBy,
				managedByKey:                             managedBy,
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

func (s *Syncer) applyHTTPRoute(ctx context.Context, serviceName string, port int32, gatewayName string) error {
	routeName := serviceName + "-" + gatewayName
	hostname := serviceName + "." + s.routeCfg.DomainSuffix

	route := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":      routeName,
				"namespace": s.namespace,
				"labels": map[string]interface{}{
					managedByKey:             managedBy,
					"app.kubernetes.io/name": serviceName,
				},
			},
			"spec": map[string]interface{}{
				"parentRefs": []interface{}{
					map[string]interface{}{
						"name":        gatewayName,
						"namespace":   s.routeCfg.GatewayNamespace,
						"sectionName": s.routeCfg.GatewayListener,
					},
				},
				"hostnames": []interface{}{
					hostname,
				},
				"rules": []interface{}{
					map[string]interface{}{
						"backendRefs": []interface{}{
							map[string]interface{}{
								"name": serviceName,
								"port": int64(port),
							},
						},
					},
				},
			},
		},
	}

	data, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("marshaling httproute: %w", err)
	}

	_, err = s.dynClient.Resource(httpRouteGVR).Namespace(s.namespace).Patch(
		ctx, routeName, types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager},
	)
	if err != nil {
		return fmt.Errorf("applying httproute %s: %w", routeName, err)
	}

	slog.Info("applied httproute", "route", routeName, "gateway", gatewayName, "hostname", hostname)
	return nil
}

func (s *Syncer) cleanupHTTPRoutes(ctx context.Context, desiredRoutes map[string]bool) error {
	routes, err := s.dynClient.Resource(httpRouteGVR).Namespace(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByKey + "=" + managedBy,
	})
	if err != nil {
		return fmt.Errorf("listing managed httproutes: %w", err)
	}

	for _, route := range routes.Items {
		if desiredRoutes[route.GetName()] {
			continue
		}

		slog.Info("deleting orphaned httproute", "route", route.GetName())
		if err := s.dynClient.Resource(httpRouteGVR).Namespace(s.namespace).Delete(ctx, route.GetName(), metav1.DeleteOptions{}); err != nil {
			slog.Error("failed to delete httproute", "name", route.GetName(), "error", err)
		}
	}

	return nil
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

func hasTag(tags []string, target string) bool {
	for _, t := range tags {
		if t == target {
			return true
		}
	}
	return false
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
