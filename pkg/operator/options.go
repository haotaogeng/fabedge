// Copyright 2021 BoCloud
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

package operator

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2/klogr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	apis "github.com/fabedge/fabedge/pkg/apis/v1alpha1"
	"github.com/fabedge/fabedge/pkg/common/constants"
	"github.com/fabedge/fabedge/pkg/operator/allocator"
	"github.com/fabedge/fabedge/pkg/operator/apiserver"
	fclient "github.com/fabedge/fabedge/pkg/operator/client"
	agentctl "github.com/fabedge/fabedge/pkg/operator/controllers/agent"
	clusterctl "github.com/fabedge/fabedge/pkg/operator/controllers/cluster"
	cmmctl "github.com/fabedge/fabedge/pkg/operator/controllers/community"
	connectorctl "github.com/fabedge/fabedge/pkg/operator/controllers/connector"
	"github.com/fabedge/fabedge/pkg/operator/controllers/ipamblockmonitor"
	proxyctl "github.com/fabedge/fabedge/pkg/operator/controllers/proxy"
	"github.com/fabedge/fabedge/pkg/operator/routines"
	storepkg "github.com/fabedge/fabedge/pkg/operator/store"
	"github.com/fabedge/fabedge/pkg/operator/types"
	certutil "github.com/fabedge/fabedge/pkg/util/cert"
	nodeutil "github.com/fabedge/fabedge/pkg/util/node"
	secretutil "github.com/fabedge/fabedge/pkg/util/secret"
	timeutil "github.com/fabedge/fabedge/pkg/util/time"
	"github.com/fabedge/fabedge/third_party/calicoapi"
)

const (
	RoleHost   = "host"
	RoleMember = "member"

	ClientTLSSecretName = "api-client-tls"
)

var dns1123Reg, _ = regexp.Compile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

type Options struct {
	Cluster string
	// ClusterRole will determine how operator will be running:
	// Host: operator will start an API server
	// Member: operator has to fetch CA cert and create certificate from host cluster's API server
	ClusterRole      string
	Namespace        string
	EdgePodCIDR      string
	EndpointIDFormat string
	EdgeLabels       map[string]string
	CNIType          string

	CASecretName     string
	CertValidPeriod  int64
	CertOrganization string
	Agent            agentctl.Config
	Connector        connectorctl.Config
	Proxy            proxyctl.Config

	ManagerOpts manager.Options

	APIServerCertFile      string
	APIServerKeyFile       string
	APIServerListenAddress string
	APIServerAddress       string
	TokenValidPeriod       time.Duration
	InitToken              string

	Store        storepkg.Interface
	PodCIDRStore types.PodCIDRStore
	NewEndpoint  types.NewEndpointFunc
	Manager      manager.Manager
	APIServer    *http.Server
	APIClient    fclient.Interface
	PrivateKey   *rsa.PrivateKey
}

