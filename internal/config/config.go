package config

var Global struct {
	LogLevel              string `id:"log-level" short:"l" desc:"verbosity level for logs" default:"warn"`
	HCloudToken           string `id:"hcloud-token" desc:"API token for HCloud access"`
	ServiceLabelSelector  string `id:"service-label-selector" desc:"label selector used to match services" default:"hcloud-ip-floater.cstl.dev/ignore!=true"`
	FloatingLabelSelector string `id:"floating-label-selector" desc:"label selector used to match floating IPs" default:""`

	// optional MetalLB integration
	MetalLBNamespace  string `id:"metallb-namespace" desc:"namespace to create MetalLB ConfigMap"`
	MetalLBConfigName string `id:"metallb-config-name" desc:"name of ConfigMap resource used by MetalLB"`

	SyncSeconds int `id:"sync-interval" desc:"interval to sync with k8s and poll from hcloud" default:"300" opts:"hidden"`
}
