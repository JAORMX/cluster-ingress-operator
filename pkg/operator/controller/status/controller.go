package status

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	logf "github.com/openshift/cluster-ingress-operator/pkg/log"
	"github.com/openshift/cluster-ingress-operator/pkg/manifests"
	operatorcontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	OperatorVersionName          = "operator"
	IngressControllerVersionName = "ingress-controller"
	UnknownVersionValue          = "unknown"

	ingressesEqualConditionMessage = "desired and current number of IngressControllers are equal"

	controllerName = "status_controller"
)

var log = logf.Logger.WithName(controllerName)

// New creates the status controller. This is the controller that handles all
// the logic for creating the ClusterOperator operator and updating its status.
//
// The controller watches IngressController resources in the manager namespace
// and uses them to compute the operator status.
func New(mgr manager.Manager, config Config) (controller.Controller, error) {
	reconciler := &reconciler{
		Config: config,
		client: mgr.GetClient(),
		cache:  mgr.GetCache(),
	}
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: reconciler})
	if err != nil {
		return nil, err
	}
	if err := c.Watch(&source.Kind{Type: &operatorv1.IngressController{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return nil, err
	}
	return c, nil
}

// Config holds all the things necessary for the controller to run.
type Config struct {
	IngressControllerImage string
	OperatorReleaseVersion string
	Namespace              string
}

// reconciler handles the actual status reconciliation logic in response to
// events.
type reconciler struct {
	Config

	client client.Client
	cache  cache.Cache
}

// Reconcile computes the operator's current status and therefrom creates or
// updates the ClusterOperator resource for the operator.
func (r *reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("Reconciling", "request", request)

	ns := manifests.RouterNamespace()

	co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: operatorcontroller.IngressClusterOperatorName().Name}}
	if err := r.client.Get(context.TODO(), operatorcontroller.IngressClusterOperatorName(), co); err != nil {
		if errors.IsNotFound(err) {
			initializeClusterOperator(co)
			if err := r.client.Create(context.TODO(), co); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to create clusteroperator %s: %v", co.Name, err)
			}
			log.Info("created clusteroperator", "object", co)
		} else {
			return reconcile.Result{}, fmt.Errorf("failed to get clusteroperator %s: %v", co.Name, err)
		}
	}
	oldStatus := co.Status.DeepCopy()

	ingresses, ns, err := r.getOperatorState(ns.Name)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get operator state: %v", err)
	}

	related := []configv1.ObjectReference{
		{
			Resource: "namespaces",
			Name:     "openshift-ingress-operator",
		},
	}
	if ns != nil {
		related = append(related, configv1.ObjectReference{
			Resource: "namespaces",
			Name:     ns.Name,
		})
	}
	for _, ingress := range ingresses {
		related = append(related, configv1.ObjectReference{
			Group:     operatorv1.GroupName,
			Resource:  "IngressController",
			Namespace: r.Namespace,
			Name:      ingress.Name,
		})
	}
	co.Status.RelatedObjects = related

	allIngressesAvailable := checkAllIngressesAvailable(ingresses)

	co.Status.Versions = r.computeOperatorStatusVersions(oldStatus.Versions, allIngressesAvailable)
	co.Status.Conditions = r.computeOperatorStatusConditions(oldStatus.Conditions,
		ns, allIngressesAvailable, oldStatus.Versions, co.Status.Versions)

	if !operatorStatusesEqual(*oldStatus, co.Status) {
		if err := r.client.Status().Update(context.TODO(), co); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to update clusteroperator %s: %v", co.Name, err)
		}
	}

	return reconcile.Result{}, nil
}

// Populate versions and conditions in cluster operator status as CVO expects these fields.
func initializeClusterOperator(co *configv1.ClusterOperator) {
	co.Status.Versions = []configv1.OperandVersion{
		{
			Name:    OperatorVersionName,
			Version: UnknownVersionValue,
		},
		{
			Name:    IngressControllerVersionName,
			Version: UnknownVersionValue,
		},
	}
	co.Status.Conditions = []configv1.ClusterOperatorStatusCondition{
		{
			Type:   configv1.OperatorDegraded,
			Status: configv1.ConditionUnknown,
		},
		{
			Type:   configv1.OperatorProgressing,
			Status: configv1.ConditionUnknown,
		},
		{
			Type:   configv1.OperatorAvailable,
			Status: configv1.ConditionUnknown,
		},
	}
}