func (opts *Options) AddFlags(flag *pflag.FlagSet) {
	flag.StringVar(&opts.Cluster, "cluster", "", "The name of cluster must be unique among all clusters and be a valid dns name(RFC 1123)")
	flag.StringVar(&opts.ClusterRole, "cluster-role", "host", "The role of cluster, possible values are: host, member")
	flag.StringVar(&opts.Namespace, "namespace", "fabedge", "The namespace in which operator will get or create objects, includes pods, secrets and configmaps")
	flag.StringVar(&opts.CNIType, "cni-type", "", "The CNI name in your kubernetes cluster")
	flag.StringVar(&opts.EdgePodCIDR, "edge-pod-cidr", "", "Specify range of IP addresses for the edge pod. If set, fabedge-operator will automatically allocate CIDRs for every edge node, configure this when you use Calico")
	flag.StringVar(&opts.EndpointIDFormat, "endpoint-id-format", "C=CN, O=fabedge.io, CN={node}", "the id format of tunnel endpoint")
	flag.StringToStringVar(&opts.EdgeLabels, "edge-labels", map[string]string{"node-role.kubernetes.io/edge": ""}, "Labels to filter edge nodes, e.g. key2=,key3=value3")

	flag.StringToStringVar(&opts.Connector.ConnectorLabels, "connector-labels", map[string]string{"app": "fabedge-connector"}, "The labels used to find connector pods, e.g. key2=,key3=value3")
	flag.StringSliceVar(&opts.Connector.Endpoint.PublicAddresses, "connector-public-addresses", nil, "The connector's public addresses which should be accessible for every edge node, comma separated. Takes single IPv4 addresses, DNS names")
	flag.StringSliceVar(&opts.Connector.ProvidedSubnets, "connector-subnets", nil, "The subnets of connector, mostly the CIDRs to assign pod IP and service ClusterIP")
	flag.DurationVar(&opts.Connector.SyncInterval, "connector-config-sync-interval", 5*time.Second, "The interval to synchronize connector configmap")

	flag.StringVar(&opts.Agent.AgentImage, "agent-image", "fabedge/agent:latest", "The image of agent container of agent pod")
	flag.StringVar(&opts.Agent.StrongswanImage, "agent-strongswan-image", "fabedge/strongswan:latest", "The image of strongswan container of agent pod")
	flag.StringVar(&opts.Agent.ImagePullPolicy, "agent-image-pull-policy", "IfNotPresent", "The imagePullPolicy for all containers of agent pod")
	flag.IntVar(&opts.Agent.AgentLogLevel, "agent-log-level", 3, "The log level of agent")
	flag.BoolVar(&opts.Agent.UseXfrm, "agent-use-xfrm", false, "let agent use xfrm if edge OS supports")
	flag.BoolVar(&opts.Agent.EnableProxy, "agent-enable-proxy", false, "Enable the proxy feature")
	flag.BoolVar(&opts.Agent.MasqOutgoing, "agent-masq-outgoing", false, "Determine if perform outbound NAT from edge pods to outside of the cluster")
	flag.BoolVar(&opts.Agent.EnableEdgeHairpinMode, "agent-enable-edge-hairpinmode", true, "Enable edge node pods HairpinMode")
	flag.IntVar(&opts.Agent.NetworkPluginMTU, "agent-network-plugin-mtu", 1400, "Set network plugin MTU for edge nodes")

	flag.StringVar(&opts.CASecretName, "ca-secret", "fabedge-ca", "The name of secret which contains CA's cert and key")
	flag.StringVar(&opts.CertOrganization, "cert-organization", certutil.DefaultOrganization, "The organization name for agent's cert")
	flag.Int64Var(&opts.CertValidPeriod, "cert-validity-period", 3650, "The validity period for agent's cert")

	flag.StringVar(&opts.Proxy.IPVSScheduler, "ipvs-scheduler", "rr", "The ipvs scheduler for each service")

	flag.BoolVar(&opts.ManagerOpts.LeaderElection, "leader-election", false, "Determines whether or not to use leader election")
	flag.StringVar(&opts.ManagerOpts.LeaderElectionID, "leader-election-id", "fabedge-operator-leader", "The name of the resource that leader election will use for holding the leader lock")
	opts.ManagerOpts.LeaseDuration = flag.Duration("leader-lease-duration", 15*time.Second, "The duration that non-leader candidates will wait to force acquire leadership")
	opts.ManagerOpts.RenewDeadline = flag.Duration("leader-renew-deadline", 10*time.Second, "The duration that the acting controlplane will retry refreshing leadership before giving up")
	opts.ManagerOpts.RetryPeriod = flag.Duration("leader-retry-period", 2*time.Second, "The duration that the LeaderElector clients should wait between tries of actions")

	flag.StringVar(&opts.APIServerListenAddress, "api-server-listen-address", "0.0.0.0:3030", "The address on which for API server to listen")
	flag.StringVar(&opts.APIServerAddress, "api-server-address", "", "The address of host cluster's API server")
	flag.StringVar(&opts.APIServerCertFile, "api-server-cert-file", "", "The cert file path for api server")
	flag.StringVar(&opts.APIServerKeyFile, "api-server-key-file", "", "The key file path for api server")
	flag.StringVar(&opts.InitToken, "init-token", "", "The token used to initialize TLS cert for API client")
	flag.DurationVar(&opts.TokenValidPeriod, "token-valid-period", 12*time.Hour, "The validity duration of token for child cluster to initialize")
}

