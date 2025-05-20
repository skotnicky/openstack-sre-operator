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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hypervisors "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/hypervisors"
	openstackv1alpha1 "github.com/skotnicky/openstack-sre-operator/api/v1alpha1"
	controllerspkg "github.com/skotnicky/openstack-sre-operator/controllers"
)

var _ = Describe("OpenStackSRE Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		openstacksre := &openstackv1alpha1.OpenStackSRE{}

		nodeName := "node1"
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeName,
				Labels: map[string]string{"osre": "armed"},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionUnknown,
					},
				},
			},
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind OpenStackSRE")
			err := k8sClient.Get(ctx, typeNamespacedName, openstacksre)
			if err != nil && errors.IsNotFound(err) {
				resource := &openstackv1alpha1.OpenStackSRE{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: openstackv1alpha1.OpenStackSRESpec{
						EvacuationEnabled: true,
						BalancingEnabled:  true,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("creating a dummy node")
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &openstackv1alpha1.OpenStackSRE{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance OpenStackSRE")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			cm := &corev1.ConfigMap{}
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "openstack-sre-last-balance", Namespace: "default"}, cm)
			_ = k8sClient.Delete(ctx, cm)

			_ = k8sClient.Delete(ctx, node)
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &OpenStackSREReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying status update")
			updated := &openstackv1alpha1.OpenStackSRE{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.LastAction).NotTo(BeEmpty())

			By("ensuring node was annotated")
			n := &corev1.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, n)).To(Succeed())
			Expect(n.Annotations).To(HaveKey("openstack-sre/evacuated-at"))

			By("checking balancing ConfigMap")
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "openstack-sre-last-balance", Namespace: "default"}, cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("timestamp"))
		})

		Context("isNodeDown helper", func() {
			It("returns true for ConditionUnknown", func() {
				node := corev1.Node{
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{
								Type:   corev1.NodeReady,
								Status: corev1.ConditionUnknown,
							},
						},
					},
				}
				Expect(controllerspkg.IsNodeDown(&node)).To(BeTrue())
			})

			It("returns true when Ready condition missing", func() {
				node := corev1.Node{}
				Expect(controllerspkg.IsNodeDown(&node)).To(BeTrue())
			})
		})
	})

	var _ = Describe("Balancing logic", func() {
		It("selects hosts when difference exceeds threshold", func() {
			hosts := []hypervisorInfo{
				{Hypervisor: hypervisors.Hypervisor{HypervisorHostname: "hv1", RunningVMs: 10}, AZ: "nova"},
				{Hypervisor: hypervisors.Hypervisor{HypervisorHostname: "hv2", RunningVMs: 2}, AZ: "nova"},
			}
			src, dst, ok := selectMigrationPair(hosts, 5)
			Expect(ok).To(BeTrue())
			Expect(src.HypervisorHostname).To(Equal("hv1"))
			Expect(dst.HypervisorHostname).To(Equal("hv2"))

			_, _, ok = selectMigrationPair(hosts, 20)
			Expect(ok).To(BeFalse())
		})
	})
})
