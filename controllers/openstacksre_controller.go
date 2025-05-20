package controllers

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	availabilityzones "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/availabilityzones"
	evacuateext "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/evacuate"
	hypervisors "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/hypervisors"
	migrate "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/migrate"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openstackv1alpha1 "github.com/skotnicky/openstack-sre-operator/api/v1alpha1"
)

// OpenStackSREReconciler reconciles a OpenStackSRE object
type OpenStackSREReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// You may want a cached Gophercloud client or pass in credentials
	// from a Secret. For simplicity, we'll create a new session each time.
}

// hypervisorInfo captures basic load information for a hypervisor within an AZ.
type hypervisorInfo struct {
	hypervisors.Hypervisor
	AZ string
}

//+kubebuilder:rbac:groups=openstack.example.com,resources=openstacksres,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=openstack.example.com,resources=openstacksres/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=openstack.example.com,resources=openstacksres/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch

func (r *OpenStackSREReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the OpenStackSRE resource
	var sre openstackv1alpha1.OpenStackSRE
	if err := r.Get(ctx, req.NamespacedName, &sre); err != nil {
		// If the resource no longer exists, we don’t requeue
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Build an OpenStack provider client via Gophercloud
	provider, err := getOpenStackProvider()
	if err != nil {
		logger.Error(err, "failed to get OpenStack provider")
		return ctrl.Result{}, err
	}

	computeClient, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})
	if err != nil {
		logger.Error(err, "failed to create compute client")
		return ctrl.Result{}, err
	}

	// 2. Find all Kubernetes nodes labeled osre=armed
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, client.MatchingLabels{"osre": "armed"}); err != nil {
		logger.Error(err, "failed to list armed nodes")
		return ctrl.Result{}, err
	}

	// 3. For each armed node, check if it is "down"
	//    (In a real scenario, you'd map K8s node to an OpenStack hypervisor.)
	downHypervisors := []corev1.Node{}
	for _, node := range nodeList.Items {
		if IsNodeDown(&node) {
			downHypervisors = append(downHypervisors, node)
		}
	}

	// 4. If a hypervisor is confirmed "down", evacuate its instances
	for _, hv := range downHypervisors {
		if sre.Spec.EvacuationEnabled {
			// NOTE: You need a mapping from K8s node name -> OpenStack hypervisor ID
			// Here, we'll pretend the K8s Node name = hypervisor hostname
			hypervisorID := hv.Name
			err = r.proposeAndExecute(ctx, logger, "evacuate-"+hypervisorID, func() error {
				return evacuateHypervisor(ctx, logger, computeClient, hypervisorID)
			})
			if err != nil {
				logger.Error(err, "failed to evacuate hypervisor")
			}
		}
	}

	// 5. If balancing is enabled, do a naive load-balance
	//    This logic can be expanded to check instance distribution, anti-affinity, etc.
	if sre.Spec.BalancingEnabled {
		err := r.proposeAndExecute(ctx, logger, "rebalance-instances", func() error {
			return rebalanceInstances(ctx, logger, computeClient, sre.Spec.BalanceThreshold, sre.Spec.PreferredAZs)
		})
		if err != nil {
			logger.Error(err, "failed to rebalance instances")
		}
	}

	// 6. Update status
	sre.Status.LastAction = time.Now().String()
	if err := r.Status().Update(ctx, &sre); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	// Requeue periodically (e.g., every 60s) or watch for events
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenStackSREReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openstackv1alpha1.OpenStackSRE{}).
		Complete(r)
}

// getOpenStackProvider - create a Gophercloud provider from environment or Secrets
func getOpenStackProvider() (*gophercloud.ProviderClient, error) {
	opts, err := openstack.AuthOptionsFromEnv()
	if err != nil {
		return nil, err
	}
	provider, err := openstack.AuthenticatedClient(opts)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// IsNodeDown checks if a node should be considered down based on its Ready
// condition. It returns true when the Ready condition is False or Unknown, or
// when no Ready condition is present at all.
func IsNodeDown(node *corev1.Node) bool {
	found := false
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			found = true
			if cond.Status == corev1.ConditionFalse || cond.Status == corev1.ConditionUnknown {
				return true
			}
		}
	}
	return !found
}

// evacuateHypervisor calls "server evacuate" on each instance in the down hypervisor
func evacuateHypervisor(ctx context.Context, logger logr.Logger, computeClient *gophercloud.ServiceClient, hypervisorID string) error {
	// Example listing servers by hypervisor. One approach: filter by "host"
	// or pass the hypervisor name. This is for illustration only.
	allPages, err := servers.List(computeClient, servers.ListOpts{
		Host: hypervisorID,
	}).AllPages()
	if err != nil {
		return err
	}
	allServers, err := servers.ExtractServers(allPages)
	if err != nil {
		return err
	}
	for _, srv := range allServers {
		logger.Info("Evacuating server", "server", srv.ID, "hypervisor", hypervisorID)
		// Use the evacuate extension to move instances off the failed hypervisor
		result := evacuateext.Evacuate(computeClient, srv.ID, evacuateext.EvacuateOpts{})
		if result.Err != nil {
			logger.Error(result.Err, "Evacuation failed", "serverID", srv.ID)
		}
	}
	return nil
}

