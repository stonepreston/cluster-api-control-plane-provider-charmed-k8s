/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bootstrapv1beta1 "github.com/charmed-kubernetes/cluster-api-bootstrap-provider-charmed-k8s/api/v1beta1"

	controlplanev1beta1 "github.com/charmed-kubernetes/cluster-api-control-plane-provider-juju/api/v1beta1"
	"github.com/pkg/errors"
)

// ControlPlane holds business logic around control planes.
// It should never need to connect to a service, that responsibility lies outside of this struct.
type ControlPlane struct {
	KCP      *controlplanev1beta1.JujuControlPlane
	Cluster  *clusterv1.Cluster
	Machines []clusterv1.Machine
}

// JujuControlPlaneReconciler reconciles a JujuControlPlane object
type JujuControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io;bootstrap.cluster.x-k8s.io;controlplane.cluster.x-k8s.io,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the JujuControlPlane object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *JujuControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the JujuControlPlane instance.
	kcp := &controlplanev1beta1.JujuControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get JujuControlPlane")
		return ctrl.Result{}, err
	}

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, kcp.ObjectMeta)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("waiting for cluster owner to be found")
			return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
		}
		log.Error(err, "failed to get owner Cluster")
		return ctrl.Result{}, err
	}

	if cluster == nil {
		log.Info("waiting for cluster owner to be non-nil")
		return ctrl.Result{Requeue: true}, nil
	}

	if annotations.IsPaused(cluster, kcp) {
		log.Info("reconciliation is paused for this object")
		return ctrl.Result{Requeue: true}, nil
	}

	if !cluster.Status.InfrastructureReady {
		log.Info("cluster is not ready yet, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if kcp.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(kcp, controlplanev1beta1.JujuControlPlaneFinalizer) {
			controllerutil.AddFinalizer(kcp, controlplanev1beta1.JujuControlPlaneFinalizer)
			if err := r.Update(ctx, kcp); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("added finalizer")
			return ctrl.Result{}, nil
		}
	} else {
		// The control plane object is being deleted
		log.Info("deleting control plane")
		return r.reconcileDelete(ctx, cluster, kcp)
	}

	// Update ownerrefs on infra templates
	log.Info("updating owner references on infra templates")
	if err := r.reconcileExternalReference(ctx, kcp.Spec.MachineTemplate, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if !cluster.Spec.ControlPlaneEndpoint.IsValid() {
		log.Info("cluster does not yet have a ControlPlaneEndpoint defined")
		return ctrl.Result{}, nil
	}

	// TODO: handle proper adoption of Machines
	log.Info("Getting control plane machines")
	ownedMachines, err := r.getControlPlaneMachinesForCluster(ctx, util.ObjectKey(cluster))
	if err != nil {
		log.Error(err, "failed to retrieve control plane machines for cluster")
		return ctrl.Result{}, err
	}

	log.Info("setting MachinesReady condition based on aggregate status of owned machines")
	conditionGetters := make([]conditions.Getter, len(ownedMachines))
	for i, v := range ownedMachines {
		conditionGetters[i] = &v
	}
	conditions.SetAggregate(kcp, controlplanev1beta1.MachinesReadyCondition, conditionGetters, conditions.AddSourceRef(), conditions.WithStepCounterIf(false))

	log.Info("reconciling machines")
	result, err := r.reconcileMachines(ctx, cluster, kcp, ownedMachines)
	if err != nil {
		log.Error(err, "error reconciling machines")
		return ctrl.Result{}, err
	}

	return result, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JujuControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1beta1.JujuControlPlane{}).
		Complete(r)
}

func (r *JujuControlPlaneReconciler) reconcileExternalReference(ctx context.Context, ref corev1.ObjectReference, cluster *clusterv1.Cluster) error {
	obj, err := external.Get(ctx, r.Client, &ref, cluster.Namespace)
	if err != nil {
		return err
	}

	objPatchHelper, err := patch.NewHelper(obj, r.Client)
	if err != nil {
		return err
	}

	obj.SetOwnerReferences(util.EnsureOwnerRef(obj.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cluster.Name,
		UID:        cluster.UID,
	}))

	return objPatchHelper.Patch(ctx, obj)
}

func (r *JujuControlPlaneReconciler) getControlPlaneMachinesForCluster(ctx context.Context, cluster client.ObjectKey) ([]clusterv1.Machine, error) {
	selector := map[string]string{
		clusterv1.ClusterLabelName:             cluster.Name,
		clusterv1.MachineControlPlaneLabelName: "",
	}

	machineList := clusterv1.MachineList{}
	if err := r.Client.List(
		ctx,
		&machineList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(selector),
	); err != nil {
		return nil, err
	}

	return machineList.Items, nil
}

