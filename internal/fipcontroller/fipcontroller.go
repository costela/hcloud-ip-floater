package fipcontroller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/costela/hcloud-ip-floater/internal/config"
	"github.com/costela/hcloud-ip-floater/internal/stringset"
)

type Controller struct {
	logger       logrus.FieldLogger
	hcloudClient hcloudClienter

	attachments map[string]string
	attMu       sync.RWMutex

	fips   map[string]*hcloud.FloatingIP
	fipsMu sync.RWMutex

	sf singleflight.Group
}

func New(logger logrus.FieldLogger, hcc *hcloud.Client) *Controller {
	fc := &Controller{
		logger:       logger.WithField("component", "fipcontroller"),
		hcloudClient: hcloudClient{hcc}, // wrap in mock-helper
		attachments:  make(map[string]string),
		fips:         make(map[string]*hcloud.FloatingIP),
	}

	return fc
}

func (fc *Controller) Run() {
	for {
		<-time.NewTicker(time.Duration(config.Global.SyncSeconds) * time.Second).C

		if changed, err := fc.syncFloatingIPs(); err != nil {
			fc.logger.WithError(err).Error("could not sync floating IPs")
		} else if changed {
			fc.logger.Info("floating IPs changed")
			fc.Reconcile()
		}
	}

}

// AttachToNode adds a FIP-to-node attachment to our worldview and immediately attempts to reconcile it with hcloud's
func (fc *Controller) AttachToNode(svcIPs stringset.StringSet, node string) {
	fc.attMu.Lock()
	defer fc.attMu.Unlock()

	var changedAttachment bool
	for ip := range svcIPs {
		if oldNode, found := fc.attachments[ip]; !found || node != oldNode {
			fc.attachments[ip] = node
			changedAttachment = true
		}
	}

	if changedAttachment {
		_, err := fc.syncFloatingIPs()
		if err != nil {
			fc.logger.WithError(err).Error("could not fetch FIPs")
			return
		}
		fc.Reconcile()
	}
}

// ForgetAttachments remove the desired attachment from our worldview. This avoids "stealing" stale attachments from
// FIPs that might be known to us, but not currently in use by us.
// This does not trigger actual FIP dettachment.
func (fc *Controller) ForgetAttachments(svcIPs stringset.StringSet) {
	fc.attMu.Lock()
	defer fc.attMu.Unlock()

	for ip := range svcIPs {
		delete(fc.attachments, ip)
	}
}

func (fc *Controller) syncFloatingIPs() (bool, error) {
	fips, err := fc.hcloudClient.FloatingIP().AllWithOpts(context.Background(), hcloud.FloatingIPListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: config.Global.FloatingLabelSelector,
		},
	})
	if err != nil {
		return false, err
	}

	fc.fipsMu.Lock()
	defer fc.fipsMu.Unlock()

	var changedFIPs bool

	seenFIPs := make(stringset.StringSet)

	for _, fip := range fips {
		ip := fip.IP.String()

		seenFIPs.Add(ip)
		oldFIP := fc.fips[ip]

		if oldFIP == nil || fc.fipChanged(oldFIP, fip) {
			// resolve Server reference (API returns only empty struct with ID)
			// TODO: can we safely cache server info? Can we even support name changes?
			if fip.Server != nil {
				srv, _, err := fc.hcloudClient.Server().GetByID(context.Background(), fip.Server.ID)
				if err != nil {
					fc.logger.WithError(err).WithFields(logrus.Fields{
						"server_id": fip.Server.ID,
					}).Error("could not find server")
					continue
				}
				fip.Server = srv
			}

			fc.fips[ip] = fip
			changedFIPs = true
		} else if (oldFIP.Server != nil && oldFIP.Server.Name != fc.attachments[ip]) || (oldFIP.Server == nil && fc.attachments[ip] != "") {
			// FIP hasn't changed but attachment doesn't match so let's reconcile
			changedFIPs = true
		}
	}

	for fip := range fc.fips {
		if !seenFIPs.Has(fip) {
			delete(fc.fips, fip)
			changedFIPs = true
		}
	}

	return changedFIPs, nil
}

// Reconcile starts an asynchronous attempt to make the managed floating IPs match the controller's worldview about
// which attachments should be current.
func (fc *Controller) Reconcile() {
	_ = fc.sf.DoChan("reconciliation", func() (interface{}, error) {
		fc.logger.Info("starting reconciliation")

		toAttach := fc.getServiceIPs()

		fc.fipsMu.RLock()
		defer fc.fipsMu.RUnlock()

		for ip, fip := range fc.fips {
			fc.attMu.RLock()
			node, found := fc.attachments[ip]
			fc.attMu.RUnlock()
			if !found {
				// FIP not known to us; ignore
				fc.logger.WithFields(logrus.Fields{
					"fip": ip,
				}).Debug("ignoring unattached floating IP")
				continue
			}

			if fip.Server == nil || fip.Server.Name != node {
				err := fc.attachFIPToNode(fip, node)
				if err != nil {
					fc.logger.WithError(err).WithFields(logrus.Fields{
						"fip":  ip,
						"node": node,
					}).Error("could not attach floating IP")
				} else {
					fc.logger.WithFields(logrus.Fields{
						"fip":  ip,
						"node": node,
					}).Info("attached floating IP")
				}
			} else {
				fc.logger.WithFields(logrus.Fields{
					"fip":  ip,
					"node": node,
				}).Info("floating IP already attached")
			}
			delete(toAttach, ip)
		}
		for ip := range toAttach {
			fc.logger.WithFields(logrus.Fields{
				"fip": ip,
			}).Warn("could not find floating IP")
		}

		fc.logger.Info("reconciliation done")

		return nil, nil
	})
}

func (fc *Controller) fipChanged(oldFIP *hcloud.FloatingIP, newFIP *hcloud.FloatingIP) bool {
	if oldFIP.ID != newFIP.ID {
		return true
	}

	if oldFIP.Server != newFIP.Server {
		if oldFIP.Server != nil && newFIP.Server != nil && oldFIP.Server.ID == newFIP.Server.ID {
			return false
		}

		return true
	}

	return false
}

func (fc *Controller) getServiceIPs() stringset.StringSet {
	fc.attMu.RLock()
	defer fc.attMu.RUnlock()

	ips := make(stringset.StringSet)
	for ip := range fc.attachments {
		ips.Add(ip)
	}

	return ips
}

func (fc *Controller) attachFIPToNode(fip *hcloud.FloatingIP, node string) error {
	server, _, err := fc.hcloudClient.Server().GetByName(context.Background(), node)
	if err != nil {
		return err
	}

	// extra safety for https://github.com/costela/hcloud-ip-floater/issues/8
	if server == nil {
		return fmt.Errorf("could not find node %s", node)
	}

	act, _, err := fc.hcloudClient.FloatingIP().Assign(context.Background(), fip, server)
	if err != nil {
		return err
	}

	_, errc := fc.hcloudClient.Action().WatchProgress(context.Background(), act)
	return <-errc
}
