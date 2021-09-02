/*
Copyright 2021 The Kubernetes Authors.

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
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeClientSet "k8s.io/client-go/kubernetes"
	volerr "k8s.io/cloud-provider/volume/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1alpha1"
	azVolumeClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/util"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// defaultMaxReplicaUpdateCount refers to the maximum number of creation or deletion of AzVolumeAttachment objects in a single ManageReplica call
	defaultMaxReplicaUpdateCount = 1
)

type AttachmentProvisioner interface {
	PublishVolume(ctx context.Context, volumeID string, nodeID string, volumeContext map[string]string) (map[string]string, error)
	UnpublishVolume(ctx context.Context, volumeID string, nodeID string) error
}

/*
Attach Detach controller is responsible for
	1. attaching volume to a specified node upon creation of AzVolumeAttachment CRI
	2. promoting AzVolumeAttachment to primary upon spec update
	3. detaching volume upon deletions marked with certain annotations
*/
type ReconcileAttachDetach struct {
	client                client.Client
	azVolumeClient        azVolumeClientSet.Interface
	kubeClient            kubeClientSet.Interface
	namespace             string
	attachmentProvisioner AttachmentProvisioner
	stateLock             *sync.Map
}

var _ reconcile.Reconciler = &ReconcileAttachDetach{}

var allowedTargetAttachmentStates = map[string][]string{
	string(v1alpha1.AttachmentPending): {string(v1alpha1.Attaching), string(v1alpha1.Detaching)},
	string(v1alpha1.Attaching):         {string(v1alpha1.Attached), string(v1alpha1.AttachmentFailed)},
	string(v1alpha1.Detaching):         {string(v1alpha1.Detached), string(v1alpha1.DetachmentFailed)},
	string(v1alpha1.Attached):          {string(v1alpha1.Detaching)},
	string(v1alpha1.AttachmentFailed):  {string(v1alpha1.Attaching), string(v1alpha1.Detaching)},
	string(v1alpha1.DetachmentFailed):  {string(v1alpha1.Detaching)},
}

