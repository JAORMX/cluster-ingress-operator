package ingress

import (
	"context"
	"fmt"

	iov1 "github.com/openshift/cluster-ingress-operator/pkg/api/v1"
	logf "github.com/openshift/cluster-ingress-operator/pkg/log"
	"github.com/openshift/cluster-ingress-operator/pkg/manifests"
	"github.com/openshift/cluster-ingress-operator/pkg/util/slice"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/client-go/tools/record"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	controllerName = "ingress_controller"
)

var log = logf.Logger.WithName(controllerName)

// New creates the ingress controller from configuration. This is the controller
// that handles all the logic for implementing ingress based on
// IngressController resources.
//
// The controller will be pre-configured to watch for IngressController resources
// in the manager namespace.
func New(mgr manager.Manager, config Config) (controller.Controller, error) {
	reconciler := &reconciler{
		Config:   config,
		client:   mgr.GetClient(),
		cache:    mgr.GetCache(),
		recorder: mgr.GetEventRecorderFor(controllerName),
	}
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: reconciler})
	if err != nil {
		return nil, err
	}
	if err := c.Watch(&source.Kind{Type: &operatorv1.IngressController{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return nil, err
	}
	if err := c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, enqueueRequestForOwningIngressController(config.Namespace)); err != nil {
		return nil, err
	}
	if err := c.Watch(&source.Kind{Type: &corev1.Service{}}, enqueueRequestForOwningIngressController(config.Namespace)); err != nil {
		return nil, err
	}
	if err := c.Watch(&source.Kind{Type: &iov1.DNSRecord{}}, &handler.EnqueueRequestForOwner{OwnerType: &operatorv1.IngressController{}}); err != nil {
		return nil, err
	}
	return c, nil
}

func enqueueRequestForOwningIngressController(namespace string) handler.EventHandler {
	return &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(a handler.MapObject) []reconcile.Request {
			labels := a.Meta.GetLabels()
			if ingressName, ok := labels[manifests.OwningIngressControllerLabel]; ok {
				log.Info("queueing ingress", "name", ingressName, "related", a.Meta.GetSelfLink())
				return []reconcile.Request{
					{
						NamespacedName: types.NamespacedName{
							Namespace: namespace,
							Name:      ingressName,
						},
					},
				}
			} else {
				return []reconcile.Request{}
			}
		}),
	}
}

// Config holds all the things necessary for the controller to run.
type Config struct {
	Namespace              string
	IngressControllerImage string
}

// reconciler handles the actual ingress reconciliation logic in response to
// events.
type reconciler struct {
	Config

	client   client.Client
	cache    cache.Cache
	recorder record.EventRecorder
}

// Reconcile expects request to refer to a ingresscontroller in the operator
// namespace, and will do all the work to ensure the ingresscontroller is in the
// desired state.
func (r *reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("reconciling", "request", request)

	// Only proceed if we can get the ingresscontroller's state.
	ingress := &operatorv1.IngressController{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, ingress); err != nil {
		if errors.IsNotFound(err) {
			// This means the ingress was already deleted/finalized and there are
			// stale queue entries (or something edge triggering from a related
			// resource that got deleted async).
			log.Info("ingresscontroller not found; reconciliation will be skipped", "request", request)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get ingresscontroller %q: %v", request, err)
	}

	// If the ingresscontroller is deleted, handle that and return early.
	if ingress.DeletionTimestamp != nil {
		if err := r.ensureIngressDeleted(ingress); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to ensure ingress deletion: %v", err)
		}
		log.Info("ingresscontroller was successfully deleted", "ingresscontroller", ingress)
		return reconcile.Result{}, nil
	}

	// Only proceed if we can collect cluster config.
	dnsConfig := &configv1.DNS{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, dnsConfig); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get dns 'cluster': %v", err)
	}
	infraConfig := &configv1.Infrastructure{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, infraConfig); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get infrastructure 'cluster': %v", err)
	}
	ingressConfig := &configv1.Ingress{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, ingressConfig); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get ingress 'cluster': %v", err)
	}

	// Admit if necessary. Don't process until admission succeeds. If admission is
	// successful, immediately re-queue to refresh state.
	if !isAdmitted(ingress) {
		if err := r.admit(ingress, ingressConfig, infraConfig); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to admit ingresscontroller: %v", err)
		}
		log.Info("admitted ingresscontroller", "ingresscontroller", ingress)
		// Just re-queue for simplicity
		return reconcile.Result{Requeue: true}, nil
	}

	// The ingresscontroller is safe to process, so ensure it.
	if err := r.ensureIngressController(ingress, dnsConfig, infraConfig); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to ensure ingresscontroller: %v", err)
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) admit(current *operatorv1.IngressController, ingressConfig *configv1.Ingress, infraConfig *configv1.Infrastructure) error {
	updated := current.DeepCopy()

	setDefaultDomain(updated, ingressConfig)
	setDefaultPublishingStrategy(updated, infraConfig)

	if err := r.validate(updated); err != nil {
		updated.Status.Conditions = mergeConditions(updated.Status.Conditions, operatorv1.OperatorCondition{
			Type:    iov1.IngressControllerAdmittedConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "Invalid",
			Message: fmt.Sprintf("%v", err),
		})
		if !ingressStatusesEqual(current.Status, updated.Status) {
			if err := r.client.Status().Update(context.TODO(), updated); err != nil {
				return fmt.Errorf("failed to update status: %v", err)
			}
		}
		return nil
	}

	updated.Status.Conditions = mergeConditions(updated.Status.Conditions, operatorv1.OperatorCondition{
		Type:   iov1.IngressControllerAdmittedConditionType,
		Status: operatorv1.ConditionTrue,
		Reason: "Valid",
	})
	if !ingressStatusesEqual(current.Status, updated.Status) {
		if err := r.client.Status().Update(context.TODO(), updated); err != nil {
			return fmt.Errorf("failed to update status: %v", err)
		}
	}
	return nil
}