func (r *JujuControlPlaneReconciler) reconcileMachines(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1beta1.JujuControlPlane, machines []clusterv1.Machine) (res ctrl.Result, err error) {
	log := log.FromContext(ctx)
	// If we've made it this far, we can assume that all ownedMachines are up to date
	numMachines := len(machines)
	desiredReplicas := int(*kcp.Spec.Replicas)

	controlPlane := r.newControlPlane(cluster, kcp, machines)

	switch {
	// We are creating the first replica
	case numMachines < desiredReplicas && numMachines == 0:
		// Create new Machine
		log.Info("initializing control plane")

		return r.bootControlPlane(ctx, cluster, kcp, controlPlane)

	// We are scaling up
	case numMachines < desiredReplicas && numMachines > 0:
		conditions.MarkFalse(kcp, controlplanev1beta1.ResizedCondition, controlplanev1beta1.ScalingUpReason, clusterv1.ConditionSeverityWarning,
			"Scaling up control plane to %d replicas (actual %d)", desiredReplicas, numMachines)

		// Create a new Machine
		log.Info("scaling up control plane")
		return r.bootControlPlane(ctx, cluster, kcp, controlPlane)

	// We are scaling down
	case numMachines > desiredReplicas:
		conditions.MarkFalse(kcp, controlplanev1beta1.ResizedCondition, controlplanev1beta1.ScalingDownReason, clusterv1.ConditionSeverityWarning,
			"Scaling down control plane to %d replicas (actual %d)",
			desiredReplicas, numMachines)

		log.Info("scaling down control plane")
		res, err = r.scaleDownControlPlane(ctx, kcp, util.ObjectKey(cluster), controlPlane.KCP.Name, machines)
		if err != nil {
			if res.Requeue || res.RequeueAfter > 0 {
				log.Error(err, "failed to scale down control plane")
				return res, nil
			}
		}

		return res, err

	default:
		log.Info("updating conditions")
		if conditions.Has(kcp, clusterv1.MachinesReadyCondition) {
			log.Info("marking resized condition true")
			conditions.MarkTrue(kcp, clusterv1.ResizedCondition)
		}
		log.Info("marking machines created condition true")
		conditions.MarkTrue(kcp, clusterv1.MachinesCreatedCondition)
	}

	return ctrl.Result{}, nil

}

func (r *JujuControlPlaneReconciler) newControlPlane(cluster *clusterv1.Cluster, kcp *controlplanev1beta1.JujuControlPlane, machines []clusterv1.Machine) *ControlPlane {
	return &ControlPlane{
		KCP:      kcp,
		Cluster:  cluster,
		Machines: machines,
	}
}

func (r *JujuControlPlaneReconciler) bootControlPlane(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1beta1.JujuControlPlane, controlPlane *ControlPlane) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Since the cloned resource should eventually have a controller ref for the Machine, we create an
	// OwnerReference here without the Controller field set
	infraCloneOwner := &metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "JujuControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
	}

	// Clone the infrastructure template
	infraRef, err := external.CloneTemplate(ctx, &external.CloneTemplateInput{
		Client:      r.Client,
		TemplateRef: &kcp.Spec.MachineTemplate,
		Namespace:   kcp.Namespace,
		OwnerRef:    infraCloneOwner,
		ClusterName: cluster.Name,
	})
	if err != nil {
		conditions.MarkFalse(kcp, clusterv1.MachinesCreatedCondition,
			clusterv1.InfrastructureTemplateCloningFailedReason,
			clusterv1.ConditionSeverityError, err.Error())

		return ctrl.Result{}, err
	}

	// Clone the bootstrap configuration
	bootstrapConfig := &kcp.Spec.ControlPlaneConfig
	bootstrapRef, err := r.generateBootstrapConfig(ctx, kcp, bootstrapConfig)
	if err != nil {
		conditions.MarkFalse(kcp, clusterv1.MachinesCreatedCondition,
			clusterv1.BootstrapTemplateCloningFailedReason,
			clusterv1.ConditionSeverityError, err.Error())

		return ctrl.Result{}, err
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName(kcp.Name + "-"),
			Namespace: kcp.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterLabelName:             cluster.Name,
				clusterv1.MachineControlPlaneLabelName: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kcp, clusterv1.GroupVersion.WithKind("JujuControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       cluster.Name,
			InfrastructureRef: *infraRef,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: bootstrapRef,
			},
			//WARNING: This is a work around, I dont know how this is supposed to be set
		},
	}

	failureDomains := r.getFailureDomain(ctx, cluster)
	if len(failureDomains) > 0 {
		machine.Spec.FailureDomain = &failureDomains[rand.Intn(len(failureDomains))]
	}

	if err := r.Client.Create(ctx, machine); err != nil {
		conditions.MarkFalse(kcp, clusterv1.MachinesCreatedCondition,
			clusterv1.MachineCreationFailedReason,
			clusterv1.ConditionSeverityError, err.Error())

		return ctrl.Result{}, errors.Wrap(err, "Failed to create machine")
	}

	log.Info("created machine", "machine", machine)
	return ctrl.Result{Requeue: true}, nil
}