// getOperatorState gets and returns the resources necessary to compute the
// operator's current state.
func (r *reconciler) getOperatorState(nsName string) ([]operatorv1.IngressController, *corev1.Namespace, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: nsName}, ns); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("error getting Namespace %s: %v", nsName, err)
	}

	ingressList := &operatorv1.IngressControllerList{}
	if err := r.cache.List(context.TODO(), ingressList, client.InNamespace(r.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("failed to list IngressControllers: %v", err)
	}

	return ingressList.Items, ns, nil
}

// computeOperatorStatusVersions computes the operator's current versions.
func (r *reconciler) computeOperatorStatusVersions(oldVersions []configv1.OperandVersion, allIngressesAvailable bool) []configv1.OperandVersion {
	// We need to report old version until the operator fully transitions to the new version.
	// https://github.com/openshift/cluster-version-operator/blob/master/docs/dev/clusteroperator.md#version-reporting-during-an-upgrade
	if !allIngressesAvailable {
		return oldVersions
	}

	return []configv1.OperandVersion{
		{
			Name:    OperatorVersionName,
			Version: r.OperatorReleaseVersion,
		},
		{
			Name:    IngressControllerVersionName,
			Version: r.IngressControllerImage,
		},
	}
}

// computeOperatorStatusConditions computes the operator's current state.
func (r *reconciler) computeOperatorStatusConditions(oldConditions []configv1.ClusterOperatorStatusCondition,
	ns *corev1.Namespace, allIngressesAvailable bool,
	oldVersions, curVersions []configv1.OperandVersion) []configv1.ClusterOperatorStatusCondition {
	var oldDegradedCondition, oldProgressingCondition, oldAvailableCondition *configv1.ClusterOperatorStatusCondition
	for i := range oldConditions {
		switch oldConditions[i].Type {
		case configv1.OperatorDegraded:
			oldDegradedCondition = &oldConditions[i]
		case configv1.OperatorProgressing:
			oldProgressingCondition = &oldConditions[i]
		case configv1.OperatorAvailable:
			oldAvailableCondition = &oldConditions[i]
		}
	}

	conditions := []configv1.ClusterOperatorStatusCondition{
		computeOperatorDegradedCondition(oldDegradedCondition, ns),
		r.computeOperatorProgressingCondition(oldProgressingCondition, allIngressesAvailable, oldVersions, curVersions),
		computeOperatorAvailableCondition(oldAvailableCondition, allIngressesAvailable),
	}

	return conditions
}

// checkAllIngressesAvailable checks if all the ingress controllers are available.
func checkAllIngressesAvailable(ingresses []operatorv1.IngressController) bool {
	for _, ing := range ingresses {
		available := false
		for _, c := range ing.Status.Conditions {
			if c.Type == operatorv1.IngressControllerAvailableConditionType && c.Status == operatorv1.ConditionTrue {
				available = true
				break
			}
		}
		if !available {
			return false
		}
	}

	return (len(ingresses) != 0)
}

// computeOperatorDegradedCondition computes the operator's current Degraded status state.
func computeOperatorDegradedCondition(oldCondition *configv1.ClusterOperatorStatusCondition,
	ns *corev1.Namespace) configv1.ClusterOperatorStatusCondition {
	degradedCondition := configv1.ClusterOperatorStatusCondition{
		Type: configv1.OperatorDegraded,
	}
	if ns == nil {
		degradedCondition.Status = configv1.ConditionTrue
		degradedCondition.Reason = "NoNamespace"
		degradedCondition.Message = "operand namespace does not exist"
	} else {
		degradedCondition.Status = configv1.ConditionFalse
		degradedCondition.Message = "operand namespace exists"
	}

	setLastTransitionTime(&degradedCondition, oldCondition)
	return degradedCondition
}