// rebalanceInstances tries to live-migrate servers to level the load
func rebalanceInstances(ctx context.Context, logger logr.Logger, computeClient *gophercloud.ServiceClient, threshold int, preferredAZs []string) error {
	if threshold <= 0 {
		threshold = 1
	}

	// 1. Map hypervisor hostname -> availability zone
	azPages, err := availabilityzones.ListDetail(computeClient).AllPages()
	if err != nil {
		return err
	}
	azList, err := availabilityzones.ExtractAvailabilityZones(azPages)
	if err != nil {
		return err
	}

	hostAZ := make(map[string]string)
	for _, az := range azList {
		// If preferred AZs are specified, skip others
		if len(preferredAZs) > 0 {
			found := false
			for _, p := range preferredAZs {
				if p == az.ZoneName {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		for host := range az.Hosts {
			hostAZ[host] = az.ZoneName
		}
	}

	// 2. List hypervisors and group by AZ
	hvPages, err := hypervisors.List(computeClient, nil).AllPages()
	if err != nil {
		return err
	}
	hvList, err := hypervisors.ExtractHypervisors(hvPages)
	if err != nil {
		return err
	}

	azMap := map[string][]hypervisorInfo{}
	for _, hv := range hvList {
		az := hostAZ[hv.HypervisorHostname]
		info := hypervisorInfo{Hypervisor: hv, AZ: az}
		azMap[az] = append(azMap[az], info)
	}

	var aggErr error
	// 3. For each AZ, pick src/dst hosts and migrate
	for az, hosts := range azMap {
		src, dst, ok := selectMigrationPair(hosts, threshold)
		if !ok {
			continue
		}

		logger.Info("Balancing", "az", az, "source", src.HypervisorHostname, "target", dst.HypervisorHostname)

		// List servers on the source host
		srvPages, err := servers.List(computeClient, servers.ListOpts{Host: src.HypervisorHostname}).AllPages()
		if err != nil {
			aggErr = fmt.Errorf("%v; %w", aggErr, err)
			continue
		}
		srvList, err := servers.ExtractServers(srvPages)
		if err != nil {
			aggErr = fmt.Errorf("%v; %w", aggErr, err)
			continue
		}
		if len(srvList) == 0 {
			continue
		}

		// Migrate a single instance from src to dst
		srv := srvList[0]
		migErr := migrate.LiveMigrate(computeClient, srv.ID, migrate.LiveMigrateOpts{Host: &dst.HypervisorHostname}).ExtractErr()
		if migErr != nil {
			logger.Error(migErr, "migration failed", "server", srv.ID)
			aggErr = fmt.Errorf("%v; %w", aggErr, migErr)
			continue
		}
		logger.Info("Migrated server", "server", srv.ID, "from", src.HypervisorHostname, "to", dst.HypervisorHostname)
	}
	return aggErr
}

var lock sync.Mutex

// selectMigrationPair picks a source and target hypervisor within an AZ if the
// load difference exceeds the threshold.
func selectMigrationPair(hosts []hypervisorInfo, threshold int) (hypervisorInfo, hypervisorInfo, bool) {
	if len(hosts) < 2 {
		return hypervisorInfo{}, hypervisorInfo{}, false
	}
	sort.Slice(hosts, func(i, j int) bool {
		return hosts[i].RunningVMs > hosts[j].RunningVMs
	})
	src := hosts[0]
	dst := hosts[len(hosts)-1]
	if (src.RunningVMs - dst.RunningVMs) < threshold {
		return hypervisorInfo{}, hypervisorInfo{}, false
	}
	return src, dst, true
}

// proposeAndExecute - a naive majority-based mechanism using a ConfigMap as shared state
func (r *OpenStackSREReconciler) proposeAndExecute(
	ctx context.Context,
	logger logr.Logger,
	actionKey string,
	actionFunc func() error,
) error {
	lock.Lock()
	defer lock.Unlock()

	// 1. Get or Create a ConfigMap to track consensus
	cmName := "openstack-sre-consensus"
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: "default"}, &cm)
	if err != nil {
		// If not found, create it
		cm = corev1.ConfigMap{
			ObjectMeta: v1.ObjectMeta{
				Name:      cmName,
				Namespace: "default",
			},
			Data: map[string]string{},
		}
		if createErr := r.Create(ctx, &cm); createErr != nil {
			logger.Error(createErr, "cannot create consensus configmap")
			return createErr
		}
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	// 2. Read existing votes for actionKey
	votesStr, found := cm.Data[actionKey]
	if !found {
		votesStr = "0"
	}
	votes, _ := strconv.Atoi(votesStr)

	// 3. Increment vote (we assume each operator replica tries once)
	votes++
	cm.Data[actionKey] = fmt.Sprintf("%d", votes)

	// 4. Update the ConfigMap with new vote count
	if err := r.Update(ctx, &cm); err != nil {
		logger.Error(err, "failed to update votes in configmap")
		return err
	}

	// 5. If votes >= majority threshold, execute
	//    e.g. if we expect 3 replicas, majority is 2
	majorityThreshold := 2
	if replicaCountStr := os.Getenv("REPLICA_COUNT"); replicaCountStr != "" {
		if rc, err := strconv.Atoi(replicaCountStr); err == nil && rc > 1 {
			majorityThreshold = (rc / 2) + 1
		}
	}

	if votes >= majorityThreshold {
		logger.Info("Majority reached, executing action", "actionKey", actionKey)
		return actionFunc()
	}

	logger.Info("Not enough votes yet for action", "actionKey", actionKey, "votes", votes, "threshold", majorityThreshold)
	return nil
}