// getFailureDomain will return a slice of failure domains from the cluster status.
func (r *JujuControlPlaneReconciler) getFailureDomain(ctx context.Context, cluster *clusterv1.Cluster) []string {
	if cluster.Status.FailureDomains == nil {
		return nil
	}

	retList := []string{}
	for key := range cluster.Status.FailureDomains {
		retList = append(retList, key)
	}
	return retList
}

func (r *JujuControlPlaneReconciler) scaleDownControlPlane(ctx context.Context, kcp *controlplanev1beta1.JujuControlPlane, cluster client.ObjectKey, cpName string, machines []clusterv1.Machine) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if len(machines) == 0 {
		return ctrl.Result{}, fmt.Errorf("no machines found")
	}
	log.WithValues("machines", len(machines)).Info("found control plane machines")
	deleteMachine := machines[len(machines)-1]
	machine := machines[len(machines)-1]
	for i := len(machines) - 1; i >= 0; i-- {
		machine = machines[i]
		logger := log.WithValues("machineName", machine.Name)
		if !machine.ObjectMeta.DeletionTimestamp.IsZero() {
			logger.Info("machine is in process of deletion")
		}
		// mark the oldest machine to be deleted first
		if machine.CreationTimestamp.Before(&deleteMachine.CreationTimestamp) {
			deleteMachine = machine
		}
	}

	log.WithValues("machineName", deleteMachine.Name).Info("deleting machine")

	err := r.Client.Delete(ctx, &deleteMachine)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Requeue so that we handle any additional scaling.
	return ctrl.Result{Requeue: true}, nil
}

func (r *JujuControlPlaneReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1beta1.JujuControlPlane) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	// Get list of all control plane machines
	ownedMachines, err := r.getControlPlaneMachinesForCluster(ctx, util.ObjectKey(cluster))
	if err != nil {
		return ctrl.Result{}, err
	}

	// If no control plane machines remain, remove the finalizer
	if len(ownedMachines) == 0 {
		log.Info("no machines exist")
		if controllerutil.ContainsFinalizer(kcp, controlplanev1beta1.JujuControlPlaneFinalizer) {
			log.Info("removing finalizer and stopping reconciliation")
			controllerutil.RemoveFinalizer(kcp, controlplanev1beta1.JujuControlPlaneFinalizer)
			return ctrl.Result{}, r.Client.Update(ctx, kcp)
		}
	}

	for _, ownedMachine := range ownedMachines {
		// Already deleting this machine
		if !ownedMachine.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}
		// Submit deletion request
		if err := r.Client.Delete(ctx, &ownedMachine); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// TODO: clean up secrets for kubeconfig once that is implemented

	conditions.MarkFalse(kcp, clusterv1.ResizedCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
	// Requeue the deletion so we can check to make sure machines got cleaned up
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *JujuControlPlaneReconciler) generateBootstrapConfig(ctx context.Context, kcp *controlplanev1beta1.JujuControlPlane, spec *bootstrapv1beta1.CharmedK8sConfigSpec) (*corev1.ObjectReference, error) {
	log := log.FromContext(ctx)
	log.Info("generating bootstrap config", "spec", spec)
	owner := metav1.OwnerReference{
		APIVersion:         clusterv1.GroupVersion.String(),
		Kind:               "JujuControlPlane",
		Name:               kcp.Name,
		UID:                kcp.UID,
		BlockOwnerDeletion: pointer.BoolPtr(true),
	}

	bootstrapConfig := &bootstrapv1beta1.CharmedK8sConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:            names.SimpleNameGenerator.GenerateName(kcp.Name + "-"),
			Namespace:       kcp.Namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: *spec,
	}

	if err := r.Client.Create(ctx, bootstrapConfig); err != nil {
		return nil, errors.Wrap(err, "Failed to create bootstrap configuration")
	}

	bootstrapRef := &corev1.ObjectReference{
		APIVersion: bootstrapv1beta1.GroupVersion.String(),
		Kind:       "CharmedK8sConfig",
		Name:       bootstrapConfig.GetName(),
		Namespace:  bootstrapConfig.GetNamespace(),
		UID:        bootstrapConfig.GetUID(),
	}

	return bootstrapRef, nil
}
