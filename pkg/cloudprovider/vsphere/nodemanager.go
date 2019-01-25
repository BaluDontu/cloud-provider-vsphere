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

package vsphere

import (
	"context"
	"errors"
	"net"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	clientv1 "k8s.io/client-go/listers/core/v1"
	pb "k8s.io/cloud-provider-vsphere/pkg/cloudprovider/vsphere/proto"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	"github.com/vmware/govmomi/vim25/mo"

	cm "k8s.io/cloud-provider-vsphere/pkg/common/connectionmanager"
)

type FindVM int

const (
	FindVMByUUID FindVM = iota // 0
	FindVMByName               // 1

	// Error Messages
	VCenterNotFoundErrMsg    = "vCenter not found"
	DatacenterNotFoundErrMsg = "Datacenter not found"
	VMNotFoundErrMsg         = "VM not found"
)

// Error constants
var (
	ErrVCenterNotFound    = errors.New(VCenterNotFoundErrMsg)
	ErrDatacenterNotFound = errors.New(DatacenterNotFoundErrMsg)
	ErrVMNotFound         = errors.New(VMNotFoundErrMsg)
)

func newNodeManager(cm *cm.ConnectionManager, lister clientv1.NodeLister) *NodeManager {
	return &NodeManager{
		nodeNameMap:       make(map[string]*NodeInfo),
		nodeUUIDMap:       make(map[string]*NodeInfo),
		nodeRegUUIDMap:    make(map[string]*v1.Node),
		vcList:            make(map[string]*VCenterInfo),
		connectionManager: cm,
		nodeLister:        lister,
	}
}

// RegisterNode - Handler when node is removed from k8s cluster.
func (nm *NodeManager) RegisterNode(node *v1.Node) {
	glog.V(4).Info("RegisterNode ENTER: ", node.Name)
	nm.addNode(node)
	nm.DiscoverNode(ConvertK8sUUIDtoNormal(node.Status.NodeInfo.SystemUUID), FindVMByUUID)
	glog.V(4).Info("RegisterNode LEAVE: ", node.Name)
}

// UnregisterNode - Handler when node is removed from k8s cluster.
func (nm *NodeManager) UnregisterNode(node *v1.Node) {
	glog.V(4).Info("UnregisterNode ENTER: ", node.Name)
	nm.removeNode(node)
	glog.V(4).Info("UnregisterNode LEAVE: ", node.Name)
}

func (nm *NodeManager) addNodeInfo(node *NodeInfo) {
	nm.nodeInfoLock.Lock()
	glog.V(4).Info("addNodeInfo NodeName: ", node.NodeName, ", UUID: ", node.UUID)
	nm.nodeNameMap[node.NodeName] = node
	nm.nodeUUIDMap[node.UUID] = node
	nm.AddNodeInfoToVCList(node.vcServer, node.dataCenter.Name(), node)
	nm.nodeInfoLock.Unlock()
}

func (nm *NodeManager) addNode(node *v1.Node) {
	nm.nodeRegInfoLock.Lock()
	uuid := ConvertK8sUUIDtoNormal(node.Status.NodeInfo.SystemUUID)
	glog.V(4).Info("addNode NodeName: ", node.GetName(), ", UID: ", uuid)
	nm.nodeRegUUIDMap[uuid] = node
	nm.nodeRegInfoLock.Unlock()
}

func (nm *NodeManager) removeNode(node *v1.Node) {
	nm.nodeRegInfoLock.Lock()
	uuid := ConvertK8sUUIDtoNormal(node.Status.NodeInfo.SystemUUID)
	glog.V(4).Info("removeNode NodeName: ", node.GetName(), ", UID: ", uuid)
	delete(nm.nodeRegUUIDMap, uuid)
	nm.nodeRegInfoLock.Unlock()
}

