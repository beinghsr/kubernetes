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
	"fmt"
	"hash/fnv"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/scheduler/algorithm"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/predicates"
	schedulercache "k8s.io/kubernetes/pkg/scheduler/cache"
	hashutil "k8s.io/kubernetes/pkg/util/hash"

	"github.com/golang/glog"
)

// EquivalenceCache saves and reuses the output of predicate functions. Use
// RunPredicate to get or update the cached results. An appropriate Invalidate*
// function should be called when some predicate results are no longer valid.
//
// Internally, results are keyed by node name, predicate name, and "equivalence
// class". (Equivalence class is defined in the `EquivalenceClassInfo` type.)
// Saved results will be reused until an appropriate invalidation function is
// called.
type EquivalenceCache struct {
	mu    sync.RWMutex
	cache nodeMap
}

// nodeMap stores PredicateCaches with node name as the key.
type nodeMap map[string]predicateMap

// predicateMap stores resultMaps with predicate name as the key.
type predicateMap map[string]resultMap

// resultMap stores PredicateResult with pod equivalence hash as the key.
type resultMap map[uint64]predicateResult

// predicateResult stores the output of a FitPredicate.
type predicateResult struct {
	Fit         bool
	FailReasons []algorithm.PredicateFailureReason
}

// NewEquivalenceCache returns EquivalenceCache to speed up predicates by caching
// result from previous scheduling.
func NewEquivalenceCache() *EquivalenceCache {
	return &EquivalenceCache{
		cache: make(nodeMap),
	}
}

// RunPredicate returns a cached predicate result. In case of a cache miss, the predicate will be
// run and its results cached for the next call.
//
// NOTE: RunPredicate will not update the equivalence cache if the given NodeInfo is stale.
func (ec *EquivalenceCache) RunPredicate(
	pred algorithm.FitPredicate,
	predicateKey string,
	pod *v1.Pod,
	meta algorithm.PredicateMetadata,
	nodeInfo *schedulercache.NodeInfo,
	equivClassInfo *EquivalenceClassInfo,
	cache schedulercache.Cache,
) (bool, []algorithm.PredicateFailureReason, error) {
	if nodeInfo == nil || nodeInfo.Node() == nil {
		// This may happen during tests.
		return false, []algorithm.PredicateFailureReason{}, fmt.Errorf("nodeInfo is nil or node is invalid")
	}

	result, ok := ec.lookupResult(pod.GetName(), nodeInfo.Node().GetName(), predicateKey, equivClassInfo.hash)
	if ok {
		return result.Fit, result.FailReasons, nil
	}
	fit, reasons, err := pred(pod, meta, nodeInfo)
	if err != nil {
		return fit, reasons, err
	}
	if cache != nil {
		ec.updateResult(pod.GetName(), predicateKey, fit, reasons, equivClassInfo.hash, cache, nodeInfo)
	}
	return fit, reasons, nil
}

// updateResult updates the cached result of a predicate.
func (ec *EquivalenceCache) updateResult(
	podName, predicateKey string,
	fit bool,
	reasons []algorithm.PredicateFailureReason,
	equivalenceHash uint64,
	cache schedulercache.Cache,
	nodeInfo *schedulercache.NodeInfo,
) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	if nodeInfo == nil || nodeInfo.Node() == nil {
		// This may happen during tests.
		return
	}
	// Skip update if NodeInfo is stale.
	if !cache.IsUpToDate(nodeInfo) {
		return
	}
	nodeName := nodeInfo.Node().GetName()
	if _, exist := ec.cache[nodeName]; !exist {
		ec.cache[nodeName] = make(predicateMap)
	}
	predicateItem := predicateResult{
		Fit:         fit,
		FailReasons: reasons,
	}
	// if cached predicate map already exists, just update the predicate by key
	if predicates, ok := ec.cache[nodeName][predicateKey]; ok {
		// maps in golang are references, no need to add them back
		predicates[equivalenceHash] = predicateItem
	} else {
		ec.cache[nodeName][predicateKey] =
			resultMap{
				equivalenceHash: predicateItem,
			}
	}
	glog.V(5).Infof("Updated cached predicate: %v for pod: %v on node: %s, with item %v", predicateKey, podName, nodeName, predicateItem)
}

// lookupResult returns cached predicate results and a bool saying whether a
// cache entry was found.
func (ec *EquivalenceCache) lookupResult(
	podName, nodeName, predicateKey string,
	equivalenceHash uint64,
) (value predicateResult, ok bool) {
	ec.mu.RLock()
	defer ec.mu.RUnlock()
	glog.V(5).Infof("Begin to calculate predicate: %v for pod: %s on node: %s based on equivalence cache",
		predicateKey, podName, nodeName)
	value, ok = ec.cache[nodeName][predicateKey][equivalenceHash]
	return value, ok
}

// InvalidatePredicates clears all cached results for the given predicates.
func (ec *EquivalenceCache) InvalidatePredicates(predicateKeys sets.String) {
	if len(predicateKeys) == 0 {
		return
	}
	ec.mu.Lock()
	defer ec.mu.Unlock()
	// ec.cache uses nodeName as key, so we just iterate it and invalid given predicates
	for _, predicates := range ec.cache {
		for predicateKey := range predicateKeys {
			delete(predicates, predicateKey)
		}
	}
	glog.V(5).Infof("Done invalidating cached predicates: %v on all node", predicateKeys)
}