func isAdmitted(ic *operatorv1.IngressController) bool {
	for _, cond := range ic.Status.Conditions {
		if cond.Type == iov1.IngressControllerAdmittedConditionType && cond.Status == operatorv1.ConditionTrue {
			return true
		}
	}
	return false
}

func setDefaultDomain(ic *operatorv1.IngressController, ingressConfig *configv1.Ingress) bool {
	var effectiveDomain string
	switch {
	case len(ic.Spec.Domain) > 0:
		effectiveDomain = ic.Spec.Domain
	default:
		effectiveDomain = ingressConfig.Spec.Domain
	}
	if len(ic.Status.Domain) == 0 {
		ic.Status.Domain = effectiveDomain
		return true
	}
	return false
}

func setDefaultPublishingStrategy(ic *operatorv1.IngressController, infraConfig *configv1.Infrastructure) bool {
	effectiveStrategy := ic.Spec.EndpointPublishingStrategy
	if effectiveStrategy == nil {
		var strategyType operatorv1.EndpointPublishingStrategyType
		switch infraConfig.Status.Platform {
		case configv1.AWSPlatformType, configv1.AzurePlatformType, configv1.GCPPlatformType:
			strategyType = operatorv1.LoadBalancerServiceStrategyType
		case configv1.LibvirtPlatformType:
			strategyType = operatorv1.HostNetworkStrategyType
		default:
			strategyType = operatorv1.HostNetworkStrategyType
		}
		effectiveStrategy = &operatorv1.EndpointPublishingStrategy{
			Type: strategyType,
		}
	}
	switch effectiveStrategy.Type {
	case operatorv1.LoadBalancerServiceStrategyType:
		if effectiveStrategy.LoadBalancer == nil {
			effectiveStrategy.LoadBalancer = &operatorv1.LoadBalancerStrategy{
				Scope: operatorv1.ExternalLoadBalancer,
			}
		}
	case operatorv1.HostNetworkStrategyType:
		// No parameters.
	case operatorv1.PrivateStrategyType:
		// No parameters.
	}
	if ic.Status.EndpointPublishingStrategy == nil {
		ic.Status.EndpointPublishingStrategy = effectiveStrategy
		return true
	}
	return false
}

func (r *reconciler) validate(ic *operatorv1.IngressController) error {
	var errors []error

	ingresses := &operatorv1.IngressControllerList{}
	if err := r.cache.List(context.TODO(), ingresses, client.InNamespace(r.Namespace)); err != nil {
		return fmt.Errorf("failed to list ingresscontrollers: %v", err)
	}

	if err := validateDomain(ic); err != nil {
		errors = append(errors, err)
	}
	if err := validateDomainUniqueness(ic, ingresses.Items); err != nil {
		errors = append(errors, err)
	}

	return utilerrors.NewAggregate(errors)
}

func validateDomain(ic *operatorv1.IngressController) error {
	if len(ic.Status.Domain) == 0 {
		return fmt.Errorf("domain is required")
	}
	return nil
}

// validateDomainUniqueness returns an error if the desired controller's domain
// conflicts with any other admitted controllers.
func validateDomainUniqueness(desired *operatorv1.IngressController, existing []operatorv1.IngressController) error {
	for i := range existing {
		current := existing[i]
		if !isAdmitted(&current) {
			continue
		}
		if desired.UID != current.UID && desired.Status.Domain == current.Status.Domain {
			return fmt.Errorf("conflicts with: %s", current.Name)
		}
	}

	return nil
}

