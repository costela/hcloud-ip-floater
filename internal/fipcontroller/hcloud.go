package fipcontroller

import (
	"context"

	"github.com/hetznercloud/hcloud-go/hcloud"
)

// HcloudClienter wraps a thin interface around the hcloud.Client to make it more easily mockable
type hcloudClienter interface {
	FloatingIP() hcloudFloatingIPer
	Server() hcloudServerer
	Action() hcloudActioner
}

type hcloudClient struct {
	*hcloud.Client
}

func (hcc hcloudClient) FloatingIP() hcloudFloatingIPer {
	return &hcc.Client.FloatingIP
}

func (hcc hcloudClient) Server() hcloudServerer {
	return &hcc.Client.Server
}

func (hcc hcloudClient) Action() hcloudActioner {
	return &hcc.Client.Action
}

type hcloudFloatingIPer interface {
	AllWithOpts(context.Context, hcloud.FloatingIPListOpts) ([]*hcloud.FloatingIP, error)
	Assign(context.Context, *hcloud.FloatingIP, *hcloud.Server) (*hcloud.Action, *hcloud.Response, error)
}

type hcloudServerer interface {
	GetByID(context.Context, int) (*hcloud.Server, *hcloud.Response, error)
	GetByName(context.Context, string) (*hcloud.Server, *hcloud.Response, error)
}

type hcloudActioner interface {
	WatchProgress(context.Context, *hcloud.Action) (<-chan int, <-chan error)
}