func (opts *Options) Complete() (err error) {
	opts.CNIType = strings.TrimSpace(opts.CNIType)

	nodeutil.SetEdgeNodeLabels(opts.EdgeLabels)

	var (
		getEdgePodCIDRs  types.PodCIDRsGetter
		getCloudPodCIDRs types.PodCIDRsGetter
	)
	switch opts.CNIType {
	case constants.CNICalico:
		opts.Agent.EnableEdgeIPAM = true
		opts.PodCIDRStore = types.NewPodCIDRStore()
		getCloudPodCIDRs = func(node corev1.Node) []string { return opts.PodCIDRStore.Get(node.Name) }
		getEdgePodCIDRs = nodeutil.GetPodCIDRsFromAnnotation
		opts.Agent.Allocator, err = allocator.New(opts.EdgePodCIDR)
		if err != nil {
			log.Error(err, "failed to create allocator")
			return err
		}
	case constants.CNIFlannel:
		getEdgePodCIDRs = nodeutil.GetPodCIDRs
		getCloudPodCIDRs = nodeutil.GetPodCIDRs
	default:
		return fmt.Errorf("unknown CNI: %s", opts.CNIType)
	}

	getEndpointName, getEndpointID, newEndpoint := types.NewEndpointFuncs(opts.Cluster, opts.EndpointIDFormat, getEdgePodCIDRs)
	opts.NewEndpoint = newEndpoint

	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "failed to load kubeconfig")
		return nil
	}

	kubeClient, err := client.New(cfg, client.Options{})
	if err != nil {
		log.Error(err, "failed to create kube client")
		return err
	}

	opts.ManagerOpts.LeaderElectionNamespace = opts.Namespace
	opts.ManagerOpts.MetricsBindAddress = "0"
	opts.ManagerOpts.Logger = klogr.New().WithName("fabedge-operator")
	opts.Manager, err = manager.New(cfg, opts.ManagerOpts)
	if err != nil {
		log.Error(err, "failed to create controller manager")
		return err
	}

	var certManager certutil.Manager
	if opts.ClusterRole == RoleHost {
		certManager, opts.PrivateKey, err = createCertManager(kubeClient, client.ObjectKey{
			Name:      opts.CASecretName,
			Namespace: opts.Namespace,
		}, timeutil.Days(opts.CertValidPeriod))
		if err != nil {
			log.Error(err, "failed to create cert manager")
			return err
		}
	} else {
		cacert, err := fclient.GetCertificate(opts.APIServerAddress)
		if err != nil {
			log.Error(err, "failed to get CA cert from host cluster")
			return err
		}

		if err = opts.initAPIClient(kubeClient, cacert); err != nil {
			return err
		}

		certManager, err = certutil.NewRemoteManager(cacert.DER, func(csr []byte) ([]byte, error) {
			cert, innerErr := opts.APIClient.SignCert(csr)
			if innerErr != nil {
				return nil, innerErr
			}

			return cert.DER, nil
		})
		if err != nil {
			log.Error(err, "failed to create certManager")
			return err
		}
	}

	opts.Store = storepkg.NewStore()

	opts.Agent.Namespace = opts.Namespace
	opts.Agent.CertManager = certManager
	opts.Agent.Manager = opts.Manager
	opts.Agent.Store = opts.Store
	opts.Agent.NewEndpoint = opts.NewEndpoint
	opts.Agent.GetEndpointName = getEndpointName
	opts.Agent.CertOrganization = opts.CertOrganization

	opts.Connector.Namespace = opts.Namespace
	opts.Connector.CertOrganization = opts.CertOrganization
	opts.Connector.CertManager = certManager
	opts.Connector.Manager = opts.Manager
	opts.Connector.Store = opts.Store
	opts.Connector.GetPodCIDRs = getCloudPodCIDRs
	opts.Connector.Endpoint.Name = getEndpointName("connector")
	opts.Connector.Endpoint.ID = getEndpointID("connector")

	opts.Proxy.AgentNamespace = opts.Namespace
	opts.Proxy.Manager = opts.Manager
	opts.Proxy.CheckInterval = 5 * time.Second

	if opts.ClusterRole == RoleHost {
		opts.APIServer, err = apiserver.New(apiserver.Config{
			Addr:        opts.APIServerListenAddress,
			CertManager: certManager,
			Store:       opts.Store,
			Client:      opts.Manager.GetClient(),
			Log:         log.WithName("apiserver"),
		})
		if err != nil {
			log.Error(err, "failed to create api server")
			return err
		}

		certPool := x509.NewCertPool()
		certPool.AddCert(certManager.GetCACert())
		cert, err := tls.LoadX509KeyPair(opts.APIServerCertFile, opts.APIServerKeyFile)
		if err != nil {
			log.Error(err, "failed to load api server key pair")
			return err
		}
		opts.APIServer.TLSConfig = &tls.Config{
			ClientCAs:    certPool,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequestClientCert,
		}
	}

	return nil
}