// ensureIngressDeleted tries to delete ingress, and if successful, will remove
// the finalizer.
func (r *reconciler) ensureIngressDeleted(ingress *operatorv1.IngressController) error {
	if err := r.finalizeLoadBalancerService(ingress); err != nil {
		return fmt.Errorf("failed to finalize load balancer service for %s: %v", ingress.Name, err)
	}
	log.Info("finalized load balancer service for ingress", "namespace", ingress.Namespace, "name", ingress.Name)

	// Delete the wildcard DNS record, and block ingresscontroller finalization
	// until the dnsrecord has been finalized.
	if err := r.deleteWildcardDNSRecord(ingress); err != nil {
		return fmt.Errorf("failed to delete wildcard dnsrecord: %v", err)
	}
	if record, err := r.currentWildcardDNSRecord(ingress); err != nil {
		return fmt.Errorf("failed to get current wildcard dnsrecord: %v", err)
	} else {
		if record != nil {
			log.V(1).Info("waiting for wildcard dnsrecord to be deleted", "dnsrecord", record)
			return nil
		}
	}

	if err := r.ensureRouterDeleted(ingress); err != nil {
		return fmt.Errorf("failed to delete deployment for ingress %s: %v", ingress.Name, err)
	}
	log.Info("deleted deployment for ingress", "namespace", ingress.Namespace, "name", ingress.Name)

	// Clean up the finalizer to allow the ingresscontroller to be deleted.
	if slice.ContainsString(ingress.Finalizers, manifests.IngressControllerFinalizer) {
		updated := ingress.DeepCopy()
		updated.Finalizers = slice.RemoveString(updated.Finalizers, manifests.IngressControllerFinalizer)
		if err := r.client.Update(context.TODO(), updated); err != nil {
			return fmt.Errorf("failed to remove finalizer from ingresscontroller %s: %v", ingress.Name, err)
		}
	}
	return nil
}

// ensureIngressController ensures all necessary router resources exist for a given ingresscontroller.
func (r *reconciler) ensureIngressController(ci *operatorv1.IngressController, dnsConfig *configv1.DNS, infraConfig *configv1.Infrastructure) error {
	// Before doing anything at all with the controller, ensure it has a finalizer
	// so we can clean up later.
	if !slice.ContainsString(ci.Finalizers, manifests.IngressControllerFinalizer) {
		updated := ci.DeepCopy()
		updated.Finalizers = append(updated.Finalizers, manifests.IngressControllerFinalizer)
		if err := r.client.Update(context.TODO(), updated); err != nil {
			return fmt.Errorf("failed to update finalizers: %v", err)
		}
		if err := r.client.Get(context.TODO(), types.NamespacedName{Namespace: updated.Namespace, Name: updated.Name}, updated); err != nil {
			return fmt.Errorf("failed to get ingresscontroller: %v", err)
		}
		ci = updated
	}

	if err := r.ensureRouterNamespace(); err != nil {
		return fmt.Errorf("failed to ensure namespace: %v", err)
	}

	deployment, err := r.ensureRouterDeployment(ci, infraConfig)
	if err != nil {
		return fmt.Errorf("failed to ensure deployment: %v", err)
	}

	var errs []error
	trueVar := true
	deploymentRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment.Name,
		UID:        deployment.UID,
		Controller: &trueVar,
	}

	var lbService *corev1.Service
	var wildcardRecord *iov1.DNSRecord
	if lb, err := r.ensureLoadBalancerService(ci, deploymentRef, infraConfig); err != nil {
		errs = append(errs, fmt.Errorf("failed to ensure load balancer service for %s: %v", ci.Name, err))
	} else {
		lbService = lb
		if record, err := r.ensureWildcardDNSRecord(ci, lbService); err != nil {
			errs = append(errs, fmt.Errorf("failed to ensure wildcard dnsrecord for %s: %v", ci.Name, err))
		} else {
			wildcardRecord = record
		}
	}

	if internalSvc, err := r.ensureInternalIngressControllerService(ci, deploymentRef); err != nil {
		errs = append(errs, fmt.Errorf("failed to create internal router service for ingresscontroller %s: %v", ci.Name, err))
	} else if err := r.ensureMetricsIntegration(ci, internalSvc, deploymentRef); err != nil {
		errs = append(errs, fmt.Errorf("failed to integrate metrics with openshift-monitoring for ingresscontroller %s: %v", ci.Name, err))
	}

	if _, _, err := r.ensureRouterPodDisruptionBudget(ci, deploymentRef); err != nil {
		errs = append(errs, err)
	}

	operandEvents := &corev1.EventList{}
	if err := r.cache.List(context.TODO(), operandEvents, client.InNamespace("openshift-ingress")); err != nil {
		errs = append(errs, fmt.Errorf("failed to list events in namespace %q: %v", "openshift-ingress", err))
	}

	if err := r.syncIngressControllerStatus(ci, deployment, lbService, operandEvents.Items, wildcardRecord, dnsConfig); err != nil {
		errs = append(errs, fmt.Errorf("failed to sync ingresscontroller status: %v", err))
	}

	return utilerrors.NewAggregate(errs)
}

// IsStatusDomainSet checks whether status.domain of ingress is set.
func IsStatusDomainSet(ingress *operatorv1.IngressController) bool {
	if len(ingress.Status.Domain) == 0 {
		return false
	}
	return true
}
