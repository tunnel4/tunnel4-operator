package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	devenvv1 "github.com/tunnel4/tunnel4-operator/api/v1"
)

const (
	finalizerName = "tunnel4.dev/finalizer"
	idleTimeout   = 30 * time.Minute
)

func stubImage() string {
	if v := os.Getenv("STUB_IMAGE"); v != "" {
		return v
	}
	return "devenv-stub:latest"
}

func gatewayAddr() string {
	if v := os.Getenv("GATEWAY_ADDR"); v != "" {
		return v
	}
	return "tunnel4-gateway.tunnel4-system.svc.cluster.local:7777"
}

type DevEnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=devenv.tunnel4.dev,resources=devenvironments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=devenv.tunnel4.dev,resources=devenvironments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=devenv.tunnel4.dev,resources=devenvironments/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

func (r *DevEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// log := logr.FromContext(ctx)

	var devEnv devenvv1.DevEnvironment
	if err := r.Get(ctx, req.NamespacedName, &devEnv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion — delete finalizer after cleanup
	if !devEnv.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &devEnv)
	}

	// Add finalizer on first creation
	if !containsString(devEnv.Finalizers, finalizerName) {
		devEnv.Finalizers = append(devEnv.Finalizers, finalizerName)
		if err := r.Update(ctx, &devEnv); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Sleep watchdog — check idle timeout
	if devEnv.Status.Phase == devenvv1.PhaseReady {
		if r.isIdle(&devEnv) {
			return r.transitionToSleep(ctx, &devEnv)
		}
		// Check after 5 min
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Provisioning
	return r.provision(ctx, &devEnv)
}

// provision — create everything for env
func (r *DevEnvironmentReconciler) provision(ctx context.Context, devEnv *devenvv1.DevEnvironment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Update phase to Provisioning
	if devEnv.Status.Phase == "" || devEnv.Status.Phase == devenvv1.PhaseSleeping {
		if err := r.setPhase(ctx, devEnv, devenvv1.PhaseProvisioning, ""); err != nil {
			return ctrl.Result{}, err
		}
	}

	ns := buildNamespace(devEnv.Spec.Developer, devEnv.Spec.Branch)
	devEnv.Status.Namespace = ns

	// 1. Namespace
	if err := r.ensureNamespace(ctx, devEnv, ns); err != nil {
		logger.Error(err, "failed to ensure namespace")
		return ctrl.Result{}, r.setPhase(ctx, devEnv, devenvv1.PhaseFailed, err.Error())
	}

	// 2. ResourceQuota
	if err := r.ensureResourceQuota(ctx, ns); err != nil {
		return ctrl.Result{}, r.setPhase(ctx, devEnv, devenvv1.PhaseFailed, err.Error())
	}

	// 3. NetworkPolicy
	if err := r.ensureNetworkPolicy(ctx, devEnv, ns); err != nil {
		return ctrl.Result{}, r.setPhase(ctx, devEnv, devenvv1.PhaseFailed, err.Error())
	}

	// 4. Stub pod для каждого intercept
	interceptStatuses := make([]devenvv1.InterceptStatus, 0, len(devEnv.Spec.Intercepts))
	allReady := true

	for _, intercept := range devEnv.Spec.Intercepts {
		status, err := r.ensureStubPod(ctx, devEnv, ns, intercept)
		interceptStatuses = append(interceptStatuses, status)
		if err != nil || status.Phase != devenvv1.InterceptReady {
			allReady = false
		}
	}

	// 5. Cleanup удалённых intercepts
	if err := r.cleanupOrphanedStubs(ctx, devEnv, ns); err != nil {
		logger.Error(err, "failed to cleanup orphaned stubs")
	}

	// Обновить статус intercepts
	devEnv.Status.Intercepts = interceptStatuses
	if err := r.Status().Update(ctx, devEnv); err != nil {
		return ctrl.Result{}, err
	}

	if allReady {
		return ctrl.Result{RequeueAfter: 5 * time.Minute},
			r.setPhase(ctx, devEnv, devenvv1.PhaseReady, "")
	}

	// Не все готовы — попробовать снова через 10 сек
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// ensureNamespace — создать или убедиться что namespace существует
func (r *DevEnvironmentReconciler) ensureNamespace(ctx context.Context, devEnv *devenvv1.DevEnvironment, ns string) error {
	namespace := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: ns}, namespace)
	if err == nil {
		return nil // уже существует
	}
	if !errors.IsNotFound(err) {
		return err
	}

	namespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				"tunnel4.dev/managed-by": "operator",
				"tunnel4.dev/developer":  devEnv.Spec.Developer,
				"tunnel4.dev/branch":     sanitizeLabel(devEnv.Spec.Branch),
				"tunnel4.dev/env":        devEnv.Name,
			},
		},
	}
	return r.Create(ctx, namespace)
}

