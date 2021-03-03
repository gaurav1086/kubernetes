/*
Copyright 2017 The Kubernetes Authors.

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

package scheduling

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/etcd3"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	coreinformers "k8s.io/client-go/informers/core/v1"
	storageinformers "k8s.io/client-go/informers/storage/v1"
	storageinformersv1beta1 "k8s.io/client-go/informers/storage/v1beta1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	storagelistersv1beta1 "k8s.io/client-go/listers/storage/v1beta1"
	storagehelpers "k8s.io/component-helpers/storage/volume"
	csitrans "k8s.io/csi-translation-lib"
	csiplugins "k8s.io/csi-translation-lib/plugins"
	"k8s.io/klog/v2"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	pvutil "k8s.io/kubernetes/pkg/controller/volume/persistentvolume/util"
	"k8s.io/kubernetes/pkg/controller/volume/scheduling/metrics"
	"k8s.io/kubernetes/pkg/features"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

// ConflictReason is used for the special strings which explain why
// volume binding is impossible for a node.
type ConflictReason string

// ConflictReasons contains all reasons that explain why volume binding is impossible for a node.
type ConflictReasons []ConflictReason

func (reasons ConflictReasons) Len() int           { return len(reasons) }
func (reasons ConflictReasons) Less(i, j int) bool { return reasons[i] < reasons[j] }
func (reasons ConflictReasons) Swap(i, j int)      { reasons[i], reasons[j] = reasons[j], reasons[i] }

const (
	// ErrReasonBindConflict is used for VolumeBindingNoMatch predicate error.
	ErrReasonBindConflict ConflictReason = "node(s) didn't find available persistent volumes to bind"
	// ErrReasonNodeConflict is used for VolumeNodeAffinityConflict predicate error.
	ErrReasonNodeConflict ConflictReason = "node(s) had volume node affinity conflict"
	// ErrReasonNotEnoughSpace is used when a pod cannot start on a node because not enough storage space is available.
	ErrReasonNotEnoughSpace = "node(s) did not have enough free storage"
	// ErrReasonPVNotExist is used when a PVC can't find the bound persistent volumes"
	ErrReasonPVNotExist = "pvc(s) bound to non-existent pv(s)"
)

// BindingInfo holds a binding between PV and PVC.
type BindingInfo struct {
	// PVC that needs to be bound
	pvc *v1.PersistentVolumeClaim

	// Proposed PV to bind to this PVC
	pv *v1.PersistentVolume
}

// StorageClassName returns the name of the storage class.
func (b *BindingInfo) StorageClassName() string {
	return b.pv.Spec.StorageClassName
}

// StorageResource represents storage resource.
type StorageResource struct {
	Requested int64
	Capacity  int64
}

// StorageResource returns storage resource.
func (b *BindingInfo) StorageResource() *StorageResource {
	// both fields are mandatory
	requestedQty := b.pvc.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	capacityQty := b.pv.Spec.Capacity[v1.ResourceName(v1.ResourceStorage)]
	return &StorageResource{
		Requested: requestedQty.Value(),
		Capacity:  capacityQty.Value(),
	}
}

// PodVolumes holds pod's volumes information used in volume scheduling.
type PodVolumes struct {
	// StaticBindings are binding decisions for PVCs which can be bound to
	// pre-provisioned static PVs.
	StaticBindings []*BindingInfo
	// DynamicProvisions are PVCs that require dynamic provisioning
	DynamicProvisions []*v1.PersistentVolumeClaim
}

// InTreeToCSITranslator contains methods required to check migratable status
// and perform translations from InTree PV's to CSI
type InTreeToCSITranslator interface {
	IsPVMigratable(pv *v1.PersistentVolume) bool
	GetInTreePluginNameFromSpec(pv *v1.PersistentVolume, vol *v1.Volume) (string, error)
	TranslateInTreePVToCSI(pv *v1.PersistentVolume) (*v1.PersistentVolume, error)
}

// SchedulerVolumeBinder is used by the scheduler VolumeBinding plugin to
// handle PVC/PV binding and dynamic provisioning. The binding decisions are
// integrated into the pod scheduling workflow so that the PV NodeAffinity is
// also considered along with the pod's other scheduling requirements.
//
// This integrates into the existing scheduler workflow as follows:
// 1. The scheduler takes a Pod off the scheduler queue and processes it serially:
//    a. Invokes all pre-filter plugins for the pod. GetPodVolumes() is invoked
//    here, pod volume information will be saved in current scheduling cycle state for later use.
//    b. Invokes all filter plugins, parallelized across nodes.  FindPodVolumes() is invoked here.
//    c. Invokes all score plugins.  Future/TBD
//    d. Selects the best node for the Pod.
//    e. Invokes all reserve plugins. AssumePodVolumes() is invoked here.
//       i.  If PVC binding is required, cache in-memory only:
//           * For manual binding: update PV objects for prebinding to the corresponding PVCs.
//           * For dynamic provisioning: update PVC object with a selected node from c)
//           * For the pod, which PVCs and PVs need API updates.
//       ii. Afterwards, the main scheduler caches the Pod->Node binding in the scheduler's pod cache,
//           This is handled in the scheduler and not here.
//    f. Asynchronously bind volumes and pod in a separate goroutine
//        i.  BindPodVolumes() is called first in PreBind phase. It makes all the necessary API updates and waits for
//            PV controller to fully bind and provision the PVCs. If binding fails, the Pod is sent
//            back through the scheduler.
//        ii. After BindPodVolumes() is complete, then the scheduler does the final Pod->Node binding.
// 2. Once all the assume operations are done in e), the scheduler processes the next Pod in the scheduler queue
//    while the actual binding operation occurs in the background.
type SchedulerVolumeBinder interface {
	// GetPodVolumes returns a pod's PVCs separated into bound, unbound with delayed binding (including provisioning)
	// and unbound with immediate binding (including prebound)
	GetPodVolumes(pod *v1.Pod) (boundClaims, unboundClaimsDelayBinding, unboundClaimsImmediate []*v1.PersistentVolumeClaim, err error)

	// FindPodVolumes checks if all of a Pod's PVCs can be satisfied by the
	// node and returns pod's volumes information.
	//
	// If a PVC is bound, it checks if the PV's NodeAffinity matches the Node.
	// Otherwise, it tries to find an available PV to bind to the PVC.
	//
	// It returns an error when something went wrong or a list of reasons why the node is
	// (currently) not usable for the pod.
	//
	// If the CSIStorageCapacity feature is enabled, then it also checks for sufficient storage
	// for volumes that still need to be created.
	//
	// This function is called by the scheduler VolumeBinding plugin and can be called in parallel
	FindPodVolumes(pod *v1.Pod, boundClaims, claimsToBind []*v1.PersistentVolumeClaim, node *v1.Node) (podVolumes *PodVolumes, reasons ConflictReasons, err error)

	// AssumePodVolumes will:
	// 1. Take the PV matches for unbound PVCs and update the PV cache assuming
	// that the PV is prebound to the PVC.
	// 2. Take the PVCs that need provisioning and update the PVC cache with related
	// annotations set.
	//
	// It returns true if all volumes are fully bound
	//
	// This function is called serially.
	AssumePodVolumes(assumedPod *v1.Pod, nodeName string, podVolumes *PodVolumes) (allFullyBound bool, err error)

	// RevertAssumedPodVolumes will revert assumed PV and PVC cache.
	RevertAssumedPodVolumes(podVolumes *PodVolumes)

	// BindPodVolumes will:
	// 1. Initiate the volume binding by making the API call to prebind the PV
	// to its matching PVC.
	// 2. Trigger the volume provisioning by making the API call to set related
	// annotations on the PVC
	// 3. Wait for PVCs to be completely bound by the PV controller
	//
	// This function can be called in parallel.
	BindPodVolumes(assumedPod *v1.Pod, podVolumes *PodVolumes) error
}

type volumeBinder struct {
	kubeClient clientset.Interface

	classLister   storagelisters.StorageClassLister
	podLister     corelisters.PodLister
	nodeLister    corelisters.NodeLister
	csiNodeLister storagelisters.CSINodeLister

	pvcCache PVCAssumeCache
	pvCache  PVAssumeCache

	// Amount of time to wait for the bind operation to succeed
	bindTimeout time.Duration

	translator InTreeToCSITranslator

	capacityCheckEnabled     bool
	csiDriverLister          storagelisters.CSIDriverLister
	csiStorageCapacityLister storagelistersv1beta1.CSIStorageCapacityLister
}

// CapacityCheck contains additional parameters for NewVolumeBinder that
// are only needed when checking volume sizes against available storage
// capacity is desired.
type CapacityCheck struct {
	CSIDriverInformer          storageinformers.CSIDriverInformer
	CSIStorageCapacityInformer storageinformersv1beta1.CSIStorageCapacityInformer
}

// NewVolumeBinder sets up all the caches needed for the scheduler to make volume binding decisions.
//
// capacityCheck determines whether storage capacity is checked (CSIStorageCapacity feature).
func NewVolumeBinder(
	kubeClient clientset.Interface,
	podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer,
	csiNodeInformer storageinformers.CSINodeInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	pvInformer coreinformers.PersistentVolumeInformer,
	storageClassInformer storageinformers.StorageClassInformer,
	capacityCheck *CapacityCheck,
	bindTimeout time.Duration) SchedulerVolumeBinder {
	b := &volumeBinder{
		kubeClient:    kubeClient,
		podLister:     podInformer.Lister(),
		classLister:   storageClassInformer.Lister(),
		nodeLister:    nodeInformer.Lister(),
		csiNodeLister: csiNodeInformer.Lister(),
		pvcCache:      NewPVCAssumeCache(pvcInformer.Informer()),
		pvCache:       NewPVAssumeCache(pvInformer.Informer()),
		bindTimeout:   bindTimeout,
		translator:    csitrans.New(),
	}

	if capacityCheck != nil {
		b.capacityCheckEnabled = true
		b.csiDriverLister = capacityCheck.CSIDriverInformer.Lister()
		b.csiStorageCapacityLister = capacityCheck.CSIStorageCapacityInformer.Lister()
	}

	return b
}

// FindPodVolumes finds the matching PVs for PVCs and nodes to provision PVs
// for the given pod and node. If the node does not fit, confilict reasons are
// returned.
func (b *volumeBinder) FindPodVolumes(pod *v1.Pod, boundClaims, claimsToBind []*v1.PersistentVolumeClaim, node *v1.Node) (podVolumes *PodVolumes, reasons ConflictReasons, err error) {
	podVolumes = &PodVolumes{}
	podName := getPodName(pod)

	// Warning: Below log needs high verbosity as it can be printed several times (#60933).
	klog.V(5).Infof("FindPodVolumes for pod %q, node %q", podName, node.Name)

	// Initialize to true for pods that don't have volumes. These
	// booleans get translated into reason strings when the function
	// returns without an error.
	unboundVolumesSatisfied := true
	boundVolumesSatisfied := true
	sufficientStorage := true
	boundPVsFound := true
	defer func() {
		if err != nil {
			return
		}
		if !boundVolumesSatisfied {
			reasons = append(reasons, ErrReasonNodeConflict)
		}
		if !unboundVolumesSatisfied {
			reasons = append(reasons, ErrReasonBindConflict)
		}
		if !sufficientStorage {
			reasons = append(reasons, ErrReasonNotEnoughSpace)
		}
		if !boundPVsFound {
			reasons = append(reasons, ErrReasonPVNotExist)
		}
	}()

	start := time.Now()
	defer func() {
		metrics.VolumeSchedulingStageLatency.WithLabelValues("predicate").Observe(time.Since(start).Seconds())
		if err != nil {
			metrics.VolumeSchedulingStageFailed.WithLabelValues("predicate").Inc()
		}
	}()

	var (
		staticBindings    []*BindingInfo
		dynamicProvisions []*v1.PersistentVolumeClaim
	)
	defer func() {
		// Although we do not distinguish nil from empty in this function, for
		// easier testing, we normalize empty to nil.
		if len(staticBindings) == 0 {
			staticBindings = nil
		}
		if len(dynamicProvisions) == 0 {
			dynamicProvisions = nil
		}
		podVolumes.StaticBindings = staticBindings
		podVolumes.DynamicProvisions = dynamicProvisions
	}()

	// Check PV node affinity on bound volumes
	if len(boundClaims) > 0 {
		boundVolumesSatisfied, boundPVsFound, err = b.checkBoundClaims(boundClaims, node, podName)
		if err != nil {
			return
		}
	}

	// Find matching volumes and node for unbound claims
	if len(claimsToBind) > 0 {
		var (
			claimsToFindMatching []*v1.PersistentVolumeClaim
			claimsToProvision    []*v1.PersistentVolumeClaim
		)

		// Filter out claims to provision
		for _, claim := range claimsToBind {
			if selectedNode, ok := claim.Annotations[pvutil.AnnSelectedNode]; ok {
				if selectedNode != node.Name {
					// Fast path, skip unmatched node.
					unboundVolumesSatisfied = false
					return
				}
				claimsToProvision = append(claimsToProvision, claim)
			} else {
				claimsToFindMatching = append(claimsToFindMatching, claim)
			}
		}

		// Find matching volumes
		if len(claimsToFindMatching) > 0 {
			var unboundClaims []*v1.PersistentVolumeClaim
			unboundVolumesSatisfied, staticBindings, unboundClaims, err = b.findMatchingVolumes(pod, claimsToFindMatching, node)
			if err != nil {
				return
			}
			claimsToProvision = append(claimsToProvision, unboundClaims...)
		}

		// Check for claims to provision. This is the first time where we potentially
		// find out that storage is not sufficient for the node.
		if len(claimsToProvision) > 0 {
			unboundVolumesSatisfied, sufficientStorage, dynamicProvisions, err = b.checkVolumeProvisions(pod, claimsToProvision, node)
			if err != nil {
				return
			}
		}
	}

	return
}

// AssumePodVolumes will take the matching PVs and PVCs to provision in pod's
// volume information for the chosen node, and:
// 1. Update the pvCache with the new prebound PV.
// 2. Update the pvcCache with the new PVCs with annotations set
// 3. Update PodVolumes again with cached API updates for PVs and PVCs.
func (b *volumeBinder) AssumePodVolumes(assumedPod *v1.Pod, nodeName string, podVolumes *PodVolumes) (allFullyBound bool, err error) {
	podName := getPodName(assumedPod)

	klog.V(4).Infof("AssumePodVolumes for pod %q, node %q", podName, nodeName)
	start := time.Now()
	defer func() {
		metrics.VolumeSchedulingStageLatency.WithLabelValues("assume").Observe(time.Since(start).Seconds())
		if err != nil {
			metrics.VolumeSchedulingStageFailed.WithLabelValues("assume").Inc()
		}
	}()

	if allBound := b.arePodVolumesBound(assumedPod); allBound {
		klog.V(4).Infof("AssumePodVolumes for pod %q, node %q: all PVCs bound and nothing to do", podName, nodeName)
		return true, nil
	}

	// Assume PV
	newBindings := []*BindingInfo{}
	for _, binding := range podVolumes.StaticBindings {
		newPV, dirty, err := pvutil.GetBindVolumeToClaim(binding.pv, binding.pvc)
		klog.V(5).Infof("AssumePodVolumes: GetBindVolumeToClaim for pod %q, PV %q, PVC %q.  newPV %p, dirty %v, err: %v",
			podName,
			binding.pv.Name,
			binding.pvc.Name,
			newPV,
			dirty,
			err)
		if err != nil {
			b.revertAssumedPVs(newBindings)
			return false, err
		}
		// TODO: can we assume everytime?
		if dirty {
			err = b.pvCache.Assume(newPV)
			if err != nil {
				b.revertAssumedPVs(newBindings)
				return false, err
			}
		}
		newBindings = append(newBindings, &BindingInfo{pv: newPV, pvc: binding.pvc})
	}

	// Assume PVCs
	newProvisionedPVCs := []*v1.PersistentVolumeClaim{}
	for _, claim := range podVolumes.DynamicProvisions {
		// The claims from method args can be pointing to watcher cache. We must not
		// modify these, therefore create a copy.
		claimClone := claim.DeepCopy()
		metav1.SetMetaDataAnnotation(&claimClone.ObjectMeta, pvutil.AnnSelectedNode, nodeName)
		err = b.pvcCache.Assume(claimClone)
		if err != nil {
			b.revertAssumedPVs(newBindings)
			b.revertAssumedPVCs(newProvisionedPVCs)
			return
		}

		newProvisionedPVCs = append(newProvisionedPVCs, claimClone)
	}

	podVolumes.StaticBindings = newBindings
	podVolumes.DynamicProvisions = newProvisionedPVCs
	return
}

// RevertAssumedPodVolumes will revert assumed PV and PVC cache.
func (b *volumeBinder) RevertAssumedPodVolumes(podVolumes *PodVolumes) {
	b.revertAssumedPVs(podVolumes.StaticBindings)
	b.revertAssumedPVCs(podVolumes.DynamicProvisions)
}

// BindPodVolumes gets the cached bindings and PVCs to provision in pod's volumes information,
// makes the API update for those PVs/PVCs, and waits for the PVCs to be completely bound
// by the PV controller.
func (b *volumeBinder) BindPodVolumes(assumedPod *v1.Pod, podVolumes *PodVolumes) (err error) {
	podName := getPodName(assumedPod)
	klog.V(4).Infof("BindPodVolumes for pod %q, node %q", podName, assumedPod.Spec.NodeName)

	start := time.Now()
	defer func() {
		metrics.VolumeSchedulingStageLatency.WithLabelValues("bind").Observe(time.Since(start).Seconds())
		if err != nil {
			metrics.VolumeSchedulingStageFailed.WithLabelValues("bind").Inc()
		}
	}()

	bindings := podVolumes.StaticBindings
	claimsToProvision := podVolumes.DynamicProvisions

	// Start API operations
	err = b.bindAPIUpdate(podName, bindings, claimsToProvision)
	if err != nil {
		return err
	}

	err = wait.Poll(time.Second, b.bindTimeout, func() (bool, error) {
		b, err := b.checkBindings(assumedPod, bindings, claimsToProvision)
		return b, err
	})
	if err != nil {
		return fmt.Errorf("binding volumes: %w", err)
	}
	return nil
}

func getPodName(pod *v1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

func getPVCName(pvc *v1.PersistentVolumeClaim) string {
	return pvc.Namespace + "/" + pvc.Name
}

// bindAPIUpdate makes the API update for those PVs/PVCs.
func (b *volumeBinder) bindAPIUpdate(podName string, bindings []*BindingInfo, claimsToProvision []*v1.PersistentVolumeClaim) error {
	if bindings == nil {
		return fmt.Errorf("failed to get cached bindings for pod %q", podName)
	}
	if claimsToProvision == nil {
		return fmt.Errorf("failed to get cached claims to provision for pod %q", podName)
	}

	lastProcessedBinding := 0
	lastProcessedProvisioning := 0
	defer func() {
		// only revert assumed cached updates for volumes we haven't successfully bound
		if lastProcessedBinding < len(bindings) {
			b.revertAssumedPVs(bindings[lastProcessedBinding:])
		}
		// only revert assumed cached updates for claims we haven't updated,
		if lastProcessedProvisioning < len(claimsToProvision) {
			b.revertAssumedPVCs(claimsToProvision[lastProcessedProvisioning:])
		}
	}()

	var (
		binding *BindingInfo
		i       int
		claim   *v1.PersistentVolumeClaim
	)

	// Do the actual prebinding. Let the PV controller take care of the rest
	// There is no API rollback if the actual binding fails
	for _, binding = range bindings {
		klog.V(5).Infof("bindAPIUpdate: Pod %q, binding PV %q to PVC %q", podName, binding.pv.Name, binding.pvc.Name)
		// TODO: does it hurt if we make an api call and nothing needs to be updated?
		claimKey := getPVCName(binding.pvc)
		klog.V(2).Infof("claim %q bound to volume %q", claimKey, binding.pv.Name)
		newPV, err := b.kubeClient.CoreV1().PersistentVolumes().Update(context.TODO(), binding.pv, metav1.UpdateOptions{})
		if err != nil {
			klog.V(4).Infof("updating PersistentVolume[%s]: binding to %q failed: %v", binding.pv.Name, claimKey, err)
			return err
		}
		klog.V(4).Infof("updating PersistentVolume[%s]: bound to %q", binding.pv.Name, claimKey)
		// Save updated object from apiserver for later checking.
		binding.pv = newPV
		lastProcessedBinding++
	}

	// Update claims objects to trigger volume provisioning. Let the PV controller take care of the rest
	// PV controller is expected to signal back by removing related annotations if actual provisioning fails
	for i, claim = range claimsToProvision {
		klog.V(5).Infof("bindAPIUpdate: Pod %q, PVC %q", podName, getPVCName(claim))
		newClaim, err := b.kubeClient.CoreV1().PersistentVolumeClaims(claim.Namespace).Update(context.TODO(), claim, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		// Save updated object from apiserver for later checking.
		claimsToProvision[i] = newClaim
		lastProcessedProvisioning++
	}

	return nil
}

var (
	versioner = etcd3.APIObjectVersioner{}
)

// checkBindings runs through all the PVCs in the Pod and checks:
// * if the PVC is fully bound
// * if there are any conditions that require binding to fail and be retried
//
// It returns true when all of the Pod's PVCs are fully bound, and error if
// binding (and scheduling) needs to be retried
// Note that it checks on API objects not PV/PVC cache, this is because
// PV/PVC cache can be assumed again in main scheduler loop, we must check
// latest state in API server which are shared with PV controller and
// provisioners
func (b *volumeBinder) checkBindings(pod *v1.Pod, bindings []*BindingInfo, claimsToProvision []*v1.PersistentVolumeClaim) (bool, error) {
	podName := getPodName(pod)
	if bindings == nil {
		return false, fmt.Errorf("failed to get cached bindings for pod %q", podName)
	}
	if claimsToProvision == nil {
		return false, fmt.Errorf("failed to get cached claims to provision for pod %q", podName)
	}

	node, err := b.nodeLister.Get(pod.Spec.NodeName)
	if err != nil {
		return false, fmt.Errorf("failed to get node %q: %w", pod.Spec.NodeName, err)
	}

	csiNode, err := b.csiNodeLister.Get(node.Name)
	if err != nil {
		// TODO: return the error once CSINode is created by default
		klog.V(4).Infof("Could not get a CSINode object for the node %q: %v", node.Name, err)
	}

	// Check for any conditions that might require scheduling retry

	// When pod is deleted, binding operation should be cancelled. There is no
	// need to check PV/PVC bindings any more.
	_, err = b.podLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("pod does not exist any more: %w", err)
		}
		klog.Errorf("failed to get pod %s/%s from the lister: %v", pod.Namespace, pod.Name, err)
	}

	for _, binding := range bindings {
		pv, err := b.pvCache.GetAPIPV(binding.pv.Name)
		if err != nil {
			return false, fmt.Errorf("failed to check binding: %w", err)
		}

		pvc, err := b.pvcCache.GetAPIPVC(getPVCName(binding.pvc))
		if err != nil {
			return false, fmt.Errorf("failed to check binding: %w", err)
		}

		// Because we updated PV in apiserver, skip if API object is older
		// and wait for new API object propagated from apiserver.
		if versioner.CompareResourceVersion(binding.pv, pv) > 0 {
			return false, nil
		}

		pv, err = b.tryTranslatePVToCSI(pv, csiNode)
		if err != nil {
			return false, fmt.Errorf("failed to translate pv to csi: %w", err)
		}

		// Check PV's node affinity (the node might not have the proper label)
		if err := volumeutil.CheckNodeAffinity(pv, node.Labels); err != nil {
			return false, fmt.Errorf("pv %q node affinity doesn't match node %q: %w", pv.Name, node.Name, err)
		}

		// Check if pv.ClaimRef got dropped by unbindVolume()
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID == "" {
			return false, fmt.Errorf("ClaimRef got reset for pv %q", pv.Name)
		}

		// Check if pvc is fully bound
		if !b.isPVCFullyBound(pvc) {
			return false, nil
		}
	}

	for _, claim := range claimsToProvision {
		pvc, err := b.pvcCache.GetAPIPVC(getPVCName(claim))
		if err != nil {
			return false, fmt.Errorf("failed to check provisioning pvc: %w", err)
		}

		// Because we updated PVC in apiserver, skip if API object is older
		// and wait for new API object propagated from apiserver.
		if versioner.CompareResourceVersion(claim, pvc) > 0 {
			return false, nil
		}

		// Check if selectedNode annotation is still set
		if pvc.Annotations == nil {
			return false, fmt.Errorf("selectedNode annotation reset for PVC %q", pvc.Name)
		}
		selectedNode := pvc.Annotations[pvutil.AnnSelectedNode]
		if selectedNode != pod.Spec.NodeName {
			// If provisioner fails to provision a volume, selectedNode
			// annotation will be removed to signal back to the scheduler to
			// retry.
			return false, fmt.Errorf("provisioning failed for PVC %q", pvc.Name)
		}

		// If the PVC is bound to a PV, check its node affinity
		if pvc.Spec.VolumeName != "" {
			pv, err := b.pvCache.GetAPIPV(pvc.Spec.VolumeName)
			if err != nil {
				if _, ok := err.(*errNotFound); ok {
					// We tolerate NotFound error here, because PV is possibly
					// not found because of API delay, we can check next time.
					// And if PV does not exist because it's deleted, PVC will
					// be unbound eventually.
					return false, nil
				}
				return false, fmt.Errorf("failed to get pv %q from cache: %w", pvc.Spec.VolumeName, err)
			}

			pv, err = b.tryTranslatePVToCSI(pv, csiNode)
			if err != nil {
				return false, err
			}

			if err := volumeutil.CheckNodeAffinity(pv, node.Labels); err != nil {
				return false, fmt.Errorf("pv %q node affinity doesn't match node %q: %w", pv.Name, node.Name, err)
			}
		}

		// Check if pvc is fully bound
		if !b.isPVCFullyBound(pvc) {
			return false, nil
		}
	}

	// All pvs and pvcs that we operated on are bound
	klog.V(4).Infof("All PVCs for pod %q are bound", podName)
	return true, nil
}

func (b *volumeBinder) isVolumeBound(pod *v1.Pod, vol *v1.Volume) (bound bool, pvc *v1.PersistentVolumeClaim, err error) {
	pvcName := ""
	ephemeral := false
	switch {
	case vol.PersistentVolumeClaim != nil:
		pvcName = vol.PersistentVolumeClaim.ClaimName
	case vol.Ephemeral != nil:
		if !utilfeature.DefaultFeatureGate.Enabled(features.GenericEphemeralVolume) {
			return false, nil, fmt.Errorf(
				"volume %s is a generic ephemeral volume, but that feature is disabled in kube-scheduler",
				vol.Name,
			)
		}
		// Generic ephemeral inline volumes also use a PVC,
		// just with a computed name, and...
		pvcName = pod.Name + "-" + vol.Name
		ephemeral = true
	default:
		return true, nil, nil
	}

	bound, pvc, err = b.isPVCBound(pod.Namespace, pvcName)
	// ... the PVC must be owned by the pod.
	if ephemeral && err == nil && pvc != nil && !metav1.IsControlledBy(pvc, pod) {
		return false, nil, fmt.Errorf("PVC %s/%s is not owned by pod", pod.Namespace, pvcName)
	}
	return
}

func (b *volumeBinder) isPVCBound(namespace, pvcName string) (bool, *v1.PersistentVolumeClaim, error) {
	claim := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
		},
	}
	pvcKey := getPVCName(claim)
	pvc, err := b.pvcCache.GetPVC(pvcKey)
	if err != nil || pvc == nil {
		return false, nil, fmt.Errorf("error getting PVC %q: %v", pvcKey, err)
	}

	fullyBound := b.isPVCFullyBound(pvc)
	if fullyBound {
		klog.V(5).Infof("PVC %q is fully bound to PV %q", pvcKey, pvc.Spec.VolumeName)
	} else {
		if pvc.Spec.VolumeName != "" {
			klog.V(5).Infof("PVC %q is not fully bound to PV %q", pvcKey, pvc.Spec.VolumeName)
		} else {
			klog.V(5).Infof("PVC %q is not bound", pvcKey)
		}
	}
	return fullyBound, pvc, nil
}

func (b *volumeBinder) isPVCFullyBound(pvc *v1.PersistentVolumeClaim) bool {
	return pvc.Spec.VolumeName != "" && metav1.HasAnnotation(pvc.ObjectMeta, pvutil.AnnBindCompleted)
}

// arePodVolumesBound returns true if all volumes are fully bound
func (b *volumeBinder) arePodVolumesBound(pod *v1.Pod) bool {
	for _, vol := range pod.Spec.Volumes {
		if isBound, _, _ := b.isVolumeBound(pod, &vol); !isBound {
			// Pod has at least one PVC that needs binding
			return false
		}
	}
	return true
}

// GetPodVolumes returns a pod's PVCs separated into bound, unbound with delayed binding (including provisioning)
// and unbound with immediate binding (including prebound)
func (b *volumeBinder) GetPodVolumes(pod *v1.Pod) (boundClaims []*v1.PersistentVolumeClaim, unboundClaimsDelayBinding []*v1.PersistentVolumeClaim, unboundClaimsImmediate []*v1.PersistentVolumeClaim, err error) {
	boundClaims = []*v1.PersistentVolumeClaim{}
	unboundClaimsImmediate = []*v1.PersistentVolumeClaim{}
	unboundClaimsDelayBinding = []*v1.PersistentVolumeClaim{}

	for _, vol := range pod.Spec.Volumes {
		volumeBound, pvc, err := b.isVolumeBound(pod, &vol)
		if err != nil {
			return nil, nil, nil, err
		}
		if pvc == nil {
			continue
		}
		if volumeBound {
			boundClaims = append(boundClaims, pvc)
		} else {
			delayBindingMode, err := pvutil.IsDelayBindingMode(pvc, b.classLister)
			if err != nil {
				return nil, nil, nil, err
			}
			// Prebound PVCs are treated as unbound immediate binding
			if delayBindingMode && pvc.Spec.VolumeName == "" {
				// Scheduler path
				unboundClaimsDelayBinding = append(unboundClaimsDelayBinding, pvc)
			} else {
				// !delayBindingMode || pvc.Spec.VolumeName != ""
				// Immediate binding should have already been bound
				unboundClaimsImmediate = append(unboundClaimsImmediate, pvc)
			}
		}
	}
	return boundClaims, unboundClaimsDelayBinding, unboundClaimsImmediate, nil
}

func (b *volumeBinder) checkBoundClaims(claims []*v1.PersistentVolumeClaim, node *v1.Node, podName string) (bool, bool, error) {
	csiNode, err := b.csiNodeLister.Get(node.Name)
	if err != nil {
		// TODO: return the error once CSINode is created by default
		klog.V(4).Infof("Could not get a CSINode object for the node %q: %v", node.Name, err)
	}

	for _, pvc := range claims {
		pvName := pvc.Spec.VolumeName
		pv, err := b.pvCache.GetPV(pvName)
		if err != nil {
			if _, ok := err.(*errNotFound); ok {
				err = nil
			}
			return true, false, err
		}

		pv, err = b.tryTranslatePVToCSI(pv, csiNode)
		if err != nil {
			return false, true, err
		}

		err = volumeutil.CheckNodeAffinity(pv, node.Labels)
		if err != nil {
			klog.V(4).Infof("PersistentVolume %q, Node %q mismatch for Pod %q: %v", pvName, node.Name, podName, err)
			return false, true, nil
		}
		klog.V(5).Infof("PersistentVolume %q, Node %q matches for Pod %q", pvName, node.Name, podName)
	}

	klog.V(4).Infof("All bound volumes for Pod %q match with Node %q", podName, node.Name)
	return true, true, nil
}

// findMatchingVolumes tries to find matching volumes for given claims,
// and return unbound claims for further provision.
func (b *volumeBinder) findMatchingVolumes(pod *v1.Pod, claimsToBind []*v1.PersistentVolumeClaim, node *v1.Node) (foundMatches bool, bindings []*BindingInfo, unboundClaims []*v1.PersistentVolumeClaim, err error) {
	podName := getPodName(pod)
	// Sort all the claims by increasing size request to get the smallest fits
	sort.Sort(byPVCSize(claimsToBind))

	chosenPVs := map[string]*v1.PersistentVolume{}

	foundMatches = true

	for _, pvc := range claimsToBind {
		// Get storage class name from each PVC
		storageClassName := storagehelpers.GetPersistentVolumeClaimClass(pvc)
		allPVs := b.pvCache.ListPVs(storageClassName)
		pvcName := getPVCName(pvc)

		// Find a matching PV
		pv, err := pvutil.FindMatchingVolume(pvc, allPVs, node, chosenPVs, true)
		if err != nil {
			return false, nil, nil, err
		}
		if pv == nil {
			klog.V(4).Infof("No matching volumes for Pod %q, PVC %q on node %q", podName, pvcName, node.Name)
			unboundClaims = append(unboundClaims, pvc)
			foundMatches = false
			continue
		}

		// matching PV needs to be excluded so we don't select it again
		chosenPVs[pv.Name] = pv
		bindings = append(bindings, &BindingInfo{pv: pv, pvc: pvc})
		klog.V(5).Infof("Found matching PV %q for PVC %q on node %q for pod %q", pv.Name, pvcName, node.Name, podName)
	}

	if foundMatches {
		klog.V(4).Infof("Found matching volumes for pod %q on node %q", podName, node.Name)
	}

	return
}

// checkVolumeProvisions checks given unbound claims (the claims have gone through func
// findMatchingVolumes, and do not have matching volumes for binding), and return true
// if all of the claims are eligible for dynamic provision.
func (b *volumeBinder) checkVolumeProvisions(pod *v1.Pod, claimsToProvision []*v1.PersistentVolumeClaim, node *v1.Node) (provisionSatisfied, sufficientStorage bool, dynamicProvisions []*v1.PersistentVolumeClaim, err error) {
	podName := getPodName(pod)
	dynamicProvisions = []*v1.PersistentVolumeClaim{}

	// We return early with provisionedClaims == nil if a check
	// fails or we encounter an error.
	for _, claim := range claimsToProvision {
		pvcName := getPVCName(claim)
		className := storagehelpers.GetPersistentVolumeClaimClass(claim)
		if className == "" {
			return false, false, nil, fmt.Errorf("no class for claim %q", pvcName)
		}

		class, err := b.classLister.Get(className)
		if err != nil {
			return false, false, nil, fmt.Errorf("failed to find storage class %q", className)
		}
		provisioner := class.Provisioner
		if provisioner == "" || provisioner == pvutil.NotSupportedProvisioner {
			klog.V(4).Infof("storage class %q of claim %q does not support dynamic provisioning", className, pvcName)
			return false, true, nil, nil
		}

		// Check if the node can satisfy the topology requirement in the class
		if !v1helper.MatchTopologySelectorTerms(class.AllowedTopologies, labels.Set(node.Labels)) {
			klog.V(4).Infof("Node %q cannot satisfy provisioning topology requirements of claim %q", node.Name, pvcName)
			return false, true, nil, nil
		}

		// Check storage capacity.
		sufficient, err := b.hasEnoughCapacity(provisioner, claim, class, node)
		if err != nil {
			return false, false, nil, err
		}
		if !sufficient {
			// hasEnoughCapacity logs an explanation.
			return true, false, nil, nil
		}

		dynamicProvisions = append(dynamicProvisions, claim)

	}
	klog.V(4).Infof("Provisioning for %d claims of pod %q that has no matching volumes on node %q ...", len(claimsToProvision), podName, node.Name)

	return true, true, dynamicProvisions, nil
}

func (b *volumeBinder) revertAssumedPVs(bindings []*BindingInfo) {
	for _, BindingInfo := range bindings {
		b.pvCache.Restore(BindingInfo.pv.Name)
	}
}

func (b *volumeBinder) revertAssumedPVCs(claims []*v1.PersistentVolumeClaim) {
	for _, claim := range claims {
		b.pvcCache.Restore(getPVCName(claim))
	}
}

// hasEnoughCapacity checks whether the provisioner has enough capacity left for a new volume of the given size
// that is available from the node.
func (b *volumeBinder) hasEnoughCapacity(provisioner string, claim *v1.PersistentVolumeClaim, storageClass *storagev1.StorageClass, node *v1.Node) (bool, error) {
	// This is an optional feature. If disabled, we assume that
	// there is enough storage.
	if !b.capacityCheckEnabled {
		return true, nil
	}

	quantity, ok := claim.Spec.Resources.Requests[v1.ResourceStorage]
	if !ok {
		// No capacity to check for.
		return true, nil
	}

	// Only enabled for CSI drivers which opt into it.
	driver, err := b.csiDriverLister.Get(provisioner)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Either the provisioner is not a CSI driver or the driver does not
			// opt into storage capacity scheduling. Either way, skip
			// capacity checking.
			return true, nil
		}
		return false, err
	}
	if driver.Spec.StorageCapacity == nil || !*driver.Spec.StorageCapacity {
		return true, nil
	}

	// Look for a matching CSIStorageCapacity object(s).
	// TODO (for beta): benchmark this and potentially introduce some kind of lookup structure (https://github.com/kubernetes/enhancements/issues/1698#issuecomment-654356718).
	capacities, err := b.csiStorageCapacityLister.List(labels.Everything())
	if err != nil {
		return false, err
	}

	sizeInBytes := quantity.Value()
	for _, capacity := range capacities {
		if capacity.StorageClassName == storageClass.Name &&
			capacity.Capacity != nil &&
			capacity.Capacity.Value() >= sizeInBytes &&
			b.nodeHasAccess(node, capacity) {
			// Enough capacity found.
			return true, nil
		}
	}

	// TODO (?): this doesn't give any information about which pools where considered and why
	// they had to be rejected. Log that above? But that might be a lot of log output...
	klog.V(4).Infof("Node %q has no accessible CSIStorageCapacity with enough capacity for PVC %s/%s of size %d and storage class %q",
		node.Name, claim.Namespace, claim.Name, sizeInBytes, storageClass.Name)
	return false, nil
}

func (b *volumeBinder) nodeHasAccess(node *v1.Node, capacity *storagev1beta1.CSIStorageCapacity) bool {
	if capacity.NodeTopology == nil {
		// Unavailable
		return false
	}
	// Only matching by label is supported.
	selector, err := metav1.LabelSelectorAsSelector(capacity.NodeTopology)
	if err != nil {
		// This should never happen because NodeTopology must be valid.
		klog.Errorf("unexpected error converting %+v to a label selector: %v", capacity.NodeTopology, err)
		return false
	}
	return selector.Matches(labels.Set(node.Labels))
}

type byPVCSize []*v1.PersistentVolumeClaim

func (a byPVCSize) Len() int {
	return len(a)
}

func (a byPVCSize) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a byPVCSize) Less(i, j int) bool {
	iSize := a[i].Spec.Resources.Requests[v1.ResourceStorage]
	jSize := a[j].Spec.Resources.Requests[v1.ResourceStorage]
	// return true if iSize is less than jSize
	return iSize.Cmp(jSize) == -1
}

// isCSIMigrationOnForPlugin checks if CSI migrartion is enabled for a given plugin.
func isCSIMigrationOnForPlugin(pluginName string) bool {
	switch pluginName {
	case csiplugins.AWSEBSInTreePluginName:
		return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationAWS)
	case csiplugins.GCEPDInTreePluginName:
		return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationGCE)
	case csiplugins.AzureDiskInTreePluginName:
		return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationAzureDisk)
	case csiplugins.CinderInTreePluginName:
		return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationOpenStack)
	}
	return false
}

// isPluginMigratedToCSIOnNode checks if an in-tree plugin has been migrated to a CSI driver on the node.
func isPluginMigratedToCSIOnNode(pluginName string, csiNode *storagev1.CSINode) bool {
	if csiNode == nil {
		return false
	}

	csiNodeAnn := csiNode.GetAnnotations()
	if csiNodeAnn == nil {
		return false
	}

	var mpaSet sets.String
	mpa := csiNodeAnn[v1.MigratedPluginsAnnotationKey]
	if len(mpa) == 0 {
		mpaSet = sets.NewString()
	} else {
		tok := strings.Split(mpa, ",")
		mpaSet = sets.NewString(tok...)
	}

	return mpaSet.Has(pluginName)
}

// tryTranslatePVToCSI will translate the in-tree PV to CSI if it meets the criteria. If not, it returns the unmodified in-tree PV.
func (b *volumeBinder) tryTranslatePVToCSI(pv *v1.PersistentVolume, csiNode *storagev1.CSINode) (*v1.PersistentVolume, error) {
	if !b.translator.IsPVMigratable(pv) {
		return pv, nil
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.CSIMigration) {
		return pv, nil
	}

	pluginName, err := b.translator.GetInTreePluginNameFromSpec(pv, nil)
	if err != nil {
		return nil, fmt.Errorf("could not get plugin name from pv: %v", err)
	}

	if !isCSIMigrationOnForPlugin(pluginName) {
		return pv, nil
	}

	if !isPluginMigratedToCSIOnNode(pluginName, csiNode) {
		return pv, nil
	}

	transPV, err := b.translator.TranslateInTreePVToCSI(pv)
	if err != nil {
		return nil, fmt.Errorf("could not translate pv: %v", err)
	}

	return transPV, nil
}
