// Copyright 2021 FabEdge Team
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connector

import (
	"encoding/json"
	"github.com/fabedge/fabedge/pkg/util/memberlist"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"k8s.io/klog/v2"

	"github.com/fabedge/fabedge/pkg/common/about"
	"github.com/fabedge/fabedge/pkg/connector/routing"
	"github.com/fabedge/fabedge/pkg/tunnel"
	"github.com/fabedge/fabedge/pkg/tunnel/strongswan"
	"github.com/fabedge/fabedge/pkg/util/ipset"
)

type Manager struct {
	Config
	tm          tunnel.Manager
	ipt         *iptables.IPTables
	connections []tunnel.ConnConfig
	ipset       ipset.Interface
	router      routing.Routing
	mc          *memberlist.Client
}

type Config struct {
	SyncPeriod       time.Duration //sync interval
	DebounceDuration time.Duration
	TunnelConfigFile string
	CertFile         string
	ViciSocket       string
	CNIType          string
	initMembers      []string
}

func msgHandler(b []byte) {
}

func nodeLeveHandler(name string) {
}

func (c Config) Manager() (*Manager, error) {
	tm, err := strongswan.New(
		strongswan.SocketFile(c.ViciSocket),
		strongswan.StartAction("none"),
	)
	if err != nil {
		return nil, err
	}

	ipt, err := iptables.New()
	if err != nil {
		return nil, err
	}

	router, err := routing.GetRouter(c.CNIType)
	if err != nil {
		return nil, err
	}

	mc, err := memberlist.New(c.initMembers, msgHandler, nodeLeveHandler)
	if err != nil {
		return nil, err
	}

	return &Manager{
		Config: c,
		tm:     tm,
		ipt:    ipt,
		ipset:  ipset.New(),
		router: router,
		mc:     mc,
	}, nil
}

func runTasks(interval time.Duration, handler ...func()) {
	t := time.Tick(interval)
	for {
		for _, h := range handler {
			h()
		}
		<-t
	}
}

func (m *Manager) Start() {
	routeTaskFn := func() {
		active, err := m.tm.IsActive()
		if err != nil {
			klog.Errorf("failed to get tunnel manager status: %s", err)
			return
		}
		if active {
			if err = m.router.SyncRoutes(m.connections); err != nil {
				klog.Errorf("failed to sync routes: %s", err)
				return
			}
		} else {
			if err = m.router.CleanRoutes(m.connections); err != nil {
				klog.Errorf("failed to clean routes: %s", err)
				return
			}
		}

		klog.Info("routes are synced")
	}

	iptablesTaskFn := func() {
		if err := m.ensureForwardIPTablesRules(); err != nil {
			klog.Errorf("error when to add iptables forward rules: %s", err)
		} else {
			klog.Infof("iptables forward rules are added")
		}

		if err := m.ensureNatIPTablesRules(); err != nil {
			klog.Errorf("error when to add iptables nat rules: %s", err)
		} else {
			klog.Infof("iptables nat rules are added")
		}

		if err := m.ensureInputIPTablesRules(); err != nil {
			klog.Errorf("error when to add iptables input rules: %s", err)
		} else {
			klog.Infof("iptables input rules are added")
		}
	}

	// Connector broadcasts the active routing info to all cloud agents.
	broadcastToAgents := func() {
		cp, err := m.router.GetConnectorPrefixes()
		if err != nil {
			klog.Errorf("failed to get connector prefixes:%s", err)
			return
		}
		klog.V(5).Infof("get connector prefixes:%+v", cp)

		if len(cp.RemotePrefixes) < 1 || len(cp.LocalPrefixes) < 1 {
			return
		}

		b, err := json.Marshal(cp)
		if err != nil {
			klog.Errorf("failed to marshal prefixes:%s", err)
		}

		m.mc.Broadcast(b)
	}

	tunnelTaskFn := func() {
		if err := m.syncConnections(); err != nil {
			klog.Errorf("error when to sync tunnels: %s", err)
		} else {
			broadcastToAgents()
			klog.Infof("tunnels are synced")
		}
	}

	ipsetTaskFn := func() {
		if err := m.syncEdgeNodeCIDRSet(); err != nil {
			klog.Errorf("error when to sync ipset %s: %s", IPSetEdgeNodeCIDR, err)
		} else {
			klog.Infof("ipset %s are synced", IPSetEdgeNodeCIDR)
		}

		if err := m.syncCloudPodCIDRSet(); err != nil {
			klog.Errorf("error when to sync ipset %s: %s", IPSetCloudPodCIDR, err)
		} else {
			klog.Infof("ipset %s are synced", IPSetCloudPodCIDR)
		}

		if err := m.syncCloudNodeCIDRSet(); err != nil {
			klog.Errorf("error when to sync ipset %s: %s", IPSetCloudNodeCIDR, err)
		} else {
			klog.Infof("ipset %s are synced", IPSetCloudNodeCIDR)
		}

		if err := m.syncEdgePodCIDRSet(); err != nil {
			klog.Errorf("error when to sync ipset %s: %s", IPSetEdgePodCIDR, err)
		} else {
			klog.Infof("ipset %s are synced", IPSetEdgePodCIDR)
		}
	}
	tasks := []func(){tunnelTaskFn, routeTaskFn, ipsetTaskFn, iptablesTaskFn}

	if err := m.clearFabedgeIptablesChains(); err != nil {
		klog.Errorf("failed to clean iptables: %s", err)
	}

	// repeats regular tasks periodically
	go runTasks(m.SyncPeriod, tasks...)

	// sync ALL when config file changed
	go m.onConfigFileChange(m.TunnelConfigFile, tasks...)

	about.DisplayVersion()
	klog.Info("manager started")
	klog.V(5).Infof("config:%+v", m.Config)

	// wait os signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	m.gracefulShutdown()
	klog.Info("connector stopped")
}

func (m *Manager) gracefulShutdown() {
	err := m.router.CleanRoutes(m.connections)
	if err != nil {
		klog.Errorf("failed to clean routers: %s", err)
	}

	err = m.CleanSNatIPTablesRules()
	if err != nil {
		klog.Errorf("failed to clean iptables: %s", err)
	}
}