// ensureResourceQuota — ограничить ресурсы namespace
func (r *DevEnvironmentReconciler) ensureResourceQuota(ctx context.Context, ns string) error {
	quota := &corev1.ResourceQuota{}
	err := r.Get(ctx, client.ObjectKey{Name: "tunnel4-quota", Namespace: ns}, quota)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	quota = &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel4-quota",
			Namespace: ns,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
				corev1.ResourcePods:   resource.MustParse("20"),
			},
		},
	}
	return r.Create(ctx, quota)
}

// ensureNetworkPolicy — изолировать namespace
func (r *DevEnvironmentReconciler) ensureNetworkPolicy(ctx context.Context, devEnv *devenvv1.DevEnvironment, ns string) error {
	np := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Name: "tunnel4-isolation", Namespace: ns}, np)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	np = &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel4-isolation",
			Namespace: ns,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // все поды в namespace
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Трафик внутри namespace
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"tunnel4.dev/env": devEnv.Name},
						},
					}},
				},
				{
					// Трафик из WireGuard gateway
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"tunnel4.dev/managed-by": "operator"},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"tunnel4.dev/component": "gateway"},
						},
					}},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Весь egress разрешён (поды могут обращаться к внешним сервисам)
				},
			},
		},
	}
	return r.Create(ctx, np)
}

// ensureStubPod — создать stub Deployment + Service для одного intercept
func (r *DevEnvironmentReconciler) ensureStubPod(
	ctx context.Context,
	devEnv *devenvv1.DevEnvironment,
	ns string,
	intercept devenvv1.Intercept,
) (devenvv1.InterceptStatus, error) {

	status := devenvv1.InterceptStatus{
		Service:   intercept.Service,
		LocalPort: intercept.LocalPort,
		Phase:     devenvv1.InterceptPending,
	}

	stubName := intercept.Service + "-stub"
	replicas := int32(1)

	envVars := []corev1.EnvVar{
		{Name: "LISTEN_ADDR", Value: ":8080"},
		{Name: "QUIC_SERVER", Value: gatewayAddr()},
		{Name: "SERVICE_NAME", Value: intercept.Service},
		{Name: "SOURCE_NAMESPACE", Value: "default"},
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stubName,
			Namespace: ns,
			Labels: map[string]string{
				"app":              intercept.Service,
				"tunnel4.dev/stub":    "true",
				"tunnel4.dev/env":     devEnv.Name,
				"tunnel4.dev/service": intercept.Service,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":           intercept.Service,
					"tunnel4.dev/stub": "true",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":              intercept.Service,
						"tunnel4.dev/stub": "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "stub",
						Image:           stubImage(),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env:             envVars,
						Ports:           []corev1.ContainerPort{{ContainerPort: 8080}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
						},
					}},
				},
			},
		},
	}

	// Upsert deployment
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKey{Name: stubName, Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, deployment); err != nil {
			status.Phase = devenvv1.InterceptError
			status.Message = err.Error()
			return status, err
		}
	} else if err == nil {
		// Полностью заменить spec — включая volumes и env
		existing.Spec = deployment.Spec
		existing.Spec.Template.Annotations = deployment.Spec.Template.Annotations
		if err := r.Update(ctx, existing); err != nil {
			status.Phase = devenvv1.InterceptError
			status.Message = err.Error()
			return status, err
		}
		if existing.Status.ReadyReplicas > 0 {
			status.Phase = devenvv1.InterceptReady
		}
	} else {
		status.Phase = devenvv1.InterceptError
		status.Message = err.Error()
		return status, err
	}

	// Upsert Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      intercept.Service,
			Namespace: ns,
			Labels:    map[string]string{"tunnel4.dev/env": devEnv.Name},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":           intercept.Service,
				"tunnel4.dev/stub": "true",
			},
			Ports: []corev1.ServicePort{{
				Port:       8080,
				TargetPort: intstr.FromInt(8080),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}

	existingSvc := &corev1.Service{}
	err = r.Get(ctx, client.ObjectKey{Name: intercept.Service, Namespace: ns}, existingSvc)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, svc); err != nil {
			status.Phase = devenvv1.InterceptError
			status.Message = err.Error()
			return status, err
		}
	} else if err == nil {
		existingSvc.Spec.Selector = svc.Spec.Selector
		if err := r.Update(ctx, existingSvc); err != nil {
			status.Phase = devenvv1.InterceptError
			status.Message = err.Error()
			return status, fmt.Errorf("update service %s: %w", svc.Name, err)
		}
	}

	return status, nil
}