func (opts Options) Validate() (err error) {
	if len(opts.Cluster) == 0 {
		return fmt.Errorf("a cluster name is required")
	}

	if !dns1123Reg.MatchString(opts.Cluster) {
		return fmt.Errorf("invalid cluster name: %s", opts.Cluster)
	}

	if opts.ClusterRole != RoleHost && opts.ClusterRole != RoleMember {
		return fmt.Errorf("unknown cluster role: %s", opts.ClusterRole)
	}

	if opts.ClusterRole == RoleMember && len(opts.InitToken) == 0 {
		return fmt.Errorf("initialization token is needed when cluster role is member")
	}

	if opts.ClusterRole == RoleHost {
		if !fileExists(opts.APIServerKeyFile) {
			return fmt.Errorf("api server key file doesnt' exist")
		}

		if !fileExists(opts.APIServerCertFile) {
			return fmt.Errorf("api server certificate file doesnt' exist")
		}
	}

	if len(opts.EdgeLabels) == 0 {
		return fmt.Errorf("edge labels is needed")
	}

	if len(opts.Connector.ConnectorLabels) == 0 {
		return fmt.Errorf("connector labels is needed")
	}

	if len(opts.Connector.Endpoint.PublicAddresses) == 0 {
		return fmt.Errorf("connector public addresses is needed")
	}

	for _, subnet := range opts.Connector.ProvidedSubnets {
		if _, _, err := net.ParseCIDR(subnet); err != nil {
			return fmt.Errorf("invalid subnet: %s. %w", subnet, err)
		}
	}

	if opts.Agent.EnableEdgeIPAM {
		ip, subnet, err := net.ParseCIDR(opts.EdgePodCIDR)
		if err != nil {
			return fmt.Errorf("invalid edge pod cidr: %s. %w", opts.EdgePodCIDR, err)
		}

		for _, s := range opts.Connector.ProvidedSubnets {
			ip2, subnet2, _ := net.ParseCIDR(s)
			if subnet.Contains(ip2) || subnet2.Contains(ip) {
				return fmt.Errorf("EdgePodCIDR is overlaped with connector's subnets")
			}
		}
	}

	policy := corev1.PullPolicy(opts.Agent.ImagePullPolicy)
	if policy != corev1.PullAlways &&
		policy != corev1.PullIfNotPresent &&
		policy != corev1.PullNever {
		return fmt.Errorf("not supported image pull policy: %s", policy)
	}

	// from client-go leaderelection.go
	const JitterFactor = 1.2
	leaseDuration, renewDeadline, retryPeriod := *opts.ManagerOpts.LeaseDuration, *opts.ManagerOpts.RenewDeadline, *opts.ManagerOpts.RetryPeriod
	if leaseDuration <= renewDeadline {
		return fmt.Errorf("leaseDuration must be greater than renewDeadline")
	}
	if renewDeadline <= time.Duration(JitterFactor*float64(retryPeriod)) {
		return fmt.Errorf("renewDeadline must be greater than retryPeriod*JitterFactor")
	}
	if leaseDuration < time.Second {
		return fmt.Errorf("leaseDuration must be greater than 1 second")
	}
	if renewDeadline < time.Second {
		return fmt.Errorf("renewDeadline must be greater than 1 second")
	}
	if retryPeriod < time.Second {
		return fmt.Errorf("retryPeriod must be greater than 1 second")
	}

	return nil
}

func createCertManager(cli client.Client, key client.ObjectKey, validPeriod time.Duration) (certutil.Manager, *rsa.PrivateKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var secret corev1.Secret
	err := cli.Get(ctx, key, &secret)

	if err != nil {
		return nil, nil, err
	}
	certPEM, keyPEM := secretutil.GetCA(secret)

	certDER, err := certutil.DecodePEM(certPEM)
	if err != nil {
		return nil, nil, err
	}

	keyDER, err := certutil.DecodePEM(keyPEM)
	if err != nil {
		return nil, nil, err
	}

	privateKey, err := x509.ParsePKCS1PrivateKey(keyDER)
	if err != nil {
		return nil, nil, err
	}

	certManager, err := certutil.NewManger(certDER, keyDER, validPeriod)
	return certManager, privateKey, err
}

