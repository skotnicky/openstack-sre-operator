/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openstackv1alpha1 "github.com/skotnicky/openstack-sre-operator/api/v1alpha1"
	controllerspkg "github.com/skotnicky/openstack-sre-operator/controllers"
)

// OpenStackSREReconciler reconciles a OpenStackSRE object
type OpenStackSREReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=openstack.taikun.cloud,resources=openstacksres,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openstack.taikun.cloud,resources=openstacksres/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openstack.taikun.cloud,resources=openstacksres/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the OpenStackSRE object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.0/pkg/reconcile
func (r *OpenStackSREReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the CR instance
	var sre openstackv1alpha1.OpenStackSRE
	if err := r.Get(ctx, req.NamespacedName, &sre); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Find all nodes labelled as armed
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, client.MatchingLabels{"osre": "armed"}); err != nil {
		logger.Error(err, "failed to list nodes")
		return ctrl.Result{}, err
	}

	// 2. Evacuate (annotate) down nodes when enabled
	for _, node := range nodeList.Items {
		if !sre.Spec.EvacuationEnabled {
			continue
		}
		if !controllerspkg.IsNodeDown(&node) {
			continue
		}
		updated := node.DeepCopy()
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		if _, ok := updated.Annotations["openstack-sre/evacuated-at"]; !ok {
			updated.Annotations["openstack-sre/evacuated-at"] = time.Now().Format(time.RFC3339)
			if err := r.Update(ctx, updated); err != nil {
				logger.Error(err, "failed to annotate node", "node", node.Name)
				return ctrl.Result{}, err
			}
		}
	}

	// 3. Record a ConfigMap for balancing actions when enabled
	if sre.Spec.BalancingEnabled {
		cmName := types.NamespacedName{Name: "openstack-sre-last-balance", Namespace: req.Namespace}
		var cm corev1.ConfigMap
		if err := r.Get(ctx, cmName, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName.Name, Namespace: cmName.Namespace}}
				if err := r.Create(ctx, &cm); err != nil {
					logger.Error(err, "failed to create ConfigMap")
					return ctrl.Result{}, err
				}
			} else {
				logger.Error(err, "failed to get ConfigMap")
				return ctrl.Result{}, err
			}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["timestamp"] = time.Now().Format(time.RFC3339)
		if err := r.Update(ctx, &cm); err != nil {
			logger.Error(err, "failed to update ConfigMap")
			return ctrl.Result{}, err
		}
	}

	// 4. Update status with last reconciliation time
	sre.Status.LastAction = time.Now().Format(time.RFC3339)
	if err := r.Status().Update(ctx, &sre); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenStackSREReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openstackv1alpha1.OpenStackSRE{}).
		Named("openstacksre").
		Complete(r)
}