// InvalidatePredicatesOnNode clears cached results for the given predicates on one node.
func (ec *EquivalenceCache) InvalidatePredicatesOnNode(nodeName string, predicateKeys sets.String) {
	if len(predicateKeys) == 0 {
		return
	}
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for predicateKey := range predicateKeys {
		delete(ec.cache[nodeName], predicateKey)
	}
	glog.V(5).Infof("Done invalidating cached predicates: %v on node: %s", predicateKeys, nodeName)
}

// InvalidateAllPredicatesOnNode clears all cached results for one node.
func (ec *EquivalenceCache) InvalidateAllPredicatesOnNode(nodeName string) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	delete(ec.cache, nodeName)
	glog.V(5).Infof("Done invalidating all cached predicates on node: %s", nodeName)
}

// InvalidateCachedPredicateItemForPodAdd is a wrapper of
// InvalidateCachedPredicateItem for pod add case
// TODO: This logic does not belong with the equivalence cache implementation.
func (ec *EquivalenceCache) InvalidateCachedPredicateItemForPodAdd(pod *v1.Pod, nodeName string) {
	// MatchInterPodAffinity: we assume scheduler can make sure newly bound pod
	// will not break the existing inter pod affinity. So we does not need to
	// invalidate MatchInterPodAffinity when pod added.
	//
	// But when a pod is deleted, existing inter pod affinity may become invalid.
	// (e.g. this pod was preferred by some else, or vice versa)
	//
	// NOTE: assumptions above will not stand when we implemented features like
	// RequiredDuringSchedulingRequiredDuringExecution.

	// NoDiskConflict: the newly scheduled pod fits to existing pods on this node,
	// it will also fits to equivalence class of existing pods

	// GeneralPredicates: will always be affected by adding a new pod
	invalidPredicates := sets.NewString(predicates.GeneralPred)

	// MaxPDVolumeCountPredicate: we check the volumes of pod to make decision.
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			invalidPredicates.Insert(predicates.MaxEBSVolumeCountPred, predicates.MaxGCEPDVolumeCountPred, predicates.MaxAzureDiskVolumeCountPred)
		} else {
			if vol.AWSElasticBlockStore != nil {
				invalidPredicates.Insert(predicates.MaxEBSVolumeCountPred)
			}
			if vol.GCEPersistentDisk != nil {
				invalidPredicates.Insert(predicates.MaxGCEPDVolumeCountPred)
			}
			if vol.AzureDisk != nil {
				invalidPredicates.Insert(predicates.MaxAzureDiskVolumeCountPred)
			}
		}
	}
	ec.InvalidatePredicatesOnNode(nodeName, invalidPredicates)
}

// EquivalenceClassInfo holds equivalence hash which is used for checking
// equivalence cache. We will pass this to podFitsOnNode to ensure equivalence
// hash is only calculated per schedule.
type EquivalenceClassInfo struct {
	// Equivalence hash.
	hash uint64
}

// GetEquivalenceClassInfo returns a hash of the given pod. The hashing function
// returns the same value for any two pods that are equivalent from the
// perspective of scheduling.
func (ec *EquivalenceCache) GetEquivalenceClassInfo(pod *v1.Pod) *EquivalenceClassInfo {
	equivalencePod := getEquivalencePod(pod)
	if equivalencePod != nil {
		hash := fnv.New32a()
		hashutil.DeepHashObject(hash, equivalencePod)
		return &EquivalenceClassInfo{
			hash: uint64(hash.Sum32()),
		}
	}
	return nil
}

// equivalencePod is the set of pod attributes which must match for two pods to
// be considered equivalent for scheduling purposes. For correctness, this must
// include any Pod field which is used by a FitPredicate.
//
// NOTE: For equivalence hash to be formally correct, lists and maps in the
// equivalencePod should be normalized. (e.g. by sorting them) However, the vast
// majority of equivalent pod classes are expected to be created from a single
// pod template, so they will all have the same ordering.
type equivalencePod struct {
	Namespace      *string
	Labels         map[string]string
	Affinity       *v1.Affinity
	Containers     []v1.Container // See note about ordering
	InitContainers []v1.Container // See note about ordering
	NodeName       *string
	NodeSelector   map[string]string
	Tolerations    []v1.Toleration
	Volumes        []v1.Volume // See note about ordering
}

// getEquivalencePod returns a normalized representation of a pod so that two
// "equivalent" pods will hash to the same value.
func getEquivalencePod(pod *v1.Pod) *equivalencePod {
	ep := &equivalencePod{
		Namespace:      &pod.Namespace,
		Labels:         pod.Labels,
		Affinity:       pod.Spec.Affinity,
		Containers:     pod.Spec.Containers,
		InitContainers: pod.Spec.InitContainers,
		NodeName:       &pod.Spec.NodeName,
		NodeSelector:   pod.Spec.NodeSelector,
		Tolerations:    pod.Spec.Tolerations,
		Volumes:        pod.Spec.Volumes,
	}
	// DeepHashObject considers nil and empty slices to be different. Normalize them.
	if len(ep.Containers) == 0 {
		ep.Containers = nil
	}
	if len(ep.InitContainers) == 0 {
		ep.InitContainers = nil
	}
	if len(ep.Tolerations) == 0 {
		ep.Tolerations = nil
	}
	if len(ep.Volumes) == 0 {
		ep.Volumes = nil
	}
	// Normalize empty maps also.
	if len(ep.Labels) == 0 {
		ep.Labels = nil
	}
	if len(ep.NodeSelector) == 0 {
		ep.NodeSelector = nil
	}
	// TODO(misterikkit): Also normalize nested maps and slices.
	return ep
}
