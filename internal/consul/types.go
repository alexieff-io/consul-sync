package consul

// ServiceInstance represents a single healthy instance of a Consul service.
type ServiceInstance struct {
	ServiceName string
	Address     string
	Port        int
	Tags        []string
}

// ServiceState represents a Consul service and all its healthy instances.
type ServiceState struct {
	Name      string
	Instances []ServiceInstance
}
