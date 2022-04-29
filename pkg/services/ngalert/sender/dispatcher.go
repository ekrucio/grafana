package sender

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/benbjohnson/clock"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/notifier"
	"github.com/grafana/grafana/pkg/services/ngalert/state"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
)

type Dispatcher struct {
	adminConfigMtx   sync.RWMutex
	logger           log.Logger
	clock            clock.Clock
	adminConfigStore store.AdminConfigurationStore

	// Senders help us send alerts to external Alertmanagers.
	senders          map[int64]*Sender
	sendersCfgHash   map[int64]string
	multiOrgNotifier *notifier.MultiOrgAlertmanager
	sendAlertsTo     map[int64]models.AlertmanagersChoice

	appURL                  *url.URL
	disabledOrgs            map[int64]struct{}
	adminConfigPollInterval time.Duration
}

func NewDispatcher(multiOrgNotifier *notifier.MultiOrgAlertmanager, store store.AdminConfigurationStore, clk clock.Clock, appURL *url.URL, disabledOrgs map[int64]struct{}, configPollInterval time.Duration) *Dispatcher {
	d := &Dispatcher{
		adminConfigMtx:   sync.RWMutex{},
		logger:           log.New("ngalert-notifications-dispatcher"),
		clock:            clk,
		adminConfigStore: store,

		senders:          map[int64]*Sender{},
		sendersCfgHash:   map[int64]string{},
		multiOrgNotifier: multiOrgNotifier,
		sendAlertsTo:     map[int64]models.AlertmanagersChoice{},

		appURL:                  appURL,
		disabledOrgs:            disabledOrgs,
		adminConfigPollInterval: configPollInterval,
	}
	return d
}

func (d *Dispatcher) adminConfigSync(ctx context.Context) error {
	for {
		select {
		case <-time.After(d.adminConfigPollInterval):
			if err := d.SyncAndApplyConfigFromDatabase(); err != nil {
				d.logger.Error("unable to sync admin configuration", "err", err)
			}
		case <-ctx.Done():
			// Stop sending alerts to all external Alertmanager(s).
			d.adminConfigMtx.Lock()
			for orgID, s := range d.senders {
				delete(d.senders, orgID) // delete before we stop to make sure we don't accept any more alerts.
				s.Stop()
			}
			d.adminConfigMtx.Unlock()

			return nil
		}
	}
}

// SyncAndApplyConfigFromDatabase looks for the admin configuration in the database
// and adjusts the sender(s) and alert handling mechanism accordingly.
func (d *Dispatcher) SyncAndApplyConfigFromDatabase() error {
	d.logger.Debug("start of admin configuration sync")
	cfgs, err := d.adminConfigStore.GetAdminConfigurations()
	if err != nil {
		return err
	}

	d.logger.Debug("found admin configurations", "count", len(cfgs))

	orgsFound := make(map[int64]struct{}, len(cfgs))
	d.adminConfigMtx.Lock()
	for _, cfg := range cfgs {
		_, isDisabledOrg := d.disabledOrgs[cfg.OrgID]
		if isDisabledOrg {
			d.logger.Debug("skipping starting sender for disabled org", "org", cfg.OrgID)
			continue
		}

		// Update the Alertmanagers choice for the organization.
		d.sendAlertsTo[cfg.OrgID] = cfg.SendAlertsTo

		orgsFound[cfg.OrgID] = struct{}{} // keep track of the which senders we need to keep.

		existing, ok := d.senders[cfg.OrgID]

		// We have no running sender and no Alertmanager(s) configured, no-op.
		if !ok && len(cfg.Alertmanagers) == 0 {
			d.logger.Debug("no external alertmanagers configured", "org", cfg.OrgID)
			continue
		}
		//  We have no running sender and alerts are handled internally, no-op.
		if !ok && cfg.SendAlertsTo == models.InternalAlertmanager {
			d.logger.Debug("alerts are handled internally", "org", cfg.OrgID)
			continue
		}

		// We have a running sender but no Alertmanager(s) configured, shut it down.
		if ok && len(cfg.Alertmanagers) == 0 {
			d.logger.Debug("no external alertmanager(s) configured, sender will be stopped", "org", cfg.OrgID)
			delete(orgsFound, cfg.OrgID)
			continue
		}

		// We have a running sender, check if we need to apply a new config.
		if ok {
			if d.sendersCfgHash[cfg.OrgID] == cfg.AsSHA256() {
				d.logger.Debug("sender configuration is the same as the one running, no-op", "org", cfg.OrgID, "alertmanagers", cfg.Alertmanagers)
				continue
			}

			d.logger.Debug("applying new configuration to sender", "org", cfg.OrgID, "alertmanagers", cfg.Alertmanagers)
			err := existing.ApplyConfig(cfg)
			if err != nil {
				d.logger.Error("failed to apply configuration", "err", err, "org", cfg.OrgID)
				continue
			}
			d.sendersCfgHash[cfg.OrgID] = cfg.AsSHA256()
			continue
		}

		// No sender and have Alertmanager(s) to send to - start a new one.
		d.logger.Info("creating new sender for the external alertmanagers", "org", cfg.OrgID, "alertmanagers", cfg.Alertmanagers)
		s, err := New()
		if err != nil {
			d.logger.Error("unable to start the sender", "err", err, "org", cfg.OrgID)
			continue
		}

		d.senders[cfg.OrgID] = s
		s.Run()

		err = s.ApplyConfig(cfg)
		if err != nil {
			d.logger.Error("failed to apply configuration", "err", err, "org", cfg.OrgID)
			continue
		}

		d.sendersCfgHash[cfg.OrgID] = cfg.AsSHA256()
	}

	sendersToStop := map[int64]*Sender{}

	for orgID, s := range d.senders {
		if _, exists := orgsFound[orgID]; !exists {
			sendersToStop[orgID] = s
			delete(d.senders, orgID)
			delete(d.sendersCfgHash, orgID)
		}
	}
	d.adminConfigMtx.Unlock()

	// We can now stop these senders w/o having to hold a lock.
	for orgID, s := range sendersToStop {
		d.logger.Info("stopping sender", "org", orgID)
		s.Stop()
		d.logger.Info("stopped sender", "org", orgID)
	}

	d.logger.Debug("finish of admin configuration sync")

	return nil
}

