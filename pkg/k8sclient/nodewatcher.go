/*
Copyright 2018 The Kubernetes Authors.

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

package k8sclient

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/kubernetes-sigs/poseidon/pkg/firmament"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NewNodeWatcher initializes a NodeWatcher based on the given Kubernetes client and Firmament client.
func NewNodeWatcher(client kubernetes.Interface, fc firmament.FirmamentSchedulerClient) *NodeWatcher {
	glog.Info("Starting NodeWatcher...")
	NodesCond = sync.NewCond(&sync.Mutex{})
	NodeToRTND = make(map[string]*firmament.ResourceTopologyNodeDescriptor)
	ResIDToNode = make(map[string]string)
	nodewatcher := &NodeWatcher{
		clientset: client,
		fc:        fc,
	}
	_, controller := cache.NewInformer(
		&cache.ListWatch{
			ListFunc: func(alo metav1.ListOptions) (runtime.Object, error) {
				return client.CoreV1().Nodes().List(alo)
			},
			WatchFunc: func(alo metav1.ListOptions) (watch.Interface, error) {
				return client.CoreV1().Nodes().Watch(alo)
			},
		},
		&v1.Node{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err != nil {
					glog.Errorf("AddFunc: error getting key %v", err)
				}
				nodewatcher.enqueueNodeAddition(key, obj)
			},
			UpdateFunc: func(old, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err != nil {
					glog.Errorf("UpdateFunc: error getting key %v", err)
				}
				nodewatcher.enqueueNodeUpdate(key, old, new)
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err != nil {
					glog.Errorf("DeleteFunc: error getting key %v", err)
				}
				nodewatcher.enqueueNodeDeletion(key, obj)
			},
		},
	)
	nodewatcher.controller = controller
	nodewatcher.nodeWorkQueue = NewKeyedQueue()
	return nodewatcher
}

func (nw *NodeWatcher) getReadyAndOutOfDiskConditions(node *v1.Node) (isReady bool, isOutOfDisk bool) {
	isReady = false
	isOutOfDisk = false
	for _, cond := range node.Status.Conditions {
		switch cond.Type {
		case "OutOfDisk":
			isOutOfDisk = cond.Status == "True"
		case "Ready":
			isReady = cond.Status == "True"
		}
	}
	return isReady, isOutOfDisk
}

func (nw *NodeWatcher) parseNode(node *v1.Node, phase NodePhase) *Node {
	isReady, isOutOfDisk := nw.getReadyAndOutOfDiskConditions(node)
	cpuCapQuantity := node.Status.Capacity["cpu"]
	cpuAllocQuantity := node.Status.Allocatable["cpu"]
	memCapQuantity := node.Status.Capacity["memory"]
	memCap, _ := memCapQuantity.AsInt64()
	memAllocQuantity := node.Status.Allocatable["memory"]
	memAlloc, _ := memAllocQuantity.AsInt64()
	return &Node{
		Hostname:         node.Name,
		Phase:            phase,
		IsReady:          isReady,
		IsOutOfDisk:      isOutOfDisk,
		CPUCapacity:      cpuCapQuantity.MilliValue(),
		CPUAllocatable:   cpuAllocQuantity.MilliValue(),
		MemCapacityKb:    memCap / bytesToKb,
		MemAllocatableKb: memAlloc / bytesToKb,
		Labels:           node.Labels,
		Annotations:      node.Annotations,
	}
}

func (nw *NodeWatcher) enqueueNodeAddition(key, obj interface{}) {
	node := obj.(*v1.Node)
	if node.Spec.Unschedulable {
		glog.Info("enqueueNodeAddition: received an Unschedulable node", node.Name)
		return
	}
	addedNode := nw.parseNode(node, NodeAdded)
	nw.nodeWorkQueue.Add(key, addedNode)
	glog.Info("enqueueNodeAdition: Added node ", addedNode.Hostname)
}

func (nw *NodeWatcher) enqueueNodeUpdate(key, oldObj, newObj interface{}) {
	// XXX(ionel): enqueueNodeUpdate gets called whenever one of node's timestamp is updated. Figure out solution such that the method is called only when certain fields change.
	oldNode := oldObj.(*v1.Node)
	newNode := newObj.(*v1.Node)
	if oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable {
		if oldNode.Spec.Unschedulable {
			addedNode := nw.parseNode(newNode, NodeAdded)
			nw.nodeWorkQueue.Add(key, addedNode)
			glog.Info("enqueueNodeUpdate: Added node ", addedNode.Hostname)
			return
		}
		// Can not schedule pods on the node any more.
		deletedNode := nw.parseNode(newNode, NodeDeleted)
		nw.nodeWorkQueue.Add(key, deletedNode)
		glog.Info("enqueueNodeUpdate: Deleted node ", deletedNode.Hostname)
		return
	}
	oldIsReady, oldIsOutOfDisk := nw.getReadyAndOutOfDiskConditions(oldNode)
	newIsReady, newIsOutOfDisk := nw.getReadyAndOutOfDiskConditions(newNode)

	if oldIsReady != newIsReady || oldIsOutOfDisk != newIsOutOfDisk {
		if newIsReady && !newIsOutOfDisk {
			addedNode := nw.parseNode(newNode, NodeAdded)
			nw.nodeWorkQueue.Add(key, addedNode)
			glog.Info("enqueueNodeUpdate: Added node ", addedNode.Hostname)
			return
		}
		failedNode := nw.parseNode(newNode, NodeFailed)
		nw.nodeWorkQueue.Add(key, failedNode)
		glog.Info("enqueueNodeUpdate: Failed node ", failedNode.Hostname)
		return
	}
	nodeUpdated := false
	if !reflect.DeepEqual(oldNode.Labels, newNode.Labels) {
		nodeUpdated = true
	}
	if !reflect.DeepEqual(oldNode.Annotations, newNode.Annotations) {
		nodeUpdated = true
	}
	if nodeUpdated {
		updatedNode := nw.parseNode(newNode, NodeUpdated)
		nw.nodeWorkQueue.Add(key, updatedNode)
		glog.Info("enqueueNodeUpdate: Updated node ", updatedNode.Hostname)
	}
}

func (nw *NodeWatcher) enqueueNodeDeletion(key, obj interface{}) {
	node := obj.(*v1.Node)
	if node.Spec.Unschedulable {
		// Poseidon doesn't case about Unschedulable nodes.
		return
	}
	deletedNode := &Node{
		Hostname: node.Name,
		Phase:    NodeDeleted,
	}
	nw.nodeWorkQueue.Add(key, deletedNode)
	glog.Info("enqueueNodeDeletion: Added node ", deletedNode.Hostname)
}

// Run starts node watcher.
func (nw *NodeWatcher) Run(stopCh <-chan struct{}, nWorkers int) {
	defer utilruntime.HandleCrash()

	// The workers can stop when we are done.
	defer nw.nodeWorkQueue.ShutDown()
	defer glog.Info("Shutting down NodeWatcher")
	glog.Info("Geting node updates...")

	go nw.controller.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, nw.controller.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	glog.Info("Starting node watching workers")
	for i := 0; i < nWorkers; i++ {
		go wait.Until(nw.nodeWorker, time.Second, stopCh)
	}

	<-stopCh
	glog.Info("Stopping node watcher")
}

func (nw *NodeWatcher) nodeWorker() {
	for {
		func() {
			key, items, quit := nw.nodeWorkQueue.Get()
			if quit {
				return
			}
			for _, item := range items {
				node := item.(*Node)
				switch node.Phase {
				case NodeAdded:
					NodesCond.L.Lock()
					rtnd := nw.createResourceTopologyForNode(node)
					_, ok := NodeToRTND[node.Hostname]
					if ok {
						glog.Fatalf("Node %s already exists", node.Hostname)
					}
					NodeToRTND[node.Hostname] = rtnd
					ResIDToNode[rtnd.GetResourceDesc().GetUuid()] = node.Hostname
					NodesCond.L.Unlock()
					firmament.NodeAdded(nw.fc, rtnd)
				case NodeDeleted:
					NodesCond.L.Lock()
					rtnd, ok := NodeToRTND[node.Hostname]
					NodesCond.L.Unlock()
					if !ok {
						glog.Fatalf("Node %s does not exist", node.Hostname)
					}
					resID := rtnd.GetResourceDesc().GetUuid()
					firmament.NodeRemoved(nw.fc, &firmament.ResourceUID{ResourceUid: resID})
					NodesCond.L.Lock()
					delete(NodeToRTND, node.Hostname)
					delete(ResIDToNode, resID)
					NodesCond.L.Unlock()
				case NodeFailed:
					NodesCond.L.Lock()
					rtnd, ok := NodeToRTND[node.Hostname]
					NodesCond.L.Unlock()
					if !ok {
						glog.Fatalf("Node %s does not exist", node.Hostname)
					}
					resID := rtnd.GetResourceDesc().GetUuid()
					firmament.NodeFailed(nw.fc, &firmament.ResourceUID{ResourceUid: resID})
					NodesCond.L.Lock()
					nw.cleanResourceStateForNode(rtnd)
					delete(NodeToRTND, node.Hostname)
					delete(ResIDToNode, resID)
					NodesCond.L.Unlock()
				case NodeUpdated:
					NodesCond.L.Lock()
					rtnd, ok := NodeToRTND[node.Hostname]
					NodesCond.L.Unlock()
					if !ok {
						glog.Fatalf("Node %s does not exist", node.Hostname)
					}
					firmament.NodeUpdated(nw.fc, rtnd)
				default:
					glog.Fatalf("Unexpected node %s phase %s", node.Hostname, node.Phase)
				}
			}
			defer nw.nodeWorkQueue.Done(key)
		}()
	}
}

func (nw *NodeWatcher) cleanResourceStateForNode(rtnd *firmament.ResourceTopologyNodeDescriptor) {
	delete(ResIDToNode, rtnd.GetResourceDesc().GetUuid())
	for _, childRTND := range rtnd.GetChildren() {
		nw.cleanResourceStateForNode(childRTND)
	}
}

func (nw *NodeWatcher) createResourceTopologyForNode(node *Node) *firmament.ResourceTopologyNodeDescriptor {
	resUUID := nw.generateResourceID(node.Hostname)
	rtnd := &firmament.ResourceTopologyNodeDescriptor{
		ResourceDesc: &firmament.ResourceDescriptor{
			Uuid:         resUUID,
			Type:         firmament.ResourceDescriptor_RESOURCE_MACHINE,
			State:        firmament.ResourceDescriptor_RESOURCE_IDLE,
			FriendlyName: node.Hostname,
			ResourceCapacity: &firmament.ResourceVector{
				RamCap:   uint64(node.MemCapacityKb),
				CpuCores: float32(node.CPUCapacity),
			},
		},
	}
	ResIDToNode[resUUID] = node.Hostname
	// TODO(ionel) Add annotations.
	// Add labels.
	for label, value := range node.Labels {
		rtnd.ResourceDesc.Labels = append(rtnd.ResourceDesc.Labels,
			&firmament.Label{
				Key:   label,
				Value: value,
			})
	}
	// TODO(ionel): In the future, we want to get real node topology.
	// We currently only create a PU per machine because Heapster doesn't
	// provide per PU/core statistics.
	friendlyName := node.Hostname + "_PU #0"
	puUUID := nw.generateResourceID(friendlyName)
	puRtnd := &firmament.ResourceTopologyNodeDescriptor{
		ResourceDesc: &firmament.ResourceDescriptor{
			Uuid:         puUUID,
			Type:         firmament.ResourceDescriptor_RESOURCE_PU,
			State:        firmament.ResourceDescriptor_RESOURCE_IDLE,
			FriendlyName: friendlyName,
			Labels:       rtnd.ResourceDesc.Labels,
			ResourceCapacity: &firmament.ResourceVector{
				RamCap:   uint64(node.MemCapacityKb),
				CpuCores: float32(node.CPUCapacity),
			},
		},
		ParentId: resUUID,
	}
	rtnd.Children = append(rtnd.Children, puRtnd)
	ResIDToNode[puUUID] = node.Hostname

	return rtnd
}

func (nw *NodeWatcher) generateResourceID(seed string) string {
	return GenerateUUID(seed)
}