func (r *ReconcileAttachDetach) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	azVolumeAttachment, err := azureutils.GetAzVolumeAttachment(ctx, r.client, r.azVolumeClient, request.Name, request.Namespace, true)
	// if object is not found, it means the object has been deleted. Log the deletion and do not requeue
	if errors.IsNotFound(err) {
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	// if underlying cloud operation already in process, skip until operation is completed
	if isOperationInProcess(azVolumeAttachment) {
		return reconcile.Result{}, err
	}

	// detachment request
	if deletionRequested(&azVolumeAttachment.ObjectMeta) {
		if azVolumeAttachment.Status.State == v1alpha1.AttachmentPending || azVolumeAttachment.Status.State == v1alpha1.Attached || azVolumeAttachment.Status.State == v1alpha1.AttachmentFailed || azVolumeAttachment.Status.State == v1alpha1.DetachmentFailed {
			if err := r.triggerDetach(ctx, azVolumeAttachment); err != nil {
				return reconcileReturnOnError(azVolumeAttachment, "detach", err)
			}
		}
		// attachment request
	} else if azVolumeAttachment.Status.Detail == nil {
		if azVolumeAttachment.Status.State == v1alpha1.AttachmentPending || azVolumeAttachment.Status.State == v1alpha1.AttachmentFailed {
			if err := r.triggerAttach(ctx, azVolumeAttachment); err != nil {
				return reconcileReturnOnError(azVolumeAttachment, "attach", err)
			}
		}
		// promotion request
	} else if azVolumeAttachment.Spec.RequestedRole != azVolumeAttachment.Status.Detail.Role {
		if err := r.promote(ctx, azVolumeAttachment); err != nil {
			return reconcileReturnOnError(azVolumeAttachment, "promote", err)
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileAttachDetach) triggerAttach(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment) error {
	// requeue if AzVolumeAttachment's state is being updated by a different worker
	if _, ok := r.stateLock.LoadOrStore(azVolumeAttachment.Name, nil); ok {
		return getOperationRequeueError("attach", azVolumeAttachment)
	}
	defer r.stateLock.Delete(azVolumeAttachment.Name)

	// initialize metadata and update status block
	updateFunc := func(obj interface{}) error {
		azv := obj.(*v1alpha1.AzVolumeAttachment)
		// Update state to attaching, Initialize finalizer and add label to the object
		azv = r.initializeMeta(ctx, azv)
		_, derr := r.updateState(ctx, azv, v1alpha1.Attaching)
		return derr
	}
	if err := azureutils.UpdateCRIWithRetry(ctx, r.azVolumeClient, azVolumeAttachment, updateFunc); err != nil {
		return err
	}

	klog.Infof("Attaching volume (%s) to node (%s)", azVolumeAttachment.Spec.VolumeID, azVolumeAttachment.Spec.NodeName)

	// TODO reassess if adding additional finalizer to AzVolume CRI is necessary
	// // add finalizer to the bound AzVolume CRI
	// conditionFunc = func() (bool, error) {
	// 	if err := r.addFinalizerToAzVolume(ctx, azVolumeAttachment.Spec.UnderlyingVolume); err != nil {
	// 		return false, nil
	// 	}
	// 	return true, nil
	// }
	// if err := wait.PollImmediate(updateAttemptInterval, updateTimeout, conditionFunc); err != nil {
	// 	return err
	// }

	// initiate goroutine to attach volume
	go func() {
		response, err := r.attachVolume(ctx, azVolumeAttachment.Spec.VolumeID, azVolumeAttachment.Spec.NodeName, azVolumeAttachment.Spec.VolumeContext)
		var updateFunc func(interface{}) error
		if err != nil {
			klog.Errorf("failed to attach volume %s to node %s: %v", azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName, err)

			updateFunc = func(obj interface{}) error {
				azv := obj.(*v1alpha1.AzVolumeAttachment)
				azv = r.updateError(ctx, azv, err)
				_, err = r.updateState(ctx, azv, v1alpha1.AttachmentFailed)
				return err
			}
		} else {
			klog.Infof("successfully attached volume (%s) to node (%s) and update status of AzVolumeAttachment (%s)", azVolumeAttachment.Spec.UnderlyingVolume, azVolumeAttachment.Spec.NodeName, azVolumeAttachment.Name)

			updateFunc = func(obj interface{}) error {
				azv := obj.(*v1alpha1.AzVolumeAttachment)
				azv = r.updateStatusDetail(ctx, azv, response)
				_, err = r.updateState(ctx, azv, v1alpha1.Attached)
				return err
			}
		}
		if derr := azureutils.UpdateCRIWithRetry(ctx, r.azVolumeClient, azVolumeAttachment, updateFunc); derr != nil {
			klog.Errorf("failed to update AzVolumeAttachment (%s) with attachVolume result (response: %v, error: %v)", azVolumeAttachment.Name, response, err)
		} else {
			klog.Infof("Successfully updated AzVolumeAttachment (%s) with attachVolume result (response: %v, error: %v)", azVolumeAttachment.Name, response, err)
		}
	}()

	return nil
}

func (r *ReconcileAttachDetach) triggerDetach(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment) error {
	// only detach if
	// 1) detachment request was made for underling volume attachment object
	// 2) volume attachment is marked for deletion or does not exist
	// 3) no annotation has been set (in this case, this is a replica that can safely be detached)
	detachmentRequested := true
	if azVolumeAttachment.Annotations != nil && metav1.HasAnnotation(azVolumeAttachment.ObjectMeta, azureutils.VolumeAttachmentExistsAnnotation) {
		vaName := azVolumeAttachment.Annotations[azureutils.VolumeAttachmentExistsAnnotation]
		var volumeAttachment storagev1.VolumeAttachment
		err := r.client.Get(ctx, types.NamespacedName{Name: vaName}, &volumeAttachment)
		if err != nil && !errors.IsNotFound(err) {
			klog.Errorf("failed to get volumeAttachment (%s): %v", vaName, err)
			return err
		}
		detachmentRequested = errors.IsNotFound(err) || volumeAttachment.DeletionTimestamp != nil
	}
	detachmentRequested = detachmentRequested || metav1.HasAnnotation(azVolumeAttachment.ObjectMeta, azureutils.VolumeDetachRequestAnnotation)

	if detachmentRequested {
		if _, ok := r.stateLock.LoadOrStore(azVolumeAttachment.Name, nil); ok {
			return getOperationRequeueError("detach", azVolumeAttachment)
		}
		defer r.stateLock.Delete(azVolumeAttachment.Name)

		updateFunc := func(obj interface{}) error {
			azv := obj.(*v1alpha1.AzVolumeAttachment)
			// Update state to detaching
			_, derr := r.updateState(ctx, azv, v1alpha1.Detaching)
			return derr
		}
		if err := azureutils.UpdateCRIWithRetry(ctx, r.azVolumeClient, azVolumeAttachment, updateFunc); err != nil {
			return err
		}

		klog.Infof("Detaching volume (%s) from node (%s)", azVolumeAttachment.Spec.VolumeID, azVolumeAttachment.Spec.NodeName)

		go func() {
			var updateFunc func(obj interface{}) error
			if err := r.detachVolume(ctx, azVolumeAttachment.Spec.VolumeID, azVolumeAttachment.Spec.NodeName); err != nil {
				updateFunc = func(obj interface{}) error {
					azv := obj.(*v1alpha1.AzVolumeAttachment)
					azv = r.updateError(ctx, azv, err)
					_, err = r.updateState(ctx, azv, v1alpha1.DetachmentFailed)
					return err
				}
			} else {
				updateFunc = func(obj interface{}) error {
					azv := obj.(*v1alpha1.AzVolumeAttachment)
					azv = r.deleteFinalizer(ctx, azv)
					_, err = r.updateState(ctx, azv, v1alpha1.Detached)
					return err
				}
			}
			if derr := azureutils.UpdateCRIWithRetry(ctx, r.azVolumeClient, azVolumeAttachment, updateFunc); derr != nil {
				klog.Errorf("failed to update AzVolumeAttachment (%s) with detachVolume result (error: %v)", azVolumeAttachment.Name, derr)
			} else {
				klog.Infof("Successfully updated AzVolumeAttachment (%s) with detachVolume result (error: %v)", azVolumeAttachment.Name, derr)
			}
		}()
	} else {
		updateFunc := func(obj interface{}) error {
			azv := obj.(*v1alpha1.AzVolumeAttachment)
			// delete finalizer
			_ = r.deleteFinalizer(ctx, azv)
			return nil
		}
		if err := azureutils.UpdateCRIWithRetry(ctx, r.azVolumeClient, azVolumeAttachment, updateFunc); err != nil {
			return err
		}
	}
	return nil
}

func (r *ReconcileAttachDetach) promote(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment) error {
	// initialize metadata and update status block
	updateFunc := func(obj interface{}) error {
		azv := obj.(*v1alpha1.AzVolumeAttachment)
		_ = r.updateRole(ctx, azv, v1alpha1.PrimaryRole)
		return nil
	}
	return azureutils.UpdateCRIWithRetry(ctx, r.azVolumeClient, azVolumeAttachment, updateFunc)
}

func (r *ReconcileAttachDetach) initializeMeta(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment) *v1alpha1.AzVolumeAttachment {
	if azVolumeAttachment == nil {
		return nil
	}

	// if the required metadata already exists return
	if finalizerExists(azVolumeAttachment.Finalizers, azureutils.AzVolumeAttachmentFinalizer) &&
		labelExists(azVolumeAttachment.Labels, azureutils.NodeNameLabel) &&
		labelExists(azVolumeAttachment.Labels, azureutils.VolumeNameLabel) &&
		labelExists(azVolumeAttachment.Labels, azureutils.RoleLabel) {
		return azVolumeAttachment
	}

	// add finalizer
	if azVolumeAttachment.Finalizers == nil {
		azVolumeAttachment.Finalizers = []string{}
	}

	if !finalizerExists(azVolumeAttachment.Finalizers, azureutils.AzVolumeAttachmentFinalizer) {
		azVolumeAttachment.Finalizers = append(azVolumeAttachment.Finalizers, azureutils.AzVolumeAttachmentFinalizer)
	}

	// add label
	if azVolumeAttachment.Labels == nil {
		azVolumeAttachment.Labels = make(map[string]string)
	}
	azVolumeAttachment.Labels[azureutils.NodeNameLabel] = azVolumeAttachment.Spec.NodeName
	azVolumeAttachment.Labels[azureutils.VolumeNameLabel] = azVolumeAttachment.Spec.UnderlyingVolume
	azVolumeAttachment.Labels[azureutils.RoleLabel] = string(azVolumeAttachment.Spec.RequestedRole)

	return azVolumeAttachment
}

func (r *ReconcileAttachDetach) deleteFinalizer(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment) *v1alpha1.AzVolumeAttachment {
	if azVolumeAttachment == nil {
		return nil
	}

	if azVolumeAttachment.ObjectMeta.Finalizers == nil {
		return azVolumeAttachment
	}

	finalizers := []string{}
	for _, finalizer := range azVolumeAttachment.ObjectMeta.Finalizers {
		if finalizer == azureutils.AzVolumeAttachmentFinalizer {
			continue
		}
		finalizers = append(finalizers, finalizer)
	}
	azVolumeAttachment.ObjectMeta.Finalizers = finalizers
	return azVolumeAttachment
}

// TODO: reassess if adding additional finalizer to AzVolume CRI is necessary
// func (r *ReconcileAttachDetach) addFinalizerToAzVolume(ctx context.Context, volumeName string) error {
// 	var azVolume *v1alpha1.AzVolume
// 	azVolume, err := azureutils.GetAzVolume(ctx, r.client, r.azVolumeClient, volumeName, r.namespace, true)
// 	if err != nil {
// 		klog.Errorf("failed to get AzVolume (%s): %v", volumeName, err)
// 		return err
// 	}

// 	updated := azVolume.DeepCopy()

// 	if updated.Finalizers == nil {
// 		updated.Finalizers = []string{}
// 	}

// 	if finalizerExists(updated.Finalizers, azureutils.AzVolumeAttachmentFinalizer) {
// 		return nil
// 	}

// 	updated.Finalizers = append(updated.Finalizers, azureutils.AzVolumeAttachmentFinalizer)
// 	if err := r.client.Update(ctx, updated, &client.UpdateOptions{}); err != nil {
// 		klog.Errorf("failed to add finalizer (%s) to AzVolume(%s): %v", azureutils.AzVolumeAttachmentFinalizer, updated.Name, err)
// 		return err
// 	}
// 	klog.Infof("successfully added finalizer (%s) to AzVolume (%s)", azureutils.AzVolumeAttachmentFinalizer, updated.Name)
// 	return nil
// }
//
// func (r *ReconcileAttachDetach) deleteFinalizerFromAzVolume(ctx context.Context, volumeName string) error {
// 	var azVolume *v1alpha1.AzVolume
// 	azVolume, err := azureutils.GetAzVolume(ctx, r.client, r.azVolumeClient, volumeName, r.namespace, true)
// 	if err != nil {
// 		if errors.IsNotFound(err) {
// 			return nil
// 		}
// 		klog.Errorf("failed to get AzVolume (%s): %v", volumeName, err)
// 		return err
// 	}

// 	updated := azVolume.DeepCopy()
// 	updatedFinalizers := []string{}

// 	for _, finalizer := range updated.Finalizers {
// 		if finalizer == azureutils.AzVolumeAttachmentFinalizer {
// 			continue
// 		}
// 		updatedFinalizers = append(updatedFinalizers, finalizer)
// 	}
// 	updated.Finalizers = updatedFinalizers

// 	if err := r.client.Update(ctx, updated, &client.UpdateOptions{}); err != nil {
// 		klog.Errorf("failed to delete finalizer (%s) from AzVolume(%s): %v", azureutils.AzVolumeAttachmentFinalizer, updated.Name, err)
// 		return err
// 	}
// 	klog.Infof("successfully deleted finalizer (%s) from AzVolume (%s)", azureutils.AzVolumeAttachmentFinalizer, updated.Name)
// 	return nil
// }

func (r *ReconcileAttachDetach) updateRole(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment, role v1alpha1.Role) *v1alpha1.AzVolumeAttachment {
	if azVolumeAttachment == nil {
		return nil
	}

	if azVolumeAttachment.Labels == nil {
		azVolumeAttachment.Labels = map[string]string{}
	}
	azVolumeAttachment.Labels[azureutils.RoleLabel] = string(role)

	if azVolumeAttachment.Status.Detail == nil {
		return azVolumeAttachment
	}
	azVolumeAttachment.Status.Detail.Role = role

	return azVolumeAttachment
}

func (r *ReconcileAttachDetach) updateStatusDetail(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment, status map[string]string) *v1alpha1.AzVolumeAttachment {
	if azVolumeAttachment == nil {
		return nil
	}

	if azVolumeAttachment.Status.Detail == nil {
		azVolumeAttachment.Status.Detail = &v1alpha1.AzVolumeAttachmentStatusDetail{}
	}
	azVolumeAttachment.Status.Detail.Role = azVolumeAttachment.Spec.RequestedRole
	azVolumeAttachment.Status.Detail.PublishContext = status

	return azVolumeAttachment
}

func (r *ReconcileAttachDetach) updateError(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment, err error) *v1alpha1.AzVolumeAttachment {
	if azVolumeAttachment == nil {
		return nil
	}

	if err != nil {
		azVolumeAttachmentError := &v1alpha1.AzError{
			ErrorCode:    util.GetStringValueForErrorCode(status.Code(err)),
			ErrorMessage: err.Error(),
		}
		if derr, ok := err.(*volerr.DanglingAttachError); ok {
			azVolumeAttachmentError.ErrorCode = util.DanglingAttachErrorCode
			azVolumeAttachmentError.CurrentNode = derr.CurrentNode
			azVolumeAttachmentError.DevicePath = derr.DevicePath
		}

		azVolumeAttachment.Status.Error = azVolumeAttachmentError
	}

	return azVolumeAttachment
}

func (r *ReconcileAttachDetach) updateState(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment, state v1alpha1.AzVolumeAttachmentAttachmentState) (*v1alpha1.AzVolumeAttachment, error) {
	var err error
	if azVolumeAttachment == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "function `updateState` requires non-nil AzVolumeAttachment object.")
	}
	expectedStates := allowedTargetAttachmentStates[string(azVolumeAttachment.Status.State)]
	if !containsString(string(state), expectedStates) {
		err = status.Error(codes.FailedPrecondition, formatUpdateStateError("azVolume", string(azVolumeAttachment.Status.State), string(state), expectedStates...))
	}
	if err == nil {
		azVolumeAttachment.Status.State = state
	}
	return azVolumeAttachment, err
}

