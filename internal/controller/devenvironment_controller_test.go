package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	devenvv1 "github.com/tunnel4/tunnel4-operator/api/v1"
)

var _ = Describe("DevEnvironment Controller", func() {
	var reconciler *DevEnvironmentReconciler

	BeforeEach(func() {
		reconciler = &DevEnvironmentReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	// ─── helper to remove finalizer and force-delete ─────────────────────────
	forceDelete := func(ctx context.Context, name types.NamespacedName) {
		obj := &devenvv1.DevEnvironment{}
		if err := k8sClient.Get(ctx, name, obj); err != nil {
			return
		}
		obj.Finalizers = nil
		_ = k8sClient.Update(ctx, obj)
		_ = k8sClient.Delete(ctx, obj)
	}

	deleteNS := func(ctx context.Context, ns string) {
		n := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: ns}, n); err == nil {
			_ = k8sClient.Delete(ctx, n)
		}
	}

	// ─────────────────────────────────────────────────────────────────────────
	// Non-existent resource
	// ─────────────────────────────────────────────────────────────────────────
	Describe("reconciling a non-existent resource", func() {
		It("returns without error", func(ctx context.Context) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ─────────────────────────────────────────────────────────────────────────
	// New DevEnvironment provisioning
	// ─────────────────────────────────────────────────────────────────────────
	Describe("provisioning a new DevEnvironment", func() {
		var (
			devEnv  *devenvv1.DevEnvironment
			devName types.NamespacedName
			devNS   string
		)

		BeforeEach(func(ctx context.Context) {
			devEnv = &devenvv1.DevEnvironment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "alice-stripe",
					Namespace: "default",
				},
				Spec: devenvv1.DevEnvironmentSpec{
					Developer: "alice",
					Branch:    "feature/stripe",
					Intercepts: []devenvv1.Intercept{
						{Service: "payments-service", LocalPort: 8080},
						{Service: "webhook-service", LocalPort: 8081},
					},
					TTL: "8h",
				},
			}
			devName = types.NamespacedName{Name: devEnv.Name, Namespace: devEnv.Namespace}
			devNS = buildNamespace("alice", "feature/stripe") // "alice-feature-stripe"

			Expect(k8sClient.Create(ctx, devEnv)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func(ctx context.Context) {
			forceDelete(ctx, devName)
			deleteNS(ctx, devNS)
		})

		It("adds the finalizer to the resource", func(ctx context.Context) {
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			Expect(fresh.Finalizers).To(ContainElement(finalizerName))
		})

		It("creates the developer namespace with correct labels", func(ctx context.Context) {
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: devNS}, ns)).To(Succeed())
			Expect(ns.Labels).To(HaveKeyWithValue("tunnel4.dev/managed-by", "operator"))
			Expect(ns.Labels).To(HaveKeyWithValue("tunnel4.dev/developer", "alice"))
			Expect(ns.Labels).To(HaveKeyWithValue("tunnel4.dev/branch", "feature-stripe"))
			Expect(ns.Labels).To(HaveKeyWithValue("tunnel4.dev/env", "alice-stripe"))
		})

		It("creates a ResourceQuota in the namespace", func(ctx context.Context) {
			quota := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "tunnel4-quota", Namespace: devNS,
			}, quota)).To(Succeed())
			Expect(quota.Spec.Hard).To(HaveKey(corev1.ResourceCPU))
			Expect(quota.Spec.Hard).To(HaveKey(corev1.ResourceMemory))
			Expect(quota.Spec.Hard).To(HaveKey(corev1.ResourcePods))
		})

		It("creates a NetworkPolicy that isolates the namespace", func(ctx context.Context) {
			np := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "tunnel4-isolation", Namespace: devNS,
			}, np)).To(Succeed())
			Expect(np.Spec.PolicyTypes).To(ContainElements(
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			))
		})

		It("creates a stub Deployment for each intercept", func(ctx context.Context) {
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "payments-service-stub", Namespace: devNS,
			}, deploy)).To(Succeed())
			Expect(deploy.Labels).To(HaveKeyWithValue("tunnel4.dev/stub", "true"))
			Expect(deploy.Labels).To(HaveKeyWithValue("app", "payments-service"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "webhook-service-stub", Namespace: devNS,
			}, deploy)).To(Succeed())
			Expect(deploy.Labels).To(HaveKeyWithValue("app", "webhook-service"))
		})

		It("creates a stub Service for each intercept using the original service name", func(ctx context.Context) {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "payments-service", Namespace: devNS,
			}, svc)).To(Succeed())
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("tunnel4.dev/stub", "true"))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("app", "payments-service"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "webhook-service", Namespace: devNS,
			}, svc)).To(Succeed())
		})

		It("sets the status phase to Provisioning", func(ctx context.Context) {
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			Expect(fresh.Status.Phase).To(Equal(devenvv1.PhaseProvisioning))
		})

		It("records the namespace in status", func(ctx context.Context) {
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			Expect(fresh.Status.Namespace).To(Equal(devNS))
		})

		It("records intercept statuses", func(ctx context.Context) {
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			Expect(fresh.Status.Intercepts).To(HaveLen(2))
			services := []string{
				fresh.Status.Intercepts[0].Service,
				fresh.Status.Intercepts[1].Service,
			}
			Expect(services).To(ConsistOf("payments-service", "webhook-service"))
		})

		It("requeues after 10s while stubs are not yet ready", func(ctx context.Context) {
			// Trigger a second reconcile — stubs exist but ReadyReplicas=0 in envtest
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(10 * time.Second))
		})
	})

	// ─────────────────────────────────────────────────────────────────────────
	// Deletion flow
	// ─────────────────────────────────────────────────────────────────────────
	Describe("handling deletion of a DevEnvironment", func() {
		var (
			devName types.NamespacedName
			devNS   string
		)

		BeforeEach(func(ctx context.Context) {
			devNS = buildNamespace("bob", "main")
			devName = types.NamespacedName{Name: "bob-del", Namespace: "default"}

			devEnv := &devenvv1.DevEnvironment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      devName.Name,
					Namespace: devName.Namespace,
				},
				Spec: devenvv1.DevEnvironmentSpec{
					Developer: "bob",
					Branch:    "main",
				},
			}
			Expect(k8sClient.Create(ctx, devEnv)).To(Succeed())

			// First reconcile: adds finalizer and provisions
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func(ctx context.Context) {
			forceDelete(ctx, devName)
			deleteNS(ctx, devNS)
		})

		It("removes the finalizer and deletes the namespace on deletion", func(ctx context.Context) {
			// Trigger k8s deletion (sets DeletionTimestamp; finalizer keeps object alive)
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			Expect(k8sClient.Delete(ctx, fresh)).To(Succeed())

			// Second reconcile: sees DeletionTimestamp, runs handleDeletion
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())

			// Finalizer removed → GC deletes the object
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, devName, &devenvv1.DevEnvironment{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
		})
	})

	// ─────────────────────────────────────────────────────────────────────────
	// Idle / sleep transition
	// ─────────────────────────────────────────────────────────────────────────
	Describe("sleeping an idle DevEnvironment", func() {
		var (
			devName types.NamespacedName
			devNS   string
		)

		BeforeEach(func(ctx context.Context) {
			devNS = buildNamespace("carol", "main")
			devName = types.NamespacedName{Name: "carol-idle", Namespace: "default"}

			devEnv := &devenvv1.DevEnvironment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      devName.Name,
					Namespace: devName.Namespace,
				},
				Spec: devenvv1.DevEnvironmentSpec{
					Developer: "carol",
					Branch:    "main",
				},
			}
			Expect(k8sClient.Create(ctx, devEnv)).To(Succeed())

			// First reconcile to provision (creates namespace)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())

			// Manually drive to PhaseReady with an expired heartbeat
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			fresh.Status.Phase = devenvv1.PhaseReady
			fresh.Status.Namespace = devNS
			fresh.Status.LastHeartbeat = metav1.Time{Time: time.Now().Add(-1 * time.Hour)}
			Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
		})

		AfterEach(func(ctx context.Context) {
			forceDelete(ctx, devName)
			deleteNS(ctx, devNS)
		})

		It("transitions to PhaseSleeping and requeues after 5m", func(ctx context.Context) {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			Expect(fresh.Status.Phase).To(Equal(devenvv1.PhaseSleeping))
			Expect(fresh.Status.Message).To(Equal("idle timeout"))
		})

		It("requeues after 5m even when heartbeat is recent", func(ctx context.Context) {
			// Update heartbeat to now so it's not idle
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			fresh.Status.LastHeartbeat = metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			// Phase stays Ready
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			Expect(fresh.Status.Phase).To(Equal(devenvv1.PhaseReady))
		})
	})

	// ─────────────────────────────────────────────────────────────────────────
	// Orphan cleanup
	// ─────────────────────────────────────────────────────────────────────────
	Describe("cleaning up orphaned stubs when an intercept is removed", func() {
		var (
			devName types.NamespacedName
			devNS   string
		)

		BeforeEach(func(ctx context.Context) {
			devNS = buildNamespace("dave", "main")
			devName = types.NamespacedName{Name: "dave-orphan", Namespace: "default"}

			devEnv := &devenvv1.DevEnvironment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      devName.Name,
					Namespace: devName.Namespace,
				},
				Spec: devenvv1.DevEnvironmentSpec{
					Developer: "dave",
					Branch:    "main",
					Intercepts: []devenvv1.Intercept{
						{Service: "api-service", LocalPort: 8080},
						{Service: "extra-service", LocalPort: 8081},
					},
				},
			}
			Expect(k8sClient.Create(ctx, devEnv)).To(Succeed())

			// First reconcile: creates both stubs
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())

			// Remove extra-service from spec
			fresh := &devenvv1.DevEnvironment{}
			Expect(k8sClient.Get(ctx, devName, fresh)).To(Succeed())
			fresh.Spec.Intercepts = []devenvv1.Intercept{
				{Service: "api-service", LocalPort: 8080},
			}
			Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		})

		AfterEach(func(ctx context.Context) {
			forceDelete(ctx, devName)
			deleteNS(ctx, devNS)
		})

		It("deletes the orphaned stub deployment", func(ctx context.Context) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())

			orphan := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: "extra-service-stub", Namespace: devNS,
			}, orphan)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("deletes the orphaned stub service", func(ctx context.Context) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())

			svc := &corev1.Service{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: "extra-service", Namespace: devNS,
			}, svc)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("keeps the still-active stub deployment", func(ctx context.Context) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devName})
			Expect(err).NotTo(HaveOccurred())

			active := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "api-service-stub", Namespace: devNS,
			}, active)).To(Succeed())
		})
	})
})
