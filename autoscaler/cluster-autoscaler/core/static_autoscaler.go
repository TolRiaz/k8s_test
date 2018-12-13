/*
Copyright 2016 The Kubernetes Authors.

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

package core

import (
	"time"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/clusterstate"
	"k8s.io/autoscaler/cluster-autoscaler/clusterstate/utils"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/context"
	"k8s.io/autoscaler/cluster-autoscaler/estimator"
	"k8s.io/autoscaler/cluster-autoscaler/expander"
	"k8s.io/autoscaler/cluster-autoscaler/metrics"
	ca_processors "k8s.io/autoscaler/cluster-autoscaler/processors"
	"k8s.io/autoscaler/cluster-autoscaler/simulator"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
	"k8s.io/autoscaler/cluster-autoscaler/utils/tpu"

	apiv1 "k8s.io/api/core/v1"

	"k8s.io/autoscaler/cluster-autoscaler/processors/status"
	"k8s.io/autoscaler/cluster-autoscaler/utils/backoff"
	"k8s.io/klog"
)

const (
	// How old the oldest unschedulable pod should be before starting scale up.
	unschedulablePodTimeBuffer = 2 * time.Second
	// How old the oldest unschedulable pod with GPU should be before starting scale up.
	// The idea is that nodes with GPU are very expensive and we're ready to sacrifice
	// a bit more latency to wait for more pods and make a more informed scale-up decision.
	unschedulablePodWithGpuTimeBuffer = 30 * time.Second
	// How long should Cluster Autoscaler wait for nodes to become ready after start.
	nodesNotReadyAfterStartTimeout = 10 * time.Minute
)

// StaticAutoscaler is an autoscaler which has all the core functionality of a CA but without the reconfiguration feature
type StaticAutoscaler struct {
	// AutoscalingContext consists of validated settings and options for this autoscaler
	*context.AutoscalingContext
	// ClusterState for maintaining the state of cluster nodes.
	clusterStateRegistry    *clusterstate.ClusterStateRegistry
	startTime               time.Time
	lastScaleUpTime         time.Time
	lastScaleDownDeleteTime time.Time
	lastScaleDownFailTime   time.Time
	scaleDown               *ScaleDown
	processors              *ca_processors.AutoscalingProcessors
	initialized             bool
}

// NewStaticAutoscaler creates an instance of Autoscaler filled with provided parameters
func NewStaticAutoscaler(
	opts config.AutoscalingOptions,
	predicateChecker *simulator.PredicateChecker,
	autoscalingKubeClients *context.AutoscalingKubeClients,
	processors *ca_processors.AutoscalingProcessors,
	cloudProvider cloudprovider.CloudProvider,
	expanderStrategy expander.Strategy,
	estimatorBuilder estimator.EstimatorBuilder,
	backoff backoff.Backoff) *StaticAutoscaler {
	autoscalingContext := context.NewAutoscalingContext(opts, predicateChecker, autoscalingKubeClients, cloudProvider, expanderStrategy, estimatorBuilder)

	clusterStateConfig := clusterstate.ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: opts.MaxTotalUnreadyPercentage,
		OkTotalUnreadyCount:       opts.OkTotalUnreadyCount,
		MaxNodeProvisionTime:      opts.MaxNodeProvisionTime,
	}
	clusterStateRegistry := clusterstate.NewClusterStateRegistry(autoscalingContext.CloudProvider, clusterStateConfig, autoscalingContext.LogRecorder, backoff)

	scaleDown := NewScaleDown(autoscalingContext, clusterStateRegistry)

	return &StaticAutoscaler{
		AutoscalingContext:      autoscalingContext,
		startTime:               time.Now(),
		lastScaleUpTime:         time.Now(),
		lastScaleDownDeleteTime: time.Now(),
		lastScaleDownFailTime:   time.Now(),
		scaleDown:               scaleDown,
		processors:              processors,
		clusterStateRegistry:    clusterStateRegistry,
	}
}

// cleanUpIfRequired removes ToBeDeleted taints added by a previous run of CA
// the taints are removed only once per runtime
func (a *StaticAutoscaler) cleanUpIfRequired() {
	if a.initialized {
		return
	}

	// CA can die at any time. Removing taints that might have been left from the previous run.
	if readyNodes, err := a.ReadyNodeLister().List(); err != nil {
		klog.Errorf("Failed to list ready nodes, not cleaning up taints: %v", err)
	} else {
		cleanToBeDeleted(readyNodes, a.AutoscalingContext.ClientSet, a.Recorder)
	}
	a.initialized = true
}

// RunOnce iterates over node groups and scales them up/down if necessary
func (a *StaticAutoscaler) RunOnce(currentTime time.Time) errors.AutoscalerError {
	a.cleanUpIfRequired()

	unschedulablePodLister := a.UnschedulablePodLister()
	scheduledPodLister := a.ScheduledPodLister()
	pdbLister := a.PodDisruptionBudgetLister()
	scaleDown := a.scaleDown
	autoscalingContext := a.AutoscalingContext

	klog.V(4).Info("Starting main loop")

	stateUpdateStart := time.Now()
	allNodes, readyNodes, typedErr := a.obtainNodeLists()
	if typedErr != nil {
		return typedErr
	}
	if a.actOnEmptyCluster(allNodes, readyNodes, currentTime) {
		return nil
	}

	typedErr = a.updateClusterState(allNodes, currentTime)
	if typedErr != nil {
		return typedErr
	}
	metrics.UpdateDurationFromStart(metrics.UpdateState, stateUpdateStart)

	scaleUpStatus := &status.ScaleUpStatus{Result: status.ScaleUpNotTried}
	scaleUpStatusProcessorAlreadyCalled := false
	scaleDownStatus := &status.ScaleDownStatus{Result: status.ScaleDownNotTried}
	scaleDownStatusProcessorAlreadyCalled := false

	defer func() {
		// Update status information when the loop is done (regardless of reason)
		if autoscalingContext.WriteStatusConfigMap {
			status := a.clusterStateRegistry.GetStatus(currentTime)
			utils.WriteStatusConfigMap(autoscalingContext.ClientSet, autoscalingContext.ConfigNamespace,
				status.GetReadableString(), a.AutoscalingContext.LogRecorder)
		}

		// This deferred processor execution allows the processors to handle a situation when a scale-(up|down)
		// wasn't even attempted because e.g. the iteration exited earlier.
		if !scaleUpStatusProcessorAlreadyCalled && a.processors != nil && a.processors.ScaleUpStatusProcessor != nil {
			a.processors.ScaleUpStatusProcessor.Process(a.AutoscalingContext, scaleUpStatus)
		}
		if !scaleDownStatusProcessorAlreadyCalled && a.processors != nil && a.processors.ScaleDownStatusProcessor != nil {
			a.processors.ScaleDownStatusProcessor.Process(a.AutoscalingContext, scaleDownStatus)
		}

		err := a.processors.AutoscalingStatusProcessor.Process(a.AutoscalingContext, a.clusterStateRegistry, currentTime)
		if err != nil {
			klog.Errorf("AutoscalingStatusProcessor error: %v.", err)
		}
	}()

	// Check if there are any nodes that failed to register in Kubernetes
	// master.
	unregisteredNodes := a.clusterStateRegistry.GetUnregisteredNodes()
	if len(unregisteredNodes) > 0 {
		klog.V(1).Infof("%d unregistered nodes present", len(unregisteredNodes))
		removedAny, err := removeOldUnregisteredNodes(unregisteredNodes, autoscalingContext, currentTime, autoscalingContext.LogRecorder)
		// There was a problem with removing unregistered nodes. Retry in the next loop.
		if err != nil {
			if removedAny {
				klog.Warningf("Some unregistered nodes were removed, but got error: %v", err)
			} else {
				klog.Errorf("Failed to remove unregistered nodes: %v", err)

			}
			return errors.ToAutoscalerError(errors.CloudProviderError, err)
		}
		// Some nodes were removed. Let's skip this iteration, the next one should be better.
		if removedAny {
			klog.V(0).Infof("Some unregistered nodes were removed, skipping iteration")
			return nil
		}
	}
	if !a.clusterStateRegistry.IsClusterHealthy() {
		klog.Warning("Cluster is not ready for autoscaling")
		scaleDown.CleanUpUnneededNodes()
		autoscalingContext.LogRecorder.Eventf(apiv1.EventTypeWarning, "ClusterUnhealthy", "Cluster is unhealthy")
		return nil
	}

	// Check if there has been a constant difference between the number of nodes in k8s and
	// the number of nodes on the cloud provider side.
	// TODO: andrewskim - add protection for ready AWS nodes.
	fixedSomething, err := fixNodeGroupSize(autoscalingContext, a.clusterStateRegistry, currentTime)
	if err != nil {
		klog.Errorf("Failed to fix node group sizes: %v", err)
		return errors.ToAutoscalerError(errors.CloudProviderError, err)
	}
	if fixedSomething {
		klog.V(0).Infof("Some node group target size was fixed, skipping the iteration")
		return nil
	}

	metrics.UpdateLastTime(metrics.Autoscaling, time.Now())

	allUnschedulablePods, err := unschedulablePodLister.List()
	if err != nil {
		klog.Errorf("Failed to list unscheduled pods: %v", err)
		return errors.ToAutoscalerError(errors.ApiCallError, err)
	}
	metrics.UpdateUnschedulablePodsCount(len(allUnschedulablePods))

	allScheduled, err := scheduledPodLister.List()
	if err != nil {
		klog.Errorf("Failed to list scheduled pods: %v", err)
		return errors.ToAutoscalerError(errors.ApiCallError, err)
	}

	allUnschedulablePods, allScheduled, err = a.processors.PodListProcessor.Process(a.AutoscalingContext, allUnschedulablePods, allScheduled, allNodes)
	if err != nil {
		klog.Errorf("Failed to process pod list: %v", err)
		return errors.ToAutoscalerError(errors.InternalError, err)
	}

	ConfigurePredicateCheckerForLoop(allUnschedulablePods, allScheduled, a.PredicateChecker)

	// We need to check whether pods marked as unschedulable are actually unschedulable.
	// It's likely we added a new node and the scheduler just haven't managed to put the
	// pod on in yet. In this situation we don't want to trigger another scale-up.
	//
	// It's also important to prevent uncontrollable cluster growth if CA's simulated
	// scheduler differs in opinion with real scheduler. Example of such situation:
	// - CA and Scheduler has slightly different configuration
	// - Scheduler can't schedule a pod and marks it as unschedulable
	// - CA added a node which should help the pod
	// - Scheduler doesn't schedule the pod on the new node
	//   because according to it logic it doesn't fit there
	// - CA see the pod is still unschedulable, so it adds another node to help it
	//
	// With the check enabled the last point won't happen because CA will ignore a pod
	// which is supposed to schedule on an existing node.
	scaleDownForbidden := false

	unschedulablePodsWithoutTPUs := tpu.ClearTPURequests(allUnschedulablePods)

	// Some unschedulable pods can be waiting for lower priority pods preemption so they have nominated node to run.
	// Such pods don't require scale up but should be considered during scale down.
	unschedulablePods, unschedulableWaitingForLowerPriorityPreemption := FilterOutExpendableAndSplit(unschedulablePodsWithoutTPUs, a.ExpendablePodsPriorityCutoff)

	klog.V(4).Infof("Filtering out schedulables")
	filterOutSchedulableStart := time.Now()
	unschedulablePodsToHelp := FilterOutSchedulable(unschedulablePods, readyNodes, allScheduled,
		unschedulableWaitingForLowerPriorityPreemption, a.PredicateChecker, a.ExpendablePodsPriorityCutoff)
	metrics.UpdateDurationFromStart(metrics.FilterOutSchedulable, filterOutSchedulableStart)

	if len(unschedulablePodsToHelp) != len(unschedulablePods) {
		klog.V(2).Info("Schedulable pods present")
		scaleDownForbidden = true
	} else {
		klog.V(4).Info("No schedulable pods")
	}

	// finally, filter out pods that are too "young" to safely be considered for a scale-up (delay is configurable)
	unschedulablePodsToHelp = a.filterOutYoungPods(unschedulablePodsToHelp, currentTime)

	if len(unschedulablePodsToHelp) == 0 {
		scaleUpStatus.Result = status.ScaleUpNotNeeded
		klog.V(1).Info("No unschedulable pods")
	} else if a.MaxNodesTotal > 0 && len(readyNodes) >= a.MaxNodesTotal {
		scaleUpStatus.Result = status.ScaleUpNoOptionsAvailable
		klog.V(1).Info("Max total nodes in cluster reached")
	} else if allPodsAreNew(unschedulablePodsToHelp, currentTime) {
		// The assumption here is that these pods have been created very recently and probably there
		// is more pods to come. In theory we could check the newest pod time but then if pod were created
		// slowly but at the pace of 1 every 2 seconds then no scale up would be triggered for long time.
		// We also want to skip a real scale down (just like if the pods were handled).
		scaleDownForbidden = true
		scaleUpStatus.Result = status.ScaleUpInCooldown
		klog.V(1).Info("Unschedulable pods are very new, waiting one iteration for more")
	} else {
		daemonsets, err := a.ListerRegistry.DaemonSetLister().List()
		if err != nil {
			klog.Errorf("Failed to get daemonset list")
			scaleUpStatus.Result = status.ScaleUpError
			return errors.ToAutoscalerError(errors.ApiCallError, err)
		}

		scaleUpStart := time.Now()
		metrics.UpdateLastTime(metrics.ScaleUp, scaleUpStart)

		scaleUpStatus, typedErr = ScaleUp(autoscalingContext, a.processors, a.clusterStateRegistry, unschedulablePodsToHelp, readyNodes, daemonsets)

		metrics.UpdateDurationFromStart(metrics.ScaleUp, scaleUpStart)

		if a.processors != nil && a.processors.ScaleUpStatusProcessor != nil {
			a.processors.ScaleUpStatusProcessor.Process(autoscalingContext, scaleUpStatus)
			scaleUpStatusProcessorAlreadyCalled = true
		}

		if typedErr != nil {
			klog.Errorf("Failed to scale up: %v", typedErr)
			return typedErr
		}
		if scaleUpStatus.Result == status.ScaleUpSuccessful {
			a.lastScaleUpTime = currentTime
			// No scale down in this iteration.
			scaleDownStatus.Result = status.ScaleDownInCooldown
			return nil
		}
	}

	if a.ScaleDownEnabled {
		pdbs, err := pdbLister.List()
		if err != nil {
			scaleDownStatus.Result = status.ScaleDownError
			klog.Errorf("Failed to list pod disruption budgets: %v", err)
			return errors.ToAutoscalerError(errors.ApiCallError, err)
		}

		unneededStart := time.Now()

		klog.V(4).Infof("Calculating unneeded nodes")

		scaleDown.CleanUp(currentTime)
		potentiallyUnneeded := getPotentiallyUnneededNodes(autoscalingContext, allNodes)

		typedErr := scaleDown.UpdateUnneededNodes(allNodes, potentiallyUnneeded, append(allScheduled, unschedulableWaitingForLowerPriorityPreemption...), currentTime, pdbs)
		if typedErr != nil {
			scaleDownStatus.Result = status.ScaleDownError
			klog.Errorf("Failed to scale down: %v", typedErr)
			return typedErr
		}

		metrics.UpdateDurationFromStart(metrics.FindUnneeded, unneededStart)

		if klog.V(4) {
			for key, val := range scaleDown.unneededNodes {
				klog.Infof("%s is unneeded since %s duration %s", key, val.String(), currentTime.Sub(val).String())
			}
		}

		scaleDownInCooldown := scaleDownForbidden ||
			a.lastScaleUpTime.Add(a.ScaleDownDelayAfterAdd).After(currentTime) ||
			a.lastScaleDownFailTime.Add(a.ScaleDownDelayAfterFailure).After(currentTime) ||
			a.lastScaleDownDeleteTime.Add(a.ScaleDownDelayAfterDelete).After(currentTime)
		// In dry run only utilization is updated
		calculateUnneededOnly := scaleDownInCooldown || scaleDown.nodeDeleteStatus.IsDeleteInProgress()

		klog.V(4).Infof("Scale down status: unneededOnly=%v lastScaleUpTime=%s "+
			"lastScaleDownDeleteTime=%v lastScaleDownFailTime=%s scaleDownForbidden=%v isDeleteInProgress=%v",
			calculateUnneededOnly, a.lastScaleUpTime, a.lastScaleDownDeleteTime, a.lastScaleDownFailTime,
			scaleDownForbidden, scaleDown.nodeDeleteStatus.IsDeleteInProgress())

		if scaleDownInCooldown {
			scaleDownStatus.Result = status.ScaleDownInCooldown
		} else if scaleDown.nodeDeleteStatus.IsDeleteInProgress() {
			scaleDownStatus.Result = status.ScaleDownInProgress
		} else {
			klog.V(4).Infof("Starting scale down")

			// We want to delete unneeded Node Groups only if there was no recent scale up,
			// and there is no current delete in progress and there was no recent errors.
			a.processors.NodeGroupManager.RemoveUnneededNodeGroups(autoscalingContext)

			scaleDownStart := time.Now()
			metrics.UpdateLastTime(metrics.ScaleDown, scaleDownStart)
			scaleDownStatus, typedErr := scaleDown.TryToScaleDown(allNodes, allScheduled, pdbs, currentTime)
			metrics.UpdateDurationFromStart(metrics.ScaleDown, scaleDownStart)

			if scaleDownStatus.Result == status.ScaleDownNodeDeleted {
				a.lastScaleDownDeleteTime = currentTime
				a.clusterStateRegistry.Recalculate()
			}

			if a.processors != nil && a.processors.ScaleDownStatusProcessor != nil {
				a.processors.ScaleDownStatusProcessor.Process(autoscalingContext, scaleDownStatus)
				scaleDownStatusProcessorAlreadyCalled = true
			}

			if typedErr != nil {
				klog.Errorf("Failed to scale down: %v", err)
				a.lastScaleDownFailTime = currentTime
				return typedErr
			}
		}
	}
	return nil
}

// don't consider pods newer than newPodScaleUpDelay seconds old as unschedulable
func (a *StaticAutoscaler) filterOutYoungPods(allUnschedulablePods []*apiv1.Pod, currentTime time.Time) []*apiv1.Pod {
	var oldUnschedulablePods []*apiv1.Pod
	newPodScaleUpDelay := a.AutoscalingOptions.NewPodScaleUpDelay
	for _, pod := range allUnschedulablePods {
		podAge := currentTime.Sub(pod.CreationTimestamp.Time)
		if podAge > newPodScaleUpDelay {
			oldUnschedulablePods = append(oldUnschedulablePods, pod)
		} else {
			klog.V(3).Infof("Pod %s is %.3f seconds old, too new to consider unschedulable", pod.Name, podAge.Seconds())

		}
	}
	return oldUnschedulablePods
}

// ExitCleanUp performs all necessary clean-ups when the autoscaler's exiting.
func (a *StaticAutoscaler) ExitCleanUp() {
	a.processors.CleanUp()

	if !a.AutoscalingContext.WriteStatusConfigMap {
		return
	}
	utils.DeleteStatusConfigMap(a.AutoscalingContext.ClientSet, a.AutoscalingContext.ConfigNamespace)
}

func (a *StaticAutoscaler) obtainNodeLists() ([]*apiv1.Node, []*apiv1.Node, errors.AutoscalerError) {
	allNodes, err := a.AllNodeLister().List()
	if err != nil {
		klog.Errorf("Failed to list all nodes: %v", err)
		return nil, nil, errors.ToAutoscalerError(errors.ApiCallError, err)
	}
	readyNodes, err := a.ReadyNodeLister().List()
	if err != nil {
		klog.Errorf("Failed to list ready nodes: %v", err)
		return nil, nil, errors.ToAutoscalerError(errors.ApiCallError, err)
	}

	// Handle GPU case - allocatable GPU may be equal to 0 up to 15 minutes after
	// node registers as ready. See https://github.com/kubernetes/kubernetes/issues/54959
	// Treat those nodes as unready until GPU actually becomes available and let
	// our normal handling for booting up nodes deal with this.
	// TODO: Remove this call when we handle dynamically provisioned resources.
	allNodes, readyNodes = gpu.FilterOutNodesWithUnreadyGpus(allNodes, readyNodes)
	return allNodes, readyNodes, nil
}

// actOnEmptyCluster returns true if the cluster was empty and thus acted upon
func (a *StaticAutoscaler) actOnEmptyCluster(allNodes, readyNodes []*apiv1.Node, currentTime time.Time) bool {
	if len(allNodes) == 0 {
		a.onEmptyCluster("Cluster has no nodes.", true)
		return true
	}
	if len(readyNodes) == 0 {
		// Cluster Autoscaler may start running before nodes are ready.
		// Timeout ensures no ClusterUnhealthy events are published immediately in this case.
		a.onEmptyCluster("Cluster has no ready nodes.", currentTime.After(a.startTime.Add(nodesNotReadyAfterStartTimeout)))
		return true
	}
	// the cluster is not empty
	return false
}

func (a *StaticAutoscaler) updateClusterState(allNodes []*apiv1.Node, currentTime time.Time) errors.AutoscalerError {
	err := a.AutoscalingContext.CloudProvider.Refresh()
	if err != nil {
		klog.Errorf("Failed to refresh cloud provider config: %v", err)
		return errors.ToAutoscalerError(errors.CloudProviderError, err)
	}

	err = a.clusterStateRegistry.UpdateNodes(allNodes, currentTime)
	if err != nil {
		klog.Errorf("Failed to update node registry: %v", err)
		a.scaleDown.CleanUpUnneededNodes()
		return errors.ToAutoscalerError(errors.CloudProviderError, err)
	}
	UpdateClusterStateMetrics(a.clusterStateRegistry)

	return nil
}

func (a *StaticAutoscaler) onEmptyCluster(status string, emitEvent bool) {
	klog.Warningf(status)
	a.scaleDown.CleanUpUnneededNodes()
	UpdateEmptyClusterStateMetrics()
	if a.AutoscalingContext.WriteStatusConfigMap {
		utils.WriteStatusConfigMap(a.AutoscalingContext.ClientSet, a.AutoscalingContext.ConfigNamespace, status, a.AutoscalingContext.LogRecorder)
	}
	if emitEvent {
		a.AutoscalingContext.LogRecorder.Eventf(apiv1.EventTypeWarning, "ClusterUnhealthy", status)
	}
}

func allPodsAreNew(pods []*apiv1.Pod, currentTime time.Time) bool {
	if getOldestCreateTime(pods).Add(unschedulablePodTimeBuffer).After(currentTime) {
		return true
	}
	found, oldest := getOldestCreateTimeWithGpu(pods)
	return found && oldest.Add(unschedulablePodWithGpuTimeBuffer).After(currentTime)
}