func (opts Options) RunManager() error {
	if err := opts.Manager.Add(manager.RunnableFunc(opts.initializeControllers)); err != nil {
		log.Error(err, "failed to add init runnable")
		return err
	}

	if opts.ClusterRole == RoleHost {
		if err := opts.Manager.Add(manager.RunnableFunc(opts.runAPIServer)); err != nil {
			log.Error(err, "failed to add api server runnable")
			return err
		}
	}

	err := opts.Manager.Start(signals.SetupSignalHandler())
	if err != nil {
		log.Error(err, "failed to start controller manager")
	}

	return err
}

func (opts Options) runAPIServer(ctx context.Context) error {
	errChan := make(chan error)

	go func() {
		var err error
		err = opts.APIServer.ListenAndServeTLS("", "")
		if err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	var err error
	select {
	case err = <-errChan:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		err = ctx.Err()
	}

	return err
}

// initializeControllers adds controllers which are related to tunnels management to manager.
// we have to put controller registry logic in a Runnable because allocator and store initialization
// have to be done after leader election is finished, otherwise their data may be out of date
func (opts Options) initializeControllers(ctx context.Context) error {
	if opts.CNIType == constants.CNICalico {
		if err := opts.recordIPAMBlocks(ctx); err != nil {
			log.Error(err, "failed to record calico IPAMBlocks")
			return err
		}

		if err := ipamblockmonitor.AddToManager(ipamblockmonitor.Config{
			Manager: opts.Manager,
			Store:   opts.PodCIDRStore,
		}); err != nil {
			log.Error(err, "failed to add IPAMBlockMonitor to manager")
			return err
		}
	}

	err := opts.recordEndpoints(ctx)
	if err != nil {
		log.Error(err, "failed to initialize allocator and store")
		return err
	}

	// todo: ugly!!! try to move getConnectorEndpoint init in Complete
	getConnectorEndpoint, err := connectorctl.AddToManager(opts.Connector)
	if err != nil {
		log.Error(err, "failed to add communities controller to manager")
		return err
	}

	opts.Agent.GetConnectorEndpoint = getConnectorEndpoint
	if err = agentctl.AddToManager(opts.Agent); err != nil {
		log.Error(err, "failed to add agent controller to manager")
		return err
	}

	if err = cmmctl.AddToManager(cmmctl.Config{
		Manager: opts.Manager,
		Store:   opts.Store,
	}); err != nil {
		log.Error(err, "failed to add communities controller to manager")
		return err
	}

	if opts.Agent.EnableProxy {
		if err = proxyctl.AddToManager(opts.Proxy); err != nil {
			log.Error(err, "failed to add proxy controller to manager")
			return err
		}
	}

	if err = clusterctl.AddToManager(clusterctl.Config{
		Cluster:       opts.Cluster,
		Manager:       opts.Manager,
		PrivateKey:    opts.PrivateKey,
		TokenDuration: opts.TokenValidPeriod,
		Store:         opts.Store,
	}); err != nil {
		log.Error(err, "failed to add cluster controller to manager")
		return err
	}

	if opts.ClusterRole == RoleHost {
		reporter := &routines.LocalClusterReporter{
			Cluster:      opts.Cluster,
			GetConnector: getConnectorEndpoint,
			SyncInterval: 10 * time.Second,
			Client:       opts.Manager.GetClient(),
			Log:          opts.Manager.GetLogger().WithName("LocalClusterReporter"),
		}
		if err = opts.Manager.Add(reporter); err != nil {
			log.Error(err, "failed to add local cluster reporter to manager")
			return err
		}
	} else {
		err = opts.Manager.Add(routines.LoadEndpointsAndCommunities(
			timeutil.Seconds(10),
			opts.Store,
			opts.APIClient.GetEndpointsAndCommunities,
		))
		if err != nil {
			log.Error(err, "failed to start loadEndpointsAndCommunities routine")
			return err
		}

		err = opts.Manager.Add(routines.ExportEndpoints(
			timeutil.Seconds(10),
			getConnectorEndpoint,
			opts.APIClient.UpdateEndpoints,
		))
		if err != nil {
			log.Error(err, "failed to start exportEndpoints routine")
			return err
		}
	}

	return nil
}

func (opts Options) recordEndpoints(ctx context.Context) error {
	cli := opts.Manager.GetClient()
	store := opts.Store

	var nodes corev1.NodeList
	err := cli.List(ctx, &nodes, client.MatchingLabels(nodeutil.GetEdgeNodeLabels()))
	if err != nil {
		return err
	}

	var communities apis.CommunityList
	if err = cli.List(ctx, &communities); err != nil {
		return err
	}
	for _, community := range communities.Items {
		store.SaveCommunity(types.Community{
			Name:    community.Name,
			Members: sets.NewString(community.Spec.Members...),
		})
	}

	if !opts.Agent.EnableEdgeIPAM {
		return nil
	}

	for _, node := range nodes.Items {
		ep := opts.NewEndpoint(node)
		if len(ep.PublicAddresses) == 0 || len(ep.Subnets) == 0 || len(ep.NodeSubnets) == 0 {
			continue
		}

		for _, cidr := range ep.Subnets {
			_, subnet, err := net.ParseCIDR(cidr)
			// todo: maybe we should remove invalid subnet from endpoint here
			if err != nil {
				log.Error(err, "failed to parse subnet of node", "nodeName", node.Name, "node", node)
				continue
			}
			opts.Agent.Allocator.Record(*subnet)
		}

		store.SaveEndpoint(ep)
	}

	return nil
}

func (opts Options) recordIPAMBlocks(ctx context.Context) error {
	cli := opts.Manager.GetClient()

	var ipamBlocks calicoapi.IPAMBlockList
	if err := cli.List(ctx, &ipamBlocks); err != nil {
		return err
	}

	for _, block := range ipamBlocks.Items {
		if block.Spec.Deleted || block.Spec.Affinity == nil {
			continue
		}

		nodeName := ipamblockmonitor.GetNodeName(block)
		if nodeName == "" {
			continue
		}

		opts.PodCIDRStore.Append(nodeName, block.Spec.CIDR)
	}

	return nil
}

func (opts *Options) initAPIClient(kubeClient client.Client, cacert fclient.Certificate) error {
	key := client.ObjectKey{
		Name:      ClientTLSSecretName,
		Namespace: opts.Namespace,
	}

	certPool := x509.NewCertPool()
	certPool.AddCert(cacert.Raw)

	var secret corev1.Secret
	err := kubeClient.Get(context.Background(), key, &secret)
	switch {
	case err == nil:
	case errors.IsNotFound(err):
		secret, err = opts.createTLSSecretForClient(kubeClient, certPool, cacert)
		if err != nil {
			log.Error(err, "failed to create tls secret for API client")
			return err
		}
	default:
		log.Error(err, "failed to get tls secret for API client")
		return err
	}

	certPEM, keyPEM := secretutil.GetCertAndKey(secret)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Error(err, "not able to create tls key pair")
		return err
	}

	opts.APIClient, err = fclient.NewClient(opts.APIServerAddress, opts.Cluster, &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      certPool,
			Certificates: []tls.Certificate{cert},
		},
	})
	if err != nil {
		log.Error(err, "failed to create API client")
		return err
	}

	return nil
}

func (opts Options) createTLSSecretForClient(kubeClient client.Client, certPool *x509.CertPool, cacert fclient.Certificate) (secret corev1.Secret, err error) {
	keyDER, csrDER, err := certutil.NewCertRequest(certutil.Request{
		CommonName:   fmt.Sprintf("%s.fabedge-client", opts.Cluster),
		Organization: []string{opts.CertOrganization},
	})
	if err != nil {
		log.Error(err, "failed to create certificate request")
		return secret, err
	}

	cert, err := fclient.SignCertByToken(opts.APIServerAddress, opts.InitToken, csrDER, certPool)
	if err != nil {
		log.Error(err, "failed to create certificate for API client")
		return secret, err
	}

	secret = secretutil.TLSSecret().
		Name(ClientTLSSecretName).
		Namespace(opts.Namespace).
		EncodeKey(keyDER).
		CertPEM(cert.PEM).
		CACertPEM(cacert.PEM).
		Label(constants.KeyCreatedBy, constants.AppOperator).Build()

	err = kubeClient.Create(context.Background(), &secret)
	return secret, err
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
