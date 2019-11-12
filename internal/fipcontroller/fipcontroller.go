package fipcontroller

import (
	"context"
	"sync"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/sirupsen/logrus"

	"github.com/costela/hcloud-ip-floater/internal/config"
)

type Controller struct {
	logger       logrus.FieldLogger
	hcloudClient *hcloud.Client
	attachments  map[string]string
	mu           sync.RWMutex
}

func New(logger logrus.FieldLogger, hcc *hcloud.Client) *Controller {
	fc := &Controller{
		logger:       logger.WithField("component", "fipcontroller"),
		hcloudClient: hcc,
		attachments:  make(map[string]string),
	}

	return fc
}

// AttachToNode adds a FIP-to-node attachment to our worldview and immediately attempts to converge it with hcloud's
func (fc *Controller) AttachToNode(svcIPs []string, node string) {
	fc.mu.Lock()
	for _, ip := range svcIPs {
		fc.attachments[ip] = node
	}
	fc.mu.Unlock()

	go fc.Converge()
}

func (fc *Controller) Converge() {
	fips, err := fc.hcloudClient.FloatingIP.AllWithOpts(context.Background(), hcloud.FloatingIPListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: config.Global.FloatingLabelSelector,
		},
	})
	if err != nil {
		fc.logger.WithError(err).Errorf("could not fetch FIPs")
		return
	}

	toAttach := fc.getIPs()

	for _, fip := range fips {
		fc.mu.RLock()
		node, ok := fc.attachments[fip.IP.String()]
		fc.mu.RUnlock()
		if !ok {
			// FIP not known to us; ignore
			fc.logger.WithFields(logrus.Fields{
				"fip": fip.IP.String(),
			}).Debug("ignoring unattached floating IP")
			continue
		}

		// resolve Server reference (API returns only empty struct with ID)
		// TODO: can we safely cache server info? Can we even support name changes?
		if fip.Server != nil {
			srv, _, err := fc.hcloudClient.Server.GetByID(context.Background(), fip.Server.ID)
			if err != nil {
				fc.logger.WithError(err).WithFields(logrus.Fields{
					"server_id": fip.Server.ID,
				}).Errorf("could not find server")
				continue
			}
			fip.Server = srv
		}

		if fip.Server == nil || fip.Server.Name != node {
			err := fc.attachFIPToNode(fip, node)
			if err != nil {
				fc.logger.WithError(err).WithFields(logrus.Fields{
					"fip":  fip.IP.String(),
					"node": node,
				}).Error("could not attach floating IP")
			}
		} else {
			fc.logger.WithFields(logrus.Fields{
				"fip":  fip.IP.String(),
				"node": node,
			}).Info("floating IP already attached")
		}
		delete(toAttach, fip.IP.String())
	}
	for ip := range toAttach {
		fc.logger.WithFields(logrus.Fields{
			"fip": ip,
		}).Warn("could not find floating IP")
	}
}

func (fc *Controller) getIPs() map[string]struct{} {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	ips := make(map[string]struct{})
	for ip := range fc.attachments {
		ips[ip] = struct{}{}
	}

	return ips
}

func (fc *Controller) attachFIPToNode(fip *hcloud.FloatingIP, node string) error {
	server, _, err := fc.hcloudClient.Server.GetByName(context.Background(), node)
	if err != nil {
		return err
	}

	act, _, err := fc.hcloudClient.FloatingIP.Assign(context.Background(), fip, server)
	if err != nil {
		return err
	}

	_, errc := fc.hcloudClient.Action.WatchProgress(context.Background(), act)
	return <-errc
}