func (r *ReconcileAttachDetach) update(ctx context.Context, azVolumeAttachment *v1alpha1.AzVolumeAttachment) error {
	if azVolumeAttachment == nil {
		return status.Error(codes.FailedPrecondition, "expecting non-nil azVolumeAttachment object to update")
	}
	return r.client.Update(ctx, azVolumeAttachment, &client.UpdateOptions{})
}

func (r *ReconcileAttachDetach) attachVolume(ctx context.Context, volumeID, node string, volumeContext map[string]string) (map[string]string, error) {
	return r.attachmentProvisioner.PublishVolume(ctx, volumeID, node, volumeContext)
}

func (r *ReconcileAttachDetach) detachVolume(ctx context.Context, volumeID, node string) error {
	return r.attachmentProvisioner.UnpublishVolume(ctx, volumeID, node)
}

func (r *ReconcileAttachDetach) Recover(ctx context.Context) error {
	klog.Info("Recovering AzVolumeAttachment CRIs...")
	// try to recover states
	var syncedVolumeAttachments, volumesToSync map[string]bool
	for i := 0; i < maxRetry; i++ {
		var retry bool
		var err error

		retry, syncedVolumeAttachments, volumesToSync, err = r.syncAll(ctx, syncedVolumeAttachments, volumesToSync)
		if err != nil {
			klog.Warningf("failed to complete initial AzVolumeAttachment sync: %v", err)
		}
		if !retry {
			break
		}
	}

	return nil
}