// computeOperatorProgressingCondition computes the operator's current Progressing status state.
func (r *reconciler) computeOperatorProgressingCondition(oldCondition *configv1.ClusterOperatorStatusCondition,
	allIngressesAvailable bool, oldVersions, curVersions []configv1.OperandVersion) configv1.ClusterOperatorStatusCondition {
	// TODO: Update progressingCondition when an ingresscontroller
	//       progressing condition is created. The Operator's condition
	//       should be derived from the ingresscontroller's condition.
	progressingCondition := configv1.ClusterOperatorStatusCondition{
		Type: configv1.OperatorProgressing,
	}

	progressing := false

	messages := []string{}
	if !allIngressesAvailable {
		messages = append(messages, "Not all ingress controllers are available.")
		progressing = true
	}

	oldVersionsMap := make(map[string]string)
	for _, opv := range oldVersions {
		oldVersionsMap[opv.Name] = opv.Version
	}

	for _, opv := range curVersions {
		if oldVersion, ok := oldVersionsMap[opv.Name]; ok && oldVersion != opv.Version {
			messages = append(messages, fmt.Sprintf("Upgraded %s to %q.", opv.Name, opv.Version))
		}
		switch opv.Name {
		case OperatorVersionName:
			if opv.Version != r.OperatorReleaseVersion {
				messages = append(messages, fmt.Sprintf("Moving to release version %q.", r.OperatorReleaseVersion))
				progressing = true
			}
		case IngressControllerVersionName:
			if opv.Version != r.IngressControllerImage {
				messages = append(messages, fmt.Sprintf("Moving to ingress-controller image version %q.", r.IngressControllerImage))
				progressing = true
			}
		}
	}

	if progressing {
		progressingCondition.Status = configv1.ConditionTrue
		progressingCondition.Reason = "Reconciling"
	} else {
		progressingCondition.Status = configv1.ConditionFalse
	}
	progressingCondition.Message = ingressesEqualConditionMessage
	if len(messages) > 0 {
		progressingCondition.Message = strings.Join(messages, "\n")
	}

	setLastTransitionTime(&progressingCondition, oldCondition)
	return progressingCondition
}

// computeOperatorAvailableCondition computes the operator's current Available status state.
func computeOperatorAvailableCondition(oldCondition *configv1.ClusterOperatorStatusCondition,
	allIngressesAvailable bool) configv1.ClusterOperatorStatusCondition {
	availableCondition := configv1.ClusterOperatorStatusCondition{
		Type: configv1.OperatorAvailable,
	}

	if allIngressesAvailable {
		availableCondition.Status = configv1.ConditionTrue
		availableCondition.Message = ingressesEqualConditionMessage
	} else {
		availableCondition.Status = configv1.ConditionFalse
		availableCondition.Reason = "IngressUnavailable"
		availableCondition.Message = "Not all ingress controllers are available."
	}

	setLastTransitionTime(&availableCondition, oldCondition)
	return availableCondition
}

// setLastTransitionTime sets LastTransitionTime for the given condition.
// If the condition has changed, it will assign a new timestamp otherwise keeps the old timestamp.
func setLastTransitionTime(condition, oldCondition *configv1.ClusterOperatorStatusCondition) {
	if oldCondition != nil && condition.Status == oldCondition.Status &&
		condition.Reason == oldCondition.Reason && condition.Message == oldCondition.Message {
		condition.LastTransitionTime = oldCondition.LastTransitionTime
	} else {
		condition.LastTransitionTime = metav1.Now()
	}
}

// operatorStatusesEqual compares two ClusterOperatorStatus values.  Returns
// true if the provided ClusterOperatorStatus values should be considered equal
// for the purpose of determining whether an update is necessary, false otherwise.
func operatorStatusesEqual(a, b configv1.ClusterOperatorStatus) bool {
	conditionCmpOpts := []cmp.Option{
		cmpopts.EquateEmpty(),
		cmpopts.SortSlices(func(a, b configv1.ClusterOperatorStatusCondition) bool { return a.Type < b.Type }),
	}
	if !cmp.Equal(a.Conditions, b.Conditions, conditionCmpOpts...) {
		return false
	}

	relatedCmpOpts := []cmp.Option{
		cmpopts.EquateEmpty(),
		cmpopts.SortSlices(func(a, b configv1.ObjectReference) bool { return a.Name < b.Name }),
	}
	if !cmp.Equal(a.RelatedObjects, b.RelatedObjects, relatedCmpOpts...) {
		return false
	}

	versionsCmpOpts := []cmp.Option{
		cmpopts.EquateEmpty(),
		cmpopts.SortSlices(func(a, b configv1.OperandVersion) bool { return a.Name < b.Name }),
	}
	if !cmp.Equal(a.Versions, b.Versions, versionsCmpOpts...) {
		return false
	}

	return true
}
