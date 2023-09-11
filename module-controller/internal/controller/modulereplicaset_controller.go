/*
Copyright 2023.

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
	"fmt"
	"github.com/sofastack/sofa-serverless/internal/constants/finalizer"
	label "github.com/sofastack/sofa-serverless/internal/constants/label"
	"github.com/sofastack/sofa-serverless/internal/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	moduledeploymentv1alpha1 "github.com/sofastack/sofa-serverless/api/v1alpha1"
)

// ModuleReplicaSetReconciler reconciles a ModuleReplicaSet object
type ModuleReplicaSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=serverless.alipay.com,resources=modulereplicasets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=serverless.alipay.com,resources=modulereplicasets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=serverless.alipay.com,resources=modulereplicasets/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ModuleReplicaSet object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.0/pkg/reconcile
func (r *ModuleReplicaSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.Log.Info("start reconcile for moduleReplicaSet", "request", req)
	defer log.Log.Info("finish reconcile for moduleReplicaSet", "request", req)
	// get the moduleReplicaSet
	moduleReplicaSet := &moduledeploymentv1alpha1.ModuleReplicaSet{}
	err := r.Client.Get(ctx, req.NamespacedName, moduleReplicaSet)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Log.Info("moduleReplicaSet is deleted", "moduleReplicaSetName", moduleReplicaSet.Name)
			return reconcile.Result{}, nil
		}
		log.Log.Error(err, "Failed to get moduleReplicaSet", "moduleReplicaSetName", moduleReplicaSet.Name)
		return ctrl.Result{}, nil
	}

	// get the existed module
	existedModuleList := &moduledeploymentv1alpha1.ModuleList{}
	err = r.Client.List(ctx, existedModuleList, &client.ListOptions{Namespace: req.Namespace, LabelSelector: labels.SelectorFromSet(map[string]string{
		label.ModuleReplicasetLabel: moduleReplicaSet.Name,
	})})
	if err != nil {
		log.Log.Error(err, "Failed to list existedModuleList", "moduleReplicaSetName", moduleReplicaSet.Name)
		return ctrl.Result{}, nil
	}

	if moduleReplicaSet.DeletionTimestamp != nil {
		return r.handleDeletingModuleReplicaSet(ctx, existedModuleList, moduleReplicaSet)
	}

	// compare replicas
	if int(moduleReplicaSet.Spec.Replicas) != len(existedModuleList.Items) {
		// replicas change
		deltaReplicas := int(moduleReplicaSet.Spec.Replicas) - len(existedModuleList.Items)
		if deltaReplicas > 0 {
			// scale up
			err = r.scaleup(ctx, existedModuleList, moduleReplicaSet)
			if err != nil {
				return reconcile.Result{}, err
			}
		} else {
			// scale down
			err = r.scaledown(ctx, existedModuleList, moduleReplicaSet)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	// compare and update module
	err = r.compareAndUpdateModule(ctx, existedModuleList, moduleReplicaSet)
	if err != nil {
		return reconcile.Result{}, err
	}

	return ctrl.Result{}, nil
}

// scale up module
func (r *ModuleReplicaSetReconciler) scaleup(ctx context.Context, existedModuleList *moduledeploymentv1alpha1.ModuleList, moduleReplicaSet *moduledeploymentv1alpha1.ModuleReplicaSet) error {
	log.Log.Info("start scaleup module", "moduleReplicaSetName", moduleReplicaSet.Name)
	deltaReplicas := int(moduleReplicaSet.Spec.Replicas) - len(existedModuleList.Items)
	selector, err := metav1.LabelSelectorAsSelector(&moduleReplicaSet.Spec.Selector)
	selectedPods := &corev1.PodList{}
	if err = r.List(ctx, selectedPods, &client.ListOptions{Namespace: moduleReplicaSet.Namespace, LabelSelector: selector}); err != nil {
		log.Log.Error(err, "Failed to list pod", "moduleReplicaSetName", moduleReplicaSet.Name)
		return err
	}

	usedPodNames := make(map[string]bool)
	for _, module := range existedModuleList.Items {
		usedPodNames[module.Labels[label.BaseInstanceNameLabel]] = true
	}

	// allocate pod
	var toAllocatePod []corev1.Pod
	count := deltaReplicas
	for _, pod := range selectedPods.Items {
		if _, ok := usedPodNames[pod.Name]; !ok {
			toAllocatePod = append(toAllocatePod, pod)
			if count--; count == 0 {
				break
			}
		}
	}

	for _, pod := range toAllocatePod {
		pod.Labels[fmt.Sprintf("%s-%s", label.ModuleNameLabel, moduleReplicaSet.Spec.Template.Spec.Module.Name)] = moduleReplicaSet.Spec.Template.Spec.Module.Version
		if _, exist := pod.Labels[label.ModuleInstanceCount]; exist {
			count, err := strconv.Atoi(pod.Labels[label.ModuleInstanceCount])
			if err != nil {
				log.Log.Error(err, "failed to update module count")
			} else {
				pod.Labels[label.ModuleInstanceCount] = strconv.Itoa(count + 1)
			}
		} else {
			pod.Labels[label.ModuleInstanceCount] = "1"
		}
		err := r.Client.Update(ctx, &pod)
		// TODO add pod finalizer
		if err != nil {
			// update pod label
			return err
		}
		// create module
		module := r.generateModule(moduleReplicaSet, pod)
		if err = r.Client.Create(ctx, module); err != nil {
			log.Log.Error(err, "Failed to create module", "moduleName", module.Name)
			return err
		}
	}
	log.Log.Info("finish scaleup module", "moduleReplicaSetName", moduleReplicaSet.Name)
	return nil
}

// scale down module
func (r *ModuleReplicaSetReconciler) scaledown(ctx context.Context, existedModuleList *moduledeploymentv1alpha1.ModuleList, moduleReplicaSet *moduledeploymentv1alpha1.ModuleReplicaSet) error {
	deltaReplicas := int(moduleReplicaSet.Spec.Replicas) - len(existedModuleList.Items)
	count := -deltaReplicas
	log.Log.Info("scale down replicas", "deltaReplicas", deltaReplicas)
	var err error
	for _, existedModule := range existedModuleList.Items {
		existedModule.Labels[label.DeleteModuleLabel] = "true"
		err = r.Client.Update(ctx, &existedModule)
		if err != nil {
			log.Log.Error(err, "Failed to delete module", "module", existedModule)
		}
		if count--; count == 0 {
			break
		}

	}
	return err
}

// compare and update module
func (r *ModuleReplicaSetReconciler) compareAndUpdateModule(ctx context.Context, existedModuleList *moduledeploymentv1alpha1.ModuleList, moduleReplicaSet *moduledeploymentv1alpha1.ModuleReplicaSet) error {
	desiredModule := moduleReplicaSet.Spec.Template.Spec.Module
	for _, existedModule := range existedModuleList.Items {

		needUpgradeModule := existedModule.Spec.Module.Name != desiredModule.Name || existedModule.Spec.Module.Version != desiredModule.Version || existedModule.Spec.Module.Url != desiredModule.Url
		needUninstallModule := existedModule.Spec.Module.Name != desiredModule.Name
		if needUpgradeModule {
			existedModule.Spec.Module.Name = desiredModule.Name
			existedModule.Spec.Module.Version = desiredModule.Version
			existedModule.Spec.Module.Url = desiredModule.Url
			err := r.Client.Update(ctx, &existedModule)
			if err != nil {
				log.Log.Error(err, "Failed to update module", "moduleName", existedModule.Name)
				return err
			}
		}
		if needUninstallModule {
			err := r.Client.Delete(ctx, &existedModule)
			if err != nil {
				log.Log.Error(err, "Failed to delete module", "moduleName", existedModule.Name)
				return err
			}
		}
	}
	return nil
}

// handle deleting moduleReplicaSet
func (r *ModuleReplicaSetReconciler) handleDeletingModuleReplicaSet(ctx context.Context, existedModuleList *moduledeploymentv1alpha1.ModuleList, moduleReplicaSet *moduledeploymentv1alpha1.ModuleReplicaSet) (ctrl.Result, error) {
	if len(existedModuleList.Items) == 0 {
		if utils.HasFinalizer(&moduleReplicaSet.ObjectMeta, finalizer.ModuleExistedFinalizer) {
			// all module is removed, remove module replicaset finalizer
			log.Log.Info("all modules are deleted, remove moduleReplicaSet finalizer", "moduleReplicaSetName", moduleReplicaSet.Name)
			utils.RemoveFinalizer(&moduleReplicaSet.ObjectMeta, finalizer.ModuleExistedFinalizer)
			err := r.Client.Update(ctx, moduleReplicaSet)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	} else {
		var err error
		for _, existedModule := range existedModuleList.Items {
			log.Log.Info("moduleReplicaSet is deleting, delete module", "moduleReplicaSetName", moduleReplicaSet.Name, "module", existedModule.Name)
			existedModule.Labels[label.DeleteModuleLabel] = "true"
			err = r.Client.Update(ctx, &existedModule)
		}
		if err != nil {
			log.Log.Error(err, "Failed to update uninstall module label")
			return ctrl.Result{}, err
		}

		// wait all module deleting
		log.Log.Info("moduleReplicaSet wait module deleting", "moduleReplicaSetName", moduleReplicaSet.Name, "existedModuleSize", len(existedModuleList.Items))
		requeueAfter := utils.GetNextReconcileTime(moduleReplicaSet.DeletionTimestamp.Time)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
}

// generate module
func (r *ModuleReplicaSetReconciler) generateModule(moduleReplicaSet *moduledeploymentv1alpha1.ModuleReplicaSet, pod corev1.Pod) *moduledeploymentv1alpha1.Module {

	moduleLabels := moduleReplicaSet.Labels
	moduleLabels[label.ModuleNameLabel] = moduleReplicaSet.Spec.Template.Spec.Module.Name
	moduleLabels[label.ModuleVersionLabel] = moduleReplicaSet.Spec.Template.Spec.Module.Version
	moduleLabels[label.BaseInstanceIpLabel] = pod.Status.PodIP
	moduleLabels[label.BaseInstanceNameLabel] = pod.Name
	moduleLabels[label.ModuleReplicasetLabel] = moduleReplicaSet.Name

	module := &moduledeploymentv1alpha1.Module{
		ObjectMeta: metav1.ObjectMeta{
			Annotations:  map[string]string{},
			Labels:       moduleLabels,
			GenerateName: fmt.Sprintf("%s-%s-", moduleReplicaSet.Name, moduleReplicaSet.Spec.Template.Spec.Module.Name),
			Namespace:    moduleReplicaSet.Namespace,
		},
		Spec: moduledeploymentv1alpha1.ModuleSpec{
			Selector: moduleReplicaSet.Spec.Selector,
			Module:   moduleReplicaSet.Spec.Template.Spec.Module,
		},
	}
	// OwnerReference to moduleReplicaSet and Pod
	owner := []metav1.OwnerReference{
		{
			APIVersion:         pod.APIVersion,
			Kind:               pod.Kind,
			UID:                pod.UID,
			Name:               pod.Name,
			BlockOwnerDeletion: pointer.Bool(true),
		},
	}
	module.SetOwnerReferences(owner)
	return module
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModuleReplicaSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&moduledeploymentv1alpha1.ModuleReplicaSet{}).
		Complete(r)
}