// ManageAttachmentsForVolume will be running on a separate channel
func (r *ReconcileAttachDetach) syncAll(ctx context.Context, syncedVolumeAttachments map[string]bool, volumesToSync map[string]bool) (bool, map[string]bool, map[string]bool, error) {
	// Get all volumeAttachments
	volumeAttachments, err := r.kubeClient.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return true, syncedVolumeAttachments, volumesToSync, err
	}

	if syncedVolumeAttachments == nil {
		syncedVolumeAttachments = map[string]bool{}
	}
	if volumesToSync == nil {
		volumesToSync = map[string]bool{}
	}

	// Loop through volumeAttachments and create Primary AzVolumeAttachments in correspondence
	for _, volumeAttachment := range volumeAttachments.Items {
		// skip if sync has been completed volumeAttachment
		if syncedVolumeAttachments[volumeAttachment.Name] {
			continue
		}
		if volumeAttachment.Spec.Attacher == azureutils.DriverName {
			volumeName := volumeAttachment.Spec.Source.PersistentVolumeName
			if volumeName == nil {
				continue
			}
			// get PV and retrieve diskName
			pv, err := r.kubeClient.CoreV1().PersistentVolumes().Get(ctx, *volumeName, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("failed to get PV (%s): %v", *volumeName, err)
				return true, syncedVolumeAttachments, volumesToSync, err
			}

			if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != azureutils.DriverName {
				continue
			}
			volumesToSync[pv.Spec.CSI.VolumeHandle] = true

			diskName, err := azureutils.GetDiskNameFromAzureManagedDiskURI(pv.Spec.CSI.VolumeHandle)
			if err != nil {
				klog.Warningf("failed to extract disk name from volumehandle (%s): %v", pv.Spec.CSI.VolumeHandle, err)
				delete(volumesToSync, pv.Spec.CSI.VolumeHandle)
				continue
			}
			nodeName := volumeAttachment.Spec.NodeName
			azVolumeAttachmentName := azureutils.GetAzVolumeAttachmentName(diskName, nodeName)

			// check if the CRI exists already
			azVolumeAttachment, err := azureutils.GetAzVolumeAttachment(ctx, r.client, r.azVolumeClient, azVolumeAttachmentName, r.namespace, false)
			klog.Infof("Recovering AzVolumeAttachment(%s)", azVolumeAttachmentName)
			// if CRI already exists, append finalizer to it
			if err == nil {
				azVolumeAttachment = r.initializeMeta(ctx, azVolumeAttachment)
				err = r.update(ctx, azVolumeAttachment)
				if err != nil {
					klog.Errorf("failed to add finalizer to AzVolumeAttachment (%s): %v", azVolumeAttachmentName, err)
					return true, syncedVolumeAttachments, volumesToSync, err
				}
				// if not found, create one
			} else if errors.IsNotFound(err) {
				azVolumeAttachment := v1alpha1.AzVolumeAttachment{
					ObjectMeta: metav1.ObjectMeta{
						Name: azVolumeAttachmentName,
						Labels: map[string]string{
							azureutils.NodeNameLabel:   nodeName,
							azureutils.VolumeNameLabel: *volumeName,
						},
						Finalizers: []string{azureutils.AzVolumeAttachmentFinalizer},
					},
					Spec: v1alpha1.AzVolumeAttachmentSpec{
						UnderlyingVolume: *volumeName,
						VolumeID:         pv.Spec.CSI.VolumeHandle,
						NodeName:         nodeName,
						RequestedRole:    v1alpha1.PrimaryRole,
						VolumeContext:    map[string]string{},
					},
					Status: v1alpha1.AzVolumeAttachmentStatus{
						State: azureutils.GetAzVolumeAttachmentState(volumeAttachment.Status),
					},
				}
				if azVolumeAttachment.Status.State == v1alpha1.Attached {
					azVolumeAttachment.Status.Detail = &v1alpha1.AzVolumeAttachmentStatusDetail{
						Role: v1alpha1.PrimaryRole,
					}
				}
				_, err := r.azVolumeClient.DiskV1alpha1().AzVolumeAttachments(r.namespace).Create(ctx, &azVolumeAttachment, metav1.CreateOptions{})
				if err != nil {
					klog.Errorf("failed to create AzVolumeAttachment (%s) for volume (%s) and node (%s): %v", azVolumeAttachmentName, *volumeName, nodeName, err)
					return true, syncedVolumeAttachments, volumesToSync, err
				}
			} else {
				klog.Errorf("failed to get AzVolumeAttachment (%s): %v", azVolumeAttachmentName, err)
				return true, syncedVolumeAttachments, volumesToSync, err
			}

			syncedVolumeAttachments[volumeAttachment.Name] = true
		}
	}
	return false, syncedVolumeAttachments, volumesToSync, nil
}

