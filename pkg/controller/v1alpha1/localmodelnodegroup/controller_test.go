/*
Copyright 2025 The KServe Authors.

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

package localmodelnodegroup

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/kserve/kserve/pkg/constants"
)

var _ = Describe("LocalModelNodeGroup controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)
	var (
		localModelNodeGroupSpec = v1alpha1.LocalModelNodeGroupSpec{
			PersistentVolumeSpec: corev1.PersistentVolumeSpec{
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeMode:                    ptr.To(corev1.PersistentVolumeFilesystem),
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
				StorageClassName:              "standard",
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/models",
						Type: ptr.To(corev1.HostPathDirectory),
					},
				},
				NodeAffinity: &corev1.VolumeNodeAffinity{
					Required: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "node.kubernetes.io/instance-type",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"gpu"},
									},
								},
							},
						},
					},
				},
			},
			PersistentVolumeClaimSpec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}},
			},
		}
		configs = map[string]string{
			"localModel": `{
				"jobNamespace": "kserve-localmodel-jobs",
				"defaultJobImage": "kserve/storage-initializer:latest",
				"localModelAgentImage": "kserve/kserve-localmodelnode-agent:latest",
				"localModelAgentImagePullPolicy": "IfNotPresent",
				"localModelAgentCpuRequest": "100m",
				"localModelAgentCpuLimit": "100m",
				"localModelAgentMemoryRequest": "200Mi",
				"localModelAgentMemoryLimit": "300Mi"
			}`,
		}
	)

	Context("When creating a LocalModelNodeGroup", func() {
		var (
			configMap *corev1.ConfigMap
		)
		BeforeEach(func() {
			ctx, cancel := context.WithCancel(context.Background())
			configMap = createConfigMap(ctx, configs)
			initializeManager(ctx, cfg)
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, configMap)
				By("canceling the context")
				cancel()
			})
		})

		It("Should create PV, PVC, and DaemonSet for the node group", func() {
			defer GinkgoRecover()
			ctx, cancel := context.WithCancel(context.Background())
			DeferCleanup(cancel)

			nodeGroup := &v1alpha1.LocalModelNodeGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-gpu-group",
				},
				Spec: localModelNodeGroupSpec,
			}
			Expect(k8sClient.Create(ctx, nodeGroup)).Should(Succeed())
			defer k8sClient.Delete(ctx, nodeGroup)

			agentName := "test-gpu-group-agent"

			// Verify PV is created
			pvLookupKey := types.NamespacedName{Name: agentName}
			pv := &corev1.PersistentVolume{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, pvLookupKey, pv)
				return err == nil
			}, timeout, interval).Should(BeTrue(), "PersistentVolume should be created")
			Expect(pv.Labels[appManagedByLabel]).To(Equal(managedByValue))
			Expect(pv.Labels[appComponentLabel]).To(Equal(pvComponent))

			// Verify PVC is created
			pvcLookupKey := types.NamespacedName{Name: agentName, Namespace: constants.KServeNamespace}
			pvc := &corev1.PersistentVolumeClaim{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, pvcLookupKey, pvc)
				return err == nil
			}, timeout, interval).Should(BeTrue(), "PersistentVolumeClaim should be created")
			Expect(pvc.Labels[appManagedByLabel]).To(Equal(managedByValue))
			Expect(pvc.Labels[appComponentLabel]).To(Equal(pvcComponent))
			Expect(pvc.Spec.VolumeName).To(Equal(agentName))

			// Verify DaemonSet is created
			dsLookupKey := types.NamespacedName{Name: agentName, Namespace: constants.KServeNamespace}
			ds := &appsv1.DaemonSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, dsLookupKey, ds)
				return err == nil
			}, timeout, interval).Should(BeTrue(), "DaemonSet should be created")

			Expect(ds.Labels[appManagedByLabel]).To(Equal(managedByValue))
			Expect(ds.Labels[appComponentLabel]).To(Equal(daemonsetComponent))

			// Verify DaemonSet spec
			Expect(ds.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := ds.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal("manager"))
			Expect(container.Image).To(Equal("kserve/kserve-localmodelnode-agent:latest"))
			Expect(container.ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))

			// Verify resource requests and limits
			Expect(container.Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("100m")))
			Expect(container.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("200Mi")))
			Expect(container.Resources.Limits[corev1.ResourceCPU]).To(Equal(resource.MustParse("100m")))
			Expect(container.Resources.Limits[corev1.ResourceMemory]).To(Equal(resource.MustParse("300Mi")))

			// Verify PVC volume is used (not hostPath)
			Expect(ds.Spec.Template.Spec.Volumes).To(HaveLen(1))
			Expect(ds.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim).NotTo(BeNil())
			Expect(ds.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(agentName))

			// Verify volume mount
			Expect(container.VolumeMounts).To(HaveLen(1))
			Expect(container.VolumeMounts[0].MountPath).To(Equal(constants.DefaultModelLocalMountPath))

			// Verify node affinity from PV spec
			Expect(ds.Spec.Template.Spec.Affinity).NotTo(BeNil())
			Expect(ds.Spec.Template.Spec.Affinity.NodeAffinity).NotTo(BeNil())
			Expect(ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution).NotTo(BeNil())
			terms := ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			Expect(terms).To(HaveLen(1))
			Expect(terms[0].MatchExpressions).To(HaveLen(1))
			Expect(terms[0].MatchExpressions[0].Key).To(Equal("node.kubernetes.io/instance-type"))
			Expect(terms[0].MatchExpressions[0].Values).To(ContainElement("gpu"))

			// Verify PSS-compliant security context
			sc := container.SecurityContext
			Expect(sc).NotTo(BeNil())
			Expect(*sc.Privileged).To(BeFalse())
			Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
			Expect(*sc.RunAsNonRoot).To(BeTrue())
			Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())

			// Verify service account
			Expect(ds.Spec.Template.Spec.ServiceAccountName).To(Equal(serviceAccountName))

			// Verify finalizer was added
			updatedNodeGroup := &v1alpha1.LocalModelNodeGroup{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-gpu-group"}, updatedNodeGroup)
				if err != nil {
					return false
				}
				for _, f := range updatedNodeGroup.GetFinalizers() {
					if f == finalizerName {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "Finalizer should be added to LocalModelNodeGroup")
		})

		It("Should remove finalizer on deletion", func() {
			defer GinkgoRecover()
			ctx, cancel := context.WithCancel(context.Background())
			DeferCleanup(cancel)

			nodeGroup := &v1alpha1.LocalModelNodeGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-delete-group",
				},
				Spec: localModelNodeGroupSpec,
			}
			Expect(k8sClient.Create(ctx, nodeGroup)).Should(Succeed())

			agentName := "test-delete-group-agent"

			// Wait for DaemonSet to be created (which means reconciliation completed)
			ds := &appsv1.DaemonSet{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: agentName, Namespace: constants.KServeNamespace}, ds)
				return err == nil
			}, timeout, interval).Should(BeTrue(), "DaemonSet should be created")

			// Delete the node group
			Expect(k8sClient.Delete(ctx, nodeGroup)).Should(Succeed())

			// Verify the node group is eventually deleted (finalizer was removed)
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-delete-group"}, &v1alpha1.LocalModelNodeGroup{})
				return err != nil
			}, timeout, interval).Should(BeTrue(), "LocalModelNodeGroup should be deleted after finalizer removal")
		})
	})
})

func createConfigMap(ctx context.Context, configs map[string]string) *corev1.ConfigMap {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constants.InferenceServiceConfigMapName,
			Namespace: constants.KServeNamespace,
		},
		Data: configs,
	}
	Expect(k8sClient.Create(ctx, configMap)).NotTo(HaveOccurred())
	return configMap
}

func initializeManager(ctx context.Context, cfg *rest.Config) {
	clientset, err := kubernetes.NewForConfig(cfg)
	Expect(err).ToNot(HaveOccurred())
	Expect(clientset).ToNot(BeNil())
	Expect(testScheme).ToNot(BeNil())

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: testScheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Controller: crconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	Expect(err).ToNot(HaveOccurred())

	k8sClient = k8sManager.GetClient()
	Expect(k8sClient).ToNot(BeNil())

	err = NewLocalModelNodeGroupReconciler(
		k8sClient,
		clientset,
		ctrl.Log.WithName("v1alpha1LocalModelNodeGroupController"),
		testScheme,
	).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred())
	}()
	// Wait for cache to start
	Eventually(func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Name: constants.InferenceServiceConfigMapName, Namespace: constants.KServeNamespace}, &corev1.ConfigMap{}) == nil
	}, time.Second*10, time.Millisecond*250).Should(BeTrue())
}