func (nm *NodeManager) DiscoverNode(nodeID string, searchBy FindVM) error {
	ctx := context.Background()

	vmDI, err := nm.connectionManager.WhichVCandDCByNodeId(ctx, nodeID, cm.FindVM(searchBy))
	if err != nil {
		glog.Errorf("WhichVCandDCByNodeId failed. Err: %v", err)
	}

	var oVM mo.VirtualMachine
	err = vmDI.VM.Properties(ctx, vmDI.VM.Reference(), []string{"guest"}, &oVM)
	if err != nil {
		glog.Errorf("Error collecting properties for vm=%+v in vc=%s and datacenter=%s: %v",
			vmDI.VM, vmDI.VcServer, vmDI.DataCenter.Name(), err)
		return err
	}

	addrs := []v1.NodeAddress{}
	for _, v := range oVM.Guest.Net {
		for _, ip := range v.IpAddress {
			if net.ParseIP(ip).To4() != nil {
				v1helper.AddToNodeAddresses(&addrs,
					v1.NodeAddress{
						Type:    v1.NodeExternalIP,
						Address: ip,
					}, v1.NodeAddress{
						Type:    v1.NodeInternalIP,
						Address: ip,
					}, v1.NodeAddress{
						Type:    v1.NodeHostName,
						Address: oVM.Guest.HostName,
					},
				)
			}
		}
	}

	glog.V(2).Infof("Found node %s as vm=%+v in vc=%s and datacenter=%s",
		nodeID, vmDI.VM, vmDI.VcServer, vmDI.DataCenter.Name())
	glog.V(2).Info("Hostname: ", oVM.Guest.HostName, " UUID: ", oVM.Summary.Config.Uuid)

	nodeInfo := &NodeInfo{dataCenter: vmDI.DataCenter, vm: vmDI.VM, vcServer: vmDI.VcServer,
		UUID: vmDI.UUID, NodeName: vmDI.NodeName, NodeAddresses: addrs}
	nm.addNodeInfo(nodeInfo)

	return nil
}

// ExportNodes transforms the NodeInfoList to []*pb.Node
func (nm *NodeManager) ExportNodes(vcenter string, datacenter string, nodeList *[]*pb.Node) error {
	nm.nodeInfoLock.Lock()

	if vcenter != "" && datacenter != "" {
		dc, err := nm.FindDatacenterInfoInVCList(vcenter, datacenter)
		if err != nil {
			nm.nodeInfoLock.Unlock()
			return err
		}

		nm.datacenterToNodeList(dc.vmList, nodeList)
	} else if vcenter != "" {
		if nm.vcList[vcenter] == nil {
			nm.nodeInfoLock.Unlock()
			return ErrVCenterNotFound
		}

		for _, dc := range nm.vcList[vcenter].dcList {
			nm.datacenterToNodeList(dc.vmList, nodeList)
		}
	} else {
		for _, vc := range nm.vcList {
			for _, dc := range vc.dcList {
				nm.datacenterToNodeList(dc.vmList, nodeList)
			}
		}
	}

	nm.nodeInfoLock.Unlock()

	return nil
}

func (nm *NodeManager) datacenterToNodeList(vmList map[string]*NodeInfo, nodeList *[]*pb.Node) {
	for _, node := range vmList {
		pbNode := &pb.Node{
			Vcenter:    node.vcServer,
			Datacenter: node.dataCenter.Name(),
			Name:       node.NodeName,
			Dnsnames:   make([]string, 0),
			Addresses:  make([]string, 0),
			Uuid:       node.UUID,
		}
		for _, address := range node.NodeAddresses {
			switch address.Type {
			case v1.NodeExternalIP:
				pbNode.Addresses = append(pbNode.Addresses, address.Address)
			case v1.NodeHostName:
				pbNode.Dnsnames = append(pbNode.Dnsnames, address.Address)
			default:
				glog.Warning("Unknown/unsupported address type:", address.Type)
			}
		}
		*nodeList = append(*nodeList, pbNode)
	}
}

// AddNodeInfoToVCList creates a relational mapping from VC -> DC -> VM/Node
func (nm *NodeManager) AddNodeInfoToVCList(vcenter string, datacenter string, node *NodeInfo) {
	if nm.vcList[vcenter] == nil {
		nm.vcList[vcenter] = &VCenterInfo{
			address: vcenter,
			dcList:  make(map[string]*DatacenterInfo),
		}
	}
	vc := nm.vcList[vcenter]

	if vc.dcList[datacenter] == nil {
		vc.dcList[datacenter] = &DatacenterInfo{
			name:   datacenter,
			vmList: make(map[string]*NodeInfo),
		}
	}
	dc := vc.dcList[datacenter]

	dc.vmList[node.UUID] = node
}

// FindDatacenterInfoInVCList retrieves the DatacenterInfo from the tree
func (nm *NodeManager) FindDatacenterInfoInVCList(vcenter string, datacenter string) (*DatacenterInfo, error) {
	vc := nm.vcList[vcenter]
	if vc == nil {
		return nil, ErrVCenterNotFound
	}

	dc := vc.dcList[datacenter]
	if dc == nil {
		return nil, ErrDatacenterNotFound
	}

	return dc, nil
}

// FindNodeInfoInVCList retrieves the NodeInfo from the tree
func (nm *NodeManager) FindNodeInfoInVCList(vcenter string, datacenter string, UUID string) (*NodeInfo, error) {
	dc, err := nm.FindDatacenterInfoInVCList(vcenter, datacenter)
	if err != nil {
		return nil, err
	}

	vm := dc.vmList[UUID]
	if vm == nil {
		return nil, ErrVMNotFound
	}

	return vm, nil
}
