/*
Copyright 2022 The Kubernetes Authors.

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

package driver

import (
	"context"
	"fmt"
	"math/rand"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	cloud "sigs.k8s.io/gcp-filestore-csi-driver/pkg/cloud_provider"
	"sigs.k8s.io/gcp-filestore-csi-driver/pkg/cloud_provider/file"
	"sigs.k8s.io/gcp-filestore-csi-driver/pkg/util"
)

// A workflow is defined as a sequence of steps to safely (by checking the storage class cache) initiate instance or share operations.
type Workflow struct {
	instance *file.MultishareInstance
	share    *file.Share
	opType   util.OperationType
	opName   string
}

// MultishareOpsManager manages storage class cache, manages the lifecycle of all instance and share operations.
type MultishareOpsManager struct {
	sync.Mutex // Lock to perform thread safe operations on the cache.
	cache      *util.StorageClassInfoCache
	cloud      *cloud.Cloud
}

func NewMultishareOpsManager(cloud *cloud.Cloud) *MultishareOpsManager {
	return &MultishareOpsManager{
		cache: util.NewStorageClassInfoCache(),
		cloud: cloud,
	}
}

func (m *MultishareOpsManager) Run() {
	// TODO: Start periodic cache hydration
	// TODO: Start periodic instance inspection for delete and shrink
}

// setupEligibleInstanceAndStartWorkflow returns a workflow object (to indicate an instance or share level workflow is started), or a share object (if existing share already found), or error.
func (m *MultishareOpsManager) setupEligibleInstanceAndStartWorkflow(ctx context.Context, req *csi.CreateVolumeRequest, instance *file.MultishareInstance) (*Workflow, *file.Share, error) {
	m.Lock()
	defer m.Unlock()

	// Check ShareCreateMap if a share create is already in progress.
	shareName := util.ConvertVolToShareName(req.Name)
	instanceScPrefix, err := getInstanceSCPrefix(req)
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Check if there is already a share create op in progress.
	lastShareCreateOpInfo, opStatus, err := m.checkAndUpdateShareCreateOp(ctx, instanceScPrefix, shareName)
	if err != nil {
		return nil, nil, err
	}

	if lastShareCreateOpInfo != nil {
		switch opStatus {
		case util.StatusRunning:
			return nil, nil, status.Errorf(codes.Aborted, "Share create operation %q in progress", lastShareCreateOpInfo.OpName)
		case util.StatusFailed:
			return nil, nil, status.Errorf(codes.Internal, "Share create operation %q failed", lastShareCreateOpInfo.OpName)
		case util.StatusSuccess:
			share, err := m.validateShareExists(ctx, lastShareCreateOpInfo.InstanceHandle, shareName)
			if err != nil {
				return nil, nil, err
			}
			return nil, share, nil
		default:
			return nil, nil, status.Errorf(codes.Internal, "unknown op status %d for op %v", opStatus, lastShareCreateOpInfo.OpName)
		}
	}

	// Check if share already part of an existing instance.
	share, err := m.checkInstanceListForShare(ctx, instanceScPrefix, shareName)
	if err != nil {
		return nil, nil, err
	}

	if share != nil {
		return nil, share, nil
	}

	// No share or running share create op fouund. Proceed to eligible instance check.
	eligible, numIneligible, err := m.runEligibleInstanceCheck(ctx, instanceScPrefix)
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if len(eligible) > 0 {
		// pick a random eligible instance
		index := rand.Intn(len(eligible))
		share, err := generateNewShare(shareName, eligible[index], req)
		if err != nil {
			return nil, nil, status.Error(codes.Internal, err.Error())
		}

		needExpand, targetBytes, err := m.instanceNeedsExpand(ctx, share)
		if err != nil {
			return nil, nil, status.Error(codes.Internal, err.Error())
		}

		if needExpand {
			eligible[index].CapacityBytes = targetBytes
			w, err := m.startInstanceWorkflow(ctx, instanceScPrefix, &Workflow{instance: eligible[index], opType: util.InstanceExpand})
			return w, nil, err
		}

		w, err := m.startShareCreateWorkflow(ctx, instanceScPrefix, share)
		return w, nil, err
	}

	if numIneligible > 0 {
		// some instances not ready yet. wait for more instances to be ready.
		return nil, nil, status.Errorf(codes.Aborted, " %d non-ready instances detected. No ready instance found", numIneligible)
	}

	w, err := m.startInstanceWorkflow(ctx, instanceScPrefix, &Workflow{instance: instance, opType: util.InstanceCreate})
	return w, nil, err
}

// checkAndUpdateShareCreateOp checks the share create op map, and evaluates the running status of the ops. If ops are detected as complete, they are removed as part of the check.
func (m *MultishareOpsManager) checkAndUpdateShareCreateOp(ctx context.Context, instanceScPrefix, shareName string) (*util.ShareCreateOpInfo, util.OperationStatus, error) {
	opInfo := m.cache.GetShareCreateOp(instanceScPrefix, shareName)
	if opInfo == nil {
		return nil, util.StatusUnknown, nil
	}

	op, err := m.cloud.File.GetOp(ctx, opInfo.OpName)
	if err != nil && !file.IsNotFoundErr(err) {
		return opInfo, util.StatusUnknown, status.Errorf(codes.Internal, "Failed to get operation %q", opInfo.OpName)
	}

	done, err := m.cloud.File.IsOpDone(op)
	if err != nil {
		// op completed with error.
		// clear cache and return retry error to the caller.
		m.cache.DeleteShareCreateOp(instanceScPrefix, shareName, opInfo.OpName)
		return opInfo, util.StatusFailed, nil
	}

	if !done {
		return opInfo, util.StatusRunning, nil
	}

	// Clear cache.
	m.cache.DeleteShareCreateOp(instanceScPrefix, shareName, opInfo.OpName)
	return opInfo, util.StatusSuccess, nil
}

// checkAndUpdateShareOp checks the share ops map, and evaluates the running status of the ops. If ops are detected as complete, they are removed as part of the check.
func (m *MultishareOpsManager) checkAndUpdateShareOp(ctx context.Context, instanceScPrefix string, share *file.Share) (*util.OpInfo, util.OperationStatus, error) {
	shareKey := util.CreateShareKey(share.Parent.Project, share.Parent.Location, share.Parent.Name, share.Name)
	opInfo := m.cache.GetShareOp(instanceScPrefix, shareKey)
	if opInfo == nil {
		return nil, util.StatusUnknown, nil
	}

	op, err := m.cloud.File.GetOp(ctx, opInfo.Name)
	if err != nil && !file.IsNotFoundErr(err) {
		return opInfo, util.StatusUnknown, status.Errorf(codes.Internal, "Failed to get operation %q", opInfo.Name)
	}

	done, err := m.cloud.File.IsOpDone(op)
	if err != nil {
		// op completed with error.
		// clear cache and return retry error to the caller.
		m.cache.DeleteShareOp(instanceScPrefix, shareKey, opInfo.Name)
		return opInfo, util.StatusFailed, nil
	}

	if !done {
		return opInfo, util.StatusRunning, nil
	}

	// Clear cache.
	m.cache.DeleteShareOp(instanceScPrefix, shareKey, opInfo.Name)
	return opInfo, util.StatusSuccess, nil
}

func (m *MultishareOpsManager) startShareCreateWorkflowSafe(ctx context.Context, instanceSCPrefix string, share *file.Share) (*Workflow, error) {
	m.Lock()
	defer m.Unlock()
	return m.startShareCreateWorkflow(ctx, instanceSCPrefix, share)
}

func (m *MultishareOpsManager) startShareCreateWorkflow(ctx context.Context, instanceSCPrefix string, share *file.Share) (*Workflow, error) {
	return m.startShareWorkflow(ctx, instanceSCPrefix, &Workflow{
		share:  share,
		opType: util.ShareCreate,
	})
}

func (m *MultishareOpsManager) startInstanceWorkflow(ctx context.Context, instanceSCPrefix string, w *Workflow) (*Workflow, error) {
	// This function has 3 distinct steps:
	// 1. verify no instance ops running for the given instance.
	// 2. verify no running share ops for the shares hosted on the given instance.
	// 3. Start the instance op.
	if w.instance == nil {
		return nil, status.Errorf(codes.Internal, "instance not found in workflow object")
	}

	instanceReady, err := m.verifyNoRunningInstanceOps(ctx, instanceSCPrefix, w.instance)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "Instance %q check error: %v", w.instance.Name, err)
	}
	if !instanceReady {
		return nil, status.Errorf(codes.Aborted, "Instance %q not ready", w.instance.Name)
	}

	// Verify no ongoing share ops for the instance. Also clear finised ops from cache, while doing verification.
	instanceReady, err = m.verifyNoRunningShareOpsForInstance(ctx, instanceSCPrefix, w.instance)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to check share ops for Instance %q, error: %v", w.instance.Name, err)
	}
	if !instanceReady {
		return nil, status.Errorf(codes.Aborted, "Instance %q not ready", w.instance.Name)
	}

	switch w.opType {
	case util.InstanceCreate:
		op, err := m.cloud.File.StartCreateMultishareInstanceOp(ctx, w.instance)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}
		w.opName = op.Name
	case util.InstanceExpand:
		op, err := m.cloud.File.StartResizeMultishareInstanceOp(ctx, w.instance)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}
		w.opName = op.Name
	case util.InstanceDelete:
		op, err := m.cloud.File.StartDeleteMultishareInstanceOp(ctx, w.instance)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}
		w.opName = op.Name
	default:
		return nil, status.Errorf(codes.Internal, "for instance workflow, unknown op type %v", w.opType)
	}

	m.cache.AddInstanceOp(instanceSCPrefix, util.CreateInstanceKey(w.instance.Project, w.instance.Location, w.instance.Name), util.OpInfo{Name: w.opName, Type: w.opType})
	return w, nil
}

func (m *MultishareOpsManager) startShareWorkflow(ctx context.Context, instanceSCPrefix string, w *Workflow) (*Workflow, error) {
	// This function has 3 distinct steps:
	// 1. verify no instance ops running for the instance hosting the given share.
	// 2. verify no running ops for the given share.
	// 3. Start the share op.
	if w.share == nil {
		return nil, status.Errorf(codes.Internal, "share not found in workflow object")
	}

	if w.share.Parent == nil {
		return nil, status.Errorf(codes.Internal, "share parent not found in workflow object")
	}

	// verify instance is ready.
	instanceReady, err := m.verifyNoRunningInstanceOps(ctx, instanceSCPrefix, w.share.Parent)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "Instance %q check error: %v", w.share.Parent.Name, err)
	}
	if !instanceReady {
		return nil, status.Errorf(codes.Aborted, "Instance %q not ready", w.share.Parent.Name)
	}

	// Verify share is ready.
	shareReady, err := m.verifyNoRunningShareOp(ctx, instanceSCPrefix, w.share)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "Share %q check error: %v", w.share.Name, err)
	}
	if !shareReady {
		return nil, status.Errorf(codes.Aborted, "Share %q not ready", w.share.Name)
	}

	switch w.opType {
	case util.ShareCreate:
		// validate no entry in cache for the share
		item := m.cache.GetShareCreateOp(instanceSCPrefix, w.share.Name)
		if item != nil {
			return nil, status.Errorf(codes.Aborted, "Share %q not ready, found op %q", w.share.Name, item.OpName)
		}

		op, err := m.cloud.File.StartCreateShareOp(ctx, w.share)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}

		// Start op and update cache
		instanceHandle, err := file.GetMultishareInstanceHandle(w.share.Parent)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}
		m.cache.AddShareCreateOp(instanceSCPrefix, w.share.Name, util.ShareCreateOpInfo{
			InstanceHandle: instanceHandle,
			OpName:         op.Name,
		})
		w.opName = op.Name
	case util.ShareExpand:
		// validate no entry in cache for the share
		item := m.cache.GetShareOp(instanceSCPrefix, util.CreateShareKey(w.share.Parent.Project, w.share.Parent.Location, w.share.Parent.Name, w.share.Name))
		if item != nil {
			return nil, status.Errorf(codes.Aborted, "Share %q not ready, found op %q", w.share.Name, item.Name)
		}

		// Start op and update cache
		op, err := m.cloud.File.StartResizeShareOp(ctx, w.share)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}
		m.cache.AddShareOp(instanceSCPrefix, util.CreateShareKey(w.share.Parent.Project, w.share.Parent.Location, w.share.Parent.Name, w.share.Name), util.OpInfo{Name: op.Name, Type: w.opType})
		w.opName = op.Name
	case util.ShareDelete:
		// validate no entry in cache for the share
		item := m.cache.GetShareOp(instanceSCPrefix, util.CreateShareKey(w.share.Parent.Project, w.share.Parent.Location, w.share.Parent.Name, w.share.Name))
		if item != nil {
			return nil, status.Errorf(codes.Aborted, "Share %q not ready, found op %q", w.share.Name, item.Name)
		}

		// Start op and update cache
		op, err := m.cloud.File.StartDeleteShareOp(ctx, w.share)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}
		m.cache.AddShareOp(instanceSCPrefix, util.CreateShareKey(w.share.Parent.Project, w.share.Parent.Location, w.share.Parent.Name, w.share.Name), util.OpInfo{Name: op.Name, Type: w.opType})
		w.opName = op.Name
	default:
		return nil, status.Errorf(codes.Internal, "for share workflow, unknown op type %v", w.opType)
	}
	return w, nil
}

func (m *MultishareOpsManager) verifyNoRunningInstanceOps(ctx context.Context, instanceSCPrefix string, instance *file.MultishareInstance) (bool, error) {
	opInfo := m.cache.GetInstanceOp(instanceSCPrefix, util.CreateInstanceKey(instance.Project, instance.Location, instance.Name))
	if opInfo == nil {
		return true, nil
	}
	if opInfo.Name == "" {
		return true, nil
	}

	op, err := m.cloud.File.GetOp(ctx, opInfo.Name)
	if err != nil && !file.IsNotFoundErr(err) {
		return false, err
	}

	// This method returns error if op completed with error. Instance is still considered ready since op completed.
	done, _ := m.cloud.File.IsOpDone(op)
	if done {
		m.cache.DeleteInstanceOp(instanceSCPrefix, util.CreateInstanceKey(instance.Project, instance.Location, instance.Name), opInfo.Name)
	}
	return done, nil
}

func (m *MultishareOpsManager) verifyNoRunningShareOp(ctx context.Context, instanceScPrefix string, share *file.Share) (bool, error) {
	shareCreateOpInfo, shareCreateOpStatus, err := m.checkAndUpdateShareCreateOp(ctx, instanceScPrefix, share.Name)
	if err != nil {
		return false, err
	}
	shareCreateopRunning := (shareCreateOpInfo != nil && shareCreateOpStatus == util.StatusRunning)

	shareOpInfo, shareOpStatus, err := m.checkAndUpdateShareOp(ctx, instanceScPrefix, share)
	if err != nil {
		return false, err
	}

	shareOpRunning := (shareOpInfo != nil && shareOpStatus == util.StatusRunning)
	return (!shareCreateopRunning && !shareOpRunning), nil
}

func (m *MultishareOpsManager) verifyNoRunningShareOpsForInstance(ctx context.Context, instanceScPrefix string, instance *file.MultishareInstance) (bool, error) {
	targetInstanceHandle, err := file.GetMultishareInstanceHandle(instance)
	if err != nil {
		return false, err
	}
	for _, item := range m.cache.GetShareCreateMap(instanceScPrefix).Items() {
		if item.OpInfo.InstanceHandle != targetInstanceHandle {
			continue
		}
		shareCreateOpInfo, shareCreateOpStatus, err := m.checkAndUpdateShareCreateOp(ctx, instanceScPrefix, item.Key)
		if err != nil {
			return false, err
		}
		if shareCreateOpInfo != nil && shareCreateOpStatus == util.StatusRunning {
			return false, nil
		}
	}

	for _, item := range m.cache.GetShareOpsMap(instanceScPrefix).Items() {
		shareHandle := item.Key
		if !containsInstancePrefix(string(shareHandle), instance.Project, instance.Location, instance.Name) {
			continue
		}

		_, _, _, shareName, err := util.ParseShareHandle(string(item.Key))
		if err != nil {
			return false, err
		}

		opInfo, status, err := m.checkAndUpdateShareOp(ctx, instanceScPrefix, &file.Share{Parent: instance, Name: shareName})
		if err != nil {
			return false, err
		}
		if opInfo != nil && status == util.StatusRunning {
			return false, nil
		}
	}

	return true, nil
}

func (m *MultishareOpsManager) validateShareExists(ctx context.Context, instanceHandle, shareName string) (*file.Share, error) {
	project, location, instanceName, err := util.ParseInstanceHandle(instanceHandle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to parse instance handle %v", instanceHandle)
	}

	return m.cloud.File.GetShare(ctx, &file.Share{
		Name: shareName,
		Parent: &file.MultishareInstance{
			Project:  project,
			Location: location,
			Name:     instanceName,
		},
	})
}

func (m *MultishareOpsManager) checkInstanceListForShare(ctx context.Context, instanceScPrefix string, targetShareName string) (*file.Share, error) {
	for _, item := range m.cache.GetInstanceMap(instanceScPrefix).Items() {
		project, location, instanceName, err := util.ParseInstanceHandle(string(item.Key))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to parse instance key %q, err: %v", item.Key, err)
		}

		shares, err := m.cloud.File.ListShares(ctx, &file.ListFilter{Project: project, Location: location, InstanceName: instanceName})
		// TODO: verify a list on a non existent instance would return not found error. or we need to check GET instance before listing shares.
		if err != nil && !file.IsNotFoundErr(err) {
			return nil, status.Errorf(codes.Internal, "failed to list shares for Instance %q, err: %v", item.Key, err)
		}

		for _, share := range shares {
			_, _, _, shareName, err := file.ParseShare(share)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to parse share URI, err: %v", err)
			}

			if shareName == targetShareName {
				return share, nil
			}
		}
	}

	return nil, nil
}

// runEligibleInstanceCheck returns a list of ready and non-ready instances.
func (m *MultishareOpsManager) runEligibleInstanceCheck(ctx context.Context, instanceScPrefix string) ([]*file.MultishareInstance, int, error) {
	var readyInstanceKeys []util.InstanceKey
	nonReadyInstanceCount := 0
	items := m.cache.GetInstanceMap(instanceScPrefix).Items()
	for _, item := range items {
		instanceKey := item.Key
		opInfo := item.OpInfo
		project, location, instanceName, err := util.ParseInstanceHandle(string(instanceKey))
		if err != nil {
			klog.Warningf("Failed to parse instance key %v", instanceKey)
			continue
		}
		if opInfo.Name == "" {
			readyInstanceKeys = append(readyInstanceKeys, instanceKey)
			continue
		}

		// check and clear completed op
		ready, err := m.verifyNoRunningInstanceOps(ctx, instanceScPrefix, &file.MultishareInstance{
			Project:  project,
			Location: location,
			Name:     instanceName,
		})
		if err != nil {
			klog.Warningf("Failed to check instance ready for instance key %v, err:%v", instanceKey, err)
		}

		if !ready && opInfo.Type != util.InstanceDelete {
			nonReadyInstanceCount = nonReadyInstanceCount + 1
		}

		if ready {
			readyInstanceKeys = append(readyInstanceKeys, instanceKey)
		}
	}

	var eligibleReadyInstances []*file.MultishareInstance
	for _, instanceKey := range readyInstanceKeys {
		project, location, instanceName, err := util.ParseInstanceHandle(string(instanceKey))
		if err != nil {
			klog.Warningf("Failed to parse instance key %v", instanceKey)
			continue
		}

		instance, err := m.cloud.File.GetMultishareInstance(ctx, &file.MultishareInstance{
			Project:  project,
			Location: location,
			Name:     instanceName,
		})
		if err != nil {
			if file.IsNotFoundErr(err) {
				klog.Infof("Instance %v not found, clear from map", instanceKey)
				m.cache.DeleteInstance(instanceScPrefix, instanceKey)
			} else {
				klog.Warningf("Failed to GET for instance key %v, err:%v", instanceKey, err)
			}
			continue
		}

		shares, err := m.cloud.File.ListShares(ctx, &file.ListFilter{Project: project, Location: location, InstanceName: instanceName})
		// TODO: verify what is the behavior for 0 share instance? i.e. len(shares)=0, err = nil?
		if err != nil {
			klog.Warningf("failed to list shares for Instance %q, err: %v", instanceKey, err)
			continue
		}

		if len(shares) < util.MaxSharesPerInstance {
			eligibleReadyInstances = append(eligibleReadyInstances, instance)
		}
	}

	return eligibleReadyInstances, nonReadyInstanceCount, nil
}

func (m *MultishareOpsManager) instanceNeedsExpand(ctx context.Context, share *file.Share) (bool, int64, error) {
	if share == nil {
		return false, 0, fmt.Errorf("empty share")
	}
	if share.Parent == nil {
		return false, 0, fmt.Errorf("Parent missing from share %q", share.Name)
	}

	shares, err := m.cloud.File.ListShares(ctx, &file.ListFilter{Project: share.Parent.Project, Location: share.Parent.Location, InstanceName: share.Parent.Name})
	if err != nil {
		return false, 0, err
	}

	var sumShareBytes int64
	for _, s := range shares {
		sumShareBytes = sumShareBytes + s.CapacityBytes
	}
	// TODO: Check if we need to align the increment to step size.
	var remainingBytes int64
	remainingBytes = share.Parent.CapacityBytes - sumShareBytes
	if remainingBytes < share.CapacityBytes {
		return true, share.Parent.CapacityBytes + (share.CapacityBytes - remainingBytes), nil
	}
	return false, 0, nil
}