func NewAttachDetachController(mgr manager.Manager, azVolumeClient azVolumeClientSet.Interface, kubeClient kubeClientSet.Interface, namespace string, attachmentProvisioner AttachmentProvisioner) (*ReconcileAttachDetach, error) {
	reconciler := ReconcileAttachDetach{
		client:                mgr.GetClient(),
		azVolumeClient:        azVolumeClient,
		kubeClient:            kubeClient,
		namespace:             namespace,
		attachmentProvisioner: attachmentProvisioner,
		stateLock:             &sync.Map{},
	}

	c, err := controller.New("azvolumeattachment-controller", mgr, controller.Options{
		MaxConcurrentReconciles: 10,
		Reconciler:              &reconciler,
		Log:                     mgr.GetLogger().WithValues("controller", "azvolumeattachment"),
	})

	if err != nil {
		klog.Errorf("failed to create a new azvolumeattachment controller: %v", err)
		return nil, err
	}

	// Watch for CRUD events on azVolumeAttachment objects
	err = c.Watch(&source.Kind{Type: &v1alpha1.AzVolumeAttachment{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		klog.Errorf("failed to initialize watch for azvolumeattachment object: %v", err)
		return nil, err
	}

	klog.V(2).Info("AzVolumeAttachment Controller successfully initialized.")
	return &reconciler, nil
}