func (d *Dispatcher) Notify(key models.AlertRuleKey, states []*state.State) error {
	firingAlerts := FromAlertStateToPostableAlerts(states, d.appURL)
	d.notify(key, firingAlerts)
	return nil
}

func (d *Dispatcher) Expire(key models.AlertRuleKey, states []*state.State) error {
	expiredAlerts := FromAlertsStateToStoppedAlert(states, d.appURL, d.clock)
	d.notify(key, expiredAlerts)
	return nil
}

func (d *Dispatcher) notify(key models.AlertRuleKey, alerts definitions.PostableAlerts) {
	logger := d.logger.New("rule_uid", key.UID, "org", key.OrgID)
	// Send alerts to local notifier if they need to be handled internally
	// or if no external AMs have been discovered yet.
	var localNotifierExist, externalNotifierExist bool
	if d.sendAlertsTo[key.OrgID] == models.ExternalAlertmanagers && len(d.AlertmanagersFor(key.OrgID)) > 0 {
		logger.Debug("no alerts to put in the notifier")
	} else {
		logger.Debug("sending alerts to local notifier", "count", len(alerts.PostableAlerts), "alerts", alerts.PostableAlerts)
		n, err := d.multiOrgNotifier.AlertmanagerFor(key.OrgID)
		if err == nil {
			localNotifierExist = true
			if err := n.PutAlerts(alerts); err != nil {
				logger.Error("failed to put alerts in the local notifier", "count", len(alerts.PostableAlerts), "err", err)
			}
		} else {
			if errors.Is(err, notifier.ErrNoAlertmanagerForOrg) {
				logger.Debug("local notifier was not found")
			} else {
				logger.Error("local notifier is not available", "err", err)
			}
		}
	}

	// Send alerts to external Alertmanager(s) if we have a sender for this organization
	// and alerts are not being handled just internally.
	d.adminConfigMtx.RLock()
	defer d.adminConfigMtx.RUnlock()
	s, ok := d.senders[key.OrgID]
	if ok && d.sendAlertsTo[key.OrgID] != models.InternalAlertmanager {
		logger.Debug("sending alerts to external notifier", "count", len(alerts.PostableAlerts), "alerts", alerts.PostableAlerts)
		s.SendAlerts(alerts)
		externalNotifierExist = true
	}

	if !localNotifierExist && !externalNotifierExist {
		logger.Error("no external or internal notifier - alerts not delivered!", "count", len(alerts.PostableAlerts))
	}
}

// AlertmanagersFor returns all the discovered Alertmanager(s) for a particular organization.
func (d *Dispatcher) AlertmanagersFor(orgID int64) []*url.URL {
	d.adminConfigMtx.RLock()
	defer d.adminConfigMtx.RUnlock()
	s, ok := d.senders[orgID]
	if !ok {
		return []*url.URL{}
	}
	return s.Alertmanagers()
}

// DroppedAlertmanagersFor returns all the dropped Alertmanager(s) for a particular organization.
func (d *Dispatcher) DroppedAlertmanagersFor(orgID int64) []*url.URL {
	d.adminConfigMtx.RLock()
	defer d.adminConfigMtx.RUnlock()
	s, ok := d.senders[orgID]
	if !ok {
		return []*url.URL{}
	}

	return s.DroppedAlertmanagers()
}

// Run starts regular updates of the configuration
func (d *Dispatcher) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := d.adminConfigSync(ctx); err != nil {
			d.logger.Error("failure while running the admin configuration sync", "err", err)
		}
	}()
	wg.Wait()
	return nil
}