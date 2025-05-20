package controllers

import (
    context "context"
    "fmt"
    "os"
    "strconv"
    "sync"
    "time"

    "github.com/go-logr/logr"
    "github.com/gophercloud/gophercloud"
    "github.com/gophercloud/gophercloud/openstack"
    "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions"
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
        if isNodeDown(&node) {
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
            return rebalanceInstances(ctx, logger, computeClient)
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

// isNodeDown - simplistic check if Node is "NotReady". In production, refine this.
func isNodeDown(node *corev1.Node) bool {
    for _, cond := range node.Status.Conditions {
        if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionFalse {
            return true
        }
    }
    return false
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
        // Gophercloud doesn't have a top-level "evacuate" call by default, but we can use extensions
        err = extensions.Evacuate(computeClient, srv.ID, extensions.EvacuateOpts{}).ExtractErr()
        if err != nil {
            logger.Error(err, "Evacuation failed", "serverID", srv.ID)
        }
    }
    return nil
}

// rebalanceInstances tries to live-migrate servers to level the load
func rebalanceInstances(ctx context.Context, logger logr.Logger, computeClient *gophercloud.ServiceClient) error {
    // For example, we could:
    // 1. List all hypervisors, check the number of running VMs
    // 2. Identify the most loaded vs. least loaded
    // 3. Use servers.Migrate / servers.LiveMigrate to move some VMs
    // 4. Check anti-affinity/availability zone constraints
    // This is a simplified placeholder

    logger.Info("Balancing load among hypervisors (placeholder)")
    // ...
    return nil
}

var lock sync.Mutex

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