// cleanupOrphanedStubs — удалить stub'ы которых больше нет в spec.intercepts
func (r *DevEnvironmentReconciler) cleanupOrphanedStubs(ctx context.Context, devEnv *devenvv1.DevEnvironment, ns string) error {
	stubList := &appsv1.DeploymentList{}
	if err := r.List(ctx, stubList,
		client.InNamespace(ns),
		client.MatchingLabels{"tunnel4.dev/env": devEnv.Name, "tunnel4.dev/stub": "true"},
	); err != nil {
		return err
	}

	active := map[string]bool{}
	for _, i := range devEnv.Spec.Intercepts {
		active[i.Service+"-stub"] = true
	}

	for i := range stubList.Items {
		stub := &stubList.Items[i]
		if !active[stub.Name] {
			// Удалить deployment
			if err := r.Delete(ctx, stub); err != nil && !errors.IsNotFound(err) {
				return err
			}
			// Удалить service
			svcName := strings.TrimSuffix(stub.Name, "-stub")
			svc := &corev1.Service{}
			if err := r.Get(ctx, client.ObjectKey{Name: svcName, Namespace: ns}, svc); err == nil {
				if err := r.Delete(ctx, svc); err != nil && !errors.IsNotFound(err) {
					return err
				}
			}
		}
	}
	return nil
}

// transitionToSleep — масштабировать все поды до 0
func (r *DevEnvironmentReconciler) transitionToSleep(ctx context.Context, devEnv *devenvv1.DevEnvironment) (ctrl.Result, error) {
	ns := devEnv.Status.Namespace
	deployList := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployList, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, err
	}

	zero := int32(0)
	for i := range deployList.Items {
		d := &deployList.Items[i]
		if *d.Spec.Replicas != 0 {
			d.Spec.Replicas = &zero
			if err := r.Update(ctx, d); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute},
		r.setPhase(ctx, devEnv, devenvv1.PhaseSleeping, "idle timeout")
}

// handleDeletion — cleanup при удалении DevEnvironment
func (r *DevEnvironmentReconciler) handleDeletion(ctx context.Context, devEnv *devenvv1.DevEnvironment) (ctrl.Result, error) {
	if err := r.setPhase(ctx, devEnv, devenvv1.PhaseTerminating, ""); err != nil {
		return ctrl.Result{}, err
	}

	// Удалить namespace — все ресурсы удалятся каскадно
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: devEnv.Status.Namespace}, ns); err == nil {
		if err := r.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Убрать finalizer
	devEnv.Finalizers = removeString(devEnv.Finalizers, finalizerName)
	return ctrl.Result{}, r.Update(ctx, devEnv)
}

// setPhase — обновить фазу в status
func (r *DevEnvironmentReconciler) setPhase(ctx context.Context, devEnv *devenvv1.DevEnvironment, phase devenvv1.DevEnvironmentPhase, msg string) error {
	devEnv.Status.Phase = phase
	devEnv.Status.Message = msg
	return r.Status().Update(ctx, devEnv)
}

// isIdle — проверить не было ли heartbeat дольше idleTimeout
func (r *DevEnvironmentReconciler) isIdle(devEnv *devenvv1.DevEnvironment) bool {
	if devEnv.Status.LastHeartbeat.IsZero() {
		return false
	}
	return time.Since(devEnv.Status.LastHeartbeat.Time) > idleTimeout
}

func (r *DevEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devenvv1.DevEnvironment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// helpers
func buildNamespace(developer, branch string) string {
	r := strings.NewReplacer("/", "-", "_", "-", ".", "-")
	ns := developer + "-" + r.Replace(strings.ToLower(branch))
	if len(ns) > 63 {
		ns = ns[:63]
	}
	return ns
}

func sanitizeLabel(s string) string {
	r := strings.NewReplacer("/", "-", "_", "-")
	return r.Replace(strings.ToLower(s))
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	result := []string{}
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

