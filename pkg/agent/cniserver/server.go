// Copyright 2019 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cniserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/vmware-tanzu/antrea/pkg/agent"
	"github.com/vmware-tanzu/antrea/pkg/agent/cniserver/ipam"
	"github.com/vmware-tanzu/antrea/pkg/agent/openflow"
	"github.com/vmware-tanzu/antrea/pkg/agent/util"
	cnipb "github.com/vmware-tanzu/antrea/pkg/apis/cni/v1beta1"
	"github.com/vmware-tanzu/antrea/pkg/cni"
	"github.com/vmware-tanzu/antrea/pkg/ovs/ovsconfig"
)

// containerAccessArbitrator is used to ensure that concurrent goroutines cannot perfom operations
// on the same containerID. Other parts of the code make this assumption (in particular the
// InstallPodFlows / UninstallPodFlows methods of the OpenFlow client, which are invoked
// respectively by CmdAdd and CmdDel). The idea is to simply the locking requirements for the rest
// of the code by ensuring that all the requests for a given container are serialized.
type containerAccessArbitrator struct {
	mutex            sync.Mutex
	cond             *sync.Cond
	busyContainerIDs map[string]bool // used as a set of container IDs
}

func newContainerAccessArbitrator() *containerAccessArbitrator {
	arbitrator := &containerAccessArbitrator{
		busyContainerIDs: make(map[string]bool),
	}
	arbitrator.cond = sync.NewCond(&arbitrator.mutex)
	return arbitrator
}

// lockContainer prevents other goroutines from accessing containerID. If containerID is already
// locked by another goroutine, this function will block until the container is available. Every
// call to lockContainer must be followed by a call to unlockContainer on the same containerID.
func (arbitrator *containerAccessArbitrator) lockContainer(containerID string) {
	arbitrator.cond.L.Lock()
	defer arbitrator.cond.L.Unlock()
	for {
		_, ok := arbitrator.busyContainerIDs[containerID]
		if !ok {
			break
		}
		arbitrator.cond.Wait()
	}
	arbitrator.busyContainerIDs[containerID] = true
}

// unlockContainer releases access to containerID.
func (arbitrator *containerAccessArbitrator) unlockContainer(containerID string) {
	arbitrator.cond.L.Lock()
	defer arbitrator.cond.L.Unlock()
	delete(arbitrator.busyContainerIDs, containerID)
	arbitrator.cond.Broadcast()
}

type CNIServer struct {
	cniSocket            string
	supportedCNIVersions map[string]bool
	serverVersion        string
	nodeConfig           *agent.NodeConfig
	ovsBridgeClient      ovsconfig.OVSBridgeClient
	ifaceStore           agent.InterfaceStore
	hostProcPathPrefix   string
	ofClient             openflow.Client
	defaultMTU           int
	kubeClient           clientset.Interface
	containerAccess      *containerAccessArbitrator
}

const (
	supportedCNIVersions = "0.1.0,0.2.0,0.3.0,0.3.1,0.4.0"
)

var supportedCNIVersionSet map[string]bool

type NetworkConfig struct {
	CNIVersion string          `json:"cniVersion,omitempty"`
	Name       string          `json:"name,omitempty"`
	Type       string          `json:"type,omitempty"`
	MTU        int             `json:"mtu,omitempty"`
	DNS        types.DNS       `json:"dns"`
	IPAM       ipam.IPAMConfig `json:"ipam,omitempty"`

	RawPrevResult map[string]interface{} `json:"prevResult,omitempty"`
	PrevResult    types.Result           `json:"-"`
}

type CNIConfig struct {
	*NetworkConfig
	*cnipb.CniCmdArgs
	*k8sArgs
}

// updateResultIfaceConfig processes the result from the IPAM plugin and does the following:
//   * updates the IP configuration for each assigned IP address: this includes computing the
//     gateway (if missing) based on the subnet and setting the interface pointer to the container
//     interface
//   * if there is no default route, add one using the provided default gateway
func updateResultIfaceConfig(result *current.Result, defaultV4Gateway net.IP) {
	for _, ipc := range result.IPs {
		// result.Interfaces[0] is host interface, and result.Interfaces[1] is container interface
		ipc.Interface = current.Int(1)
		if ipc.Gateway == nil {
			ipn := ipc.Address
			netID := ipn.IP.Mask(ipn.Mask)
			ipc.Gateway = ip.NextIP(netID)
		}
	}

	foundDefaultRoute := false
	defaultRouteDst := "0.0.0.0/0"
	if result.Routes != nil {
		for _, route := range result.Routes {
			if route.Dst.String() == defaultRouteDst {
				foundDefaultRoute = true
				break
			}
		}
	} else {
		result.Routes = []*types.Route{}
	}
	if !foundDefaultRoute {
		_, defaultRouteDstNet, _ := net.ParseCIDR(defaultRouteDst)
		result.Routes = append(result.Routes, &types.Route{Dst: *defaultRouteDstNet, GW: defaultV4Gateway})
	}
}

func (s *CNIServer) loadNetworkConfig(request *cnipb.CniCmdRequest) (*CNIConfig, error) {
	cniConfig := &CNIConfig{}
	cniConfig.CniCmdArgs = request.CniArgs
	if err := json.Unmarshal(request.CniArgs.NetworkConfiguration, cniConfig); err != nil {
		return cniConfig, err
	}
	cniConfig.k8sArgs = &k8sArgs{}
	if err := types.LoadArgs(request.CniArgs.Args, cniConfig.k8sArgs); err != nil {
		return cniConfig, err
	}
	s.updateLocalIPAMSubnet(cniConfig)
	if cniConfig.MTU == 0 {
		cniConfig.MTU = s.defaultMTU
	}
	klog.Infof("Load network configurations: %v", cniConfig)
	return cniConfig, nil
}

func (s *CNIServer) isCNIVersionSupported(reqVersion string) bool {
	_, exist := s.supportedCNIVersions[reqVersion]
	return exist
}

func (s *CNIServer) checkRequestMessage(request *cnipb.CniCmdRequest) (*CNIConfig, *cnipb.CniCmdResponse) {
	cniConfig, err := s.loadNetworkConfig(request)
	if err != nil {
		klog.Errorf("Failed to parse network configuration: %v", err)
		return nil, s.decodingFailureResponse("network config")
	}
	cniVersion := cniConfig.CNIVersion
	// Check if CNI version in the request is supported
	if !s.isCNIVersionSupported(cniVersion) {
		klog.Errorf(fmt.Sprintf("Unsupported CNI version [%s], supported CNI versions [%s]", cniVersion, supportedCNIVersions))
		return cniConfig, s.incompatibleCniVersionResponse(cniVersion)
	}
	// Find IPAM Service according configuration
	ipamType := cniConfig.IPAM.Type
	isValid := ipam.IsIPAMTypeValid(ipamType)
	if !isValid {
		klog.Errorf("Unsupported IPAM type %s", ipamType)
		return cniConfig, s.unsupportedFieldResponse("ipam/type", ipamType)
	}
	return cniConfig, nil
}

func (s *CNIServer) updateLocalIPAMSubnet(cniConfig *CNIConfig) {
	cniConfig.NetworkConfig.IPAM.Gateway = s.nodeConfig.Gateway.IP.String()
	cniConfig.NetworkConfig.IPAM.Subnet = s.nodeConfig.PodCIDR.String()
	cniConfig.NetworkConfiguration, _ = json.Marshal(cniConfig.NetworkConfig)
}

func (s *CNIServer) generateCNIErrorResponse(cniErrorCode cnipb.ErrorCode, cniErrorMsg string) *cnipb.CniCmdResponse {
	return &cnipb.CniCmdResponse{
		Error: &cnipb.Error{
			Code:    cniErrorCode,
			Message: cniErrorMsg,
		},
	}
}

func (s *CNIServer) decodingFailureResponse(what string) *cnipb.CniCmdResponse {
	return s.generateCNIErrorResponse(
		cnipb.ErrorCode_DECODING_FAILURE,
		fmt.Sprintf("Failed to decode %s", what),
	)
}

func (s *CNIServer) incompatibleCniVersionResponse(cniVersion string) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_INCOMPATIBLE_CNI_VERSION
	cniErrorMsg := fmt.Sprintf("Unsupported CNI version [%s], supported versions [%s]", cniVersion, supportedCNIVersions)
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) unsupportedFieldResponse(key string, value interface{}) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_UNSUPPORTED_FIELD
	cniErrorMsg := fmt.Sprintf("Network configuration does not support key %s and value %v", key, value)
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) unknownContainerResponse(containerID string) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_UNKNOWN_CONTAINER
	cniErrorMsg := fmt.Sprintf("Container id  %s is unknown or non-existent", containerID)
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) tryAgainLaterResponse() *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_TRY_AGAIN_LATER
	cniErrorMsg := "Server is busy, please retry later"
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) ipamFailureResponse(err error) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_IPAM_FAILURE
	cniErrorMsg := err.Error()
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) configInterfaceFailureResponse(err error) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_CONFIG_INTERFACE_FAILURE
	cniErrorMsg := err.Error()
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) checkInterfaceFailureResponse(err error) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_CHECK_INTERFACE_FAILURE
	cniErrorMsg := err.Error()
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) invalidNetworkConfigResponse(msg string) *cnipb.CniCmdResponse {
	return s.generateCNIErrorResponse(
		cnipb.ErrorCode_INVALID_NETWORK_CONFIG,
		msg,
	)
}

func buildVersionSet(versions string) map[string]bool {
	versionSet := make(map[string]bool)
	for _, ver := range strings.Split(versions, ",") {
		versionSet[strings.Trim(ver, " ")] = true
	}
	return versionSet
}

func (s *CNIServer) parsePrevResultFromRequest(networkConfig *NetworkConfig) (*current.Result, *cnipb.CniCmdResponse) {
	if networkConfig.PrevResult == nil && networkConfig.RawPrevResult == nil {
		klog.Errorf("Previous network configuration not specified")
		return nil, s.unsupportedFieldResponse("prevResult", "")
	}

	if err := parsePrevResult(networkConfig); err != nil {
		klog.Errorf("Failed to parse previous network configuration")
		return nil, s.decodingFailureResponse("prevResult")
	}
	// Convert whatever the IPAM result was into the current Result type (for the current CNI
	// version)
	prevResult, err := current.NewResultFromResult(networkConfig.PrevResult)
	if err != nil {
		klog.Errorf("Failed to construct prevResult using previous network configuration")
		return nil, s.unsupportedFieldResponse("prevResult", networkConfig.PrevResult)
	}
	return prevResult, nil
}

// When running in a container, the host's /proc directory is mounted under s.hostProcPathPrefix, so
// we need to prepend s.hostProcPathPrefix to the network namespace path provided by the cni. When
// running as a simple process, s.hostProcPathPrefix will be empty.
func (s *CNIServer) hostNetNsPath(netNS string) string {
	if netNS == "" {
		return ""
	}
	return s.hostProcPathPrefix + netNS
}

func (s *CNIServer) validatePrevResult(cfgArgs *cnipb.CniCmdArgs, k8sCNIArgs *k8sArgs, prevResult *current.Result) (*cnipb.CniCmdResponse, error) {
	var containerIntf, hostIntf *current.Interface
	hostVethName := util.GenerateContainerInterfaceName(string(k8sCNIArgs.K8S_POD_NAME), string(k8sCNIArgs.K8S_POD_NAMESPACE))
	containerID := cfgArgs.ContainerId
	netNS := s.hostNetNsPath(cfgArgs.Netns)

	// Find interfaces from previous configuration
	for _, intf := range prevResult.Interfaces {
		switch intf.Name {
		case cfgArgs.Ifname:
			containerIntf = intf
		case hostVethName:
			hostIntf = intf
		default:
			klog.Errorf("Unknown interface name %s", intf.Name)
		}
	}
	if containerIntf == nil {
		klog.Errorf("Failed to find interface %s of container %s", cfgArgs.Ifname, containerID)
		return s.invalidNetworkConfigResponse("prevResult does not match network configuration"), nil
	}
	if hostIntf == nil {
		klog.Errorf("Failed to find host interface peer %s for container %s", hostVethName, containerID)
		return s.invalidNetworkConfigResponse("prevResult does not match network configuration"), nil
	}

	if err := checkInterfaces(s.ifaceStore, containerID, netNS, containerIntf, hostIntf, hostVethName, prevResult); err != nil {
		return s.checkInterfaceFailureResponse(err), nil
	}

	return &cnipb.CniCmdResponse{
		CniResult: []byte(""),
	}, nil
}

func (s *CNIServer) CmdAdd(ctx context.Context, request *cnipb.CniCmdRequest) (
	*cnipb.CniCmdResponse, error) {
	klog.Infof("Receive CmdAdd request %v", request)
	cniConfig, response := s.checkRequestMessage(request)
	if response != nil {
		return response, nil
	}
	cniVersion := cniConfig.CNIVersion
	result := &current.Result{CNIVersion: cniVersion}
	netNS := s.hostNetNsPath(cniConfig.Netns)

	success := false
	defer func() {
		// Rollback to delete configurations once ADD is failure.
		if !success {
			klog.Warningf("CmdAdd has failed, and try to rollback")
			if _, err := s.CmdDel(ctx, request); err != nil {
				klog.Warningf("Failed to rollback after CNI add failure: %v", err)
			}
		}
	}()

	s.containerAccess.lockContainer(cniConfig.ContainerId)
	defer s.containerAccess.unlockContainer(cniConfig.ContainerId)

	// Request IP Address from IPAM driver
	ipamResult, err := ipam.ExecIPAMAdd(cniConfig.CniCmdArgs, cniConfig.IPAM.Type)
	if err != nil {
		klog.Errorf("Failed to add ip addresses from IPAM driver: %v", err)
		return s.ipamFailureResponse(err), nil
	}
	klog.Infof("Added ip addresses from IPAM driver, %v", ipamResult)
	result.IPs = ipamResult.IPs
	result.Routes = ipamResult.Routes
	// Ensure interface gateway setting and mapping relations between result.Interfaces and result.IPs
	updateResultIfaceConfig(result, s.nodeConfig.Gateway.IP)
	// Setup pod interfaces and connect to ovs bridge
	podName := string(cniConfig.K8S_POD_NAME)
	podNamespace := string(cniConfig.K8S_POD_NAMESPACE)
	if err = configureInterface(
		s.ovsBridgeClient,
		s.ofClient,
		s.nodeConfig.Gateway,
		s.ifaceStore,
		podName,
		podNamespace,
		cniConfig.ContainerId,
		netNS,
		cniConfig.Ifname,
		cniConfig.MTU,
		result,
	); err != nil {
		klog.Errorf("Failed to configure container %s interface: %v", cniConfig.ContainerId, err)
		return s.configInterfaceFailureResponse(err), nil
	}
	result.DNS = cniConfig.DNS
	var resultBytes bytes.Buffer
	result.PrintTo(&resultBytes)
	klog.Infof("CmdAdd request success")
	// mark success as true to avoid rollback
	success = true
	return &cnipb.CniCmdResponse{
		CniResult: resultBytes.Bytes(),
	}, nil
}

func (s *CNIServer) CmdDel(ctx context.Context, request *cnipb.CniCmdRequest) (
	*cnipb.CniCmdResponse, error) {
	klog.Infof("Receive CmdDel request %v", request)
	cniConfig, response := s.checkRequestMessage(request)
	if response != nil {
		return response, nil
	}

	s.containerAccess.lockContainer(cniConfig.ContainerId)
	defer s.containerAccess.unlockContainer(cniConfig.ContainerId)

	// Release IP to IPAM driver
	if err := ipam.ExecIPAMDelete(cniConfig.CniCmdArgs, cniConfig.IPAM.Type); err != nil {
		klog.Errorf("Failed to delete IP addresses by IPAM driver: %v", err)
		return s.ipamFailureResponse(err), nil
	}
	klog.Info("Deleted IP addresses by IPAM driver")
	// Remove host interface and OVS configuration
	podName := string(cniConfig.K8S_POD_NAME)
	podNamespace := string(cniConfig.K8S_POD_NAMESPACE)
	netNS := s.hostNetNsPath(cniConfig.Netns)
	if err := removeInterfaces(s.ovsBridgeClient, s.ofClient, s.ifaceStore, podName, podNamespace, cniConfig.ContainerId, netNS, cniConfig.Ifname); err != nil {
		klog.Errorf("Failed to remove container %s interface configuration: %v", cniConfig.ContainerId, err)
		return s.configInterfaceFailureResponse(err), nil
	}
	return &cnipb.CniCmdResponse{
		CniResult: []byte(""),
	}, nil
}

func (s *CNIServer) CmdCheck(ctx context.Context, request *cnipb.CniCmdRequest) (
	*cnipb.CniCmdResponse, error) {
	klog.Infof("Receive CmdCheck request %v", request)
	cniConfig, response := s.checkRequestMessage(request)
	if response != nil {
		return response, nil
	}

	s.containerAccess.lockContainer(cniConfig.ContainerId)
	defer s.containerAccess.unlockContainer(cniConfig.ContainerId)

	if err := ipam.ExecIPAMCheck(cniConfig.CniCmdArgs, cniConfig.IPAM.Type); err != nil {
		klog.Errorf("Failed to check IPAM configuration: %v", err)
		return s.ipamFailureResponse(err), nil
	}

	cniVersion := cniConfig.CNIVersion
	if valid, _ := version.GreaterThanOrEqualTo(cniVersion, "0.4.0"); valid {
		if prevResult, response := s.parsePrevResultFromRequest(cniConfig.NetworkConfig); response != nil {
			return response, nil
		} else if response, err := s.validatePrevResult(cniConfig.CniCmdArgs, cniConfig.k8sArgs, prevResult); err != nil {
			return response, err
		}
	}
	klog.Info("Succeed to check network configuration")
	return &cnipb.CniCmdResponse{
		CniResult: []byte(""),
	}, nil
}

func New(
	cniSocket, hostProcPathPrefix string,
	defaultMTU int,
	nodeConfig *agent.NodeConfig,
	ovsBridgeClient ovsconfig.OVSBridgeClient,
	ofClient openflow.Client,
	ifaceStore agent.InterfaceStore,
	kubeClient clientset.Interface,
) *CNIServer {
	return &CNIServer{
		cniSocket:            cniSocket,
		supportedCNIVersions: supportedCNIVersionSet,
		serverVersion:        cni.AntreaCNIVersion,
		nodeConfig:           nodeConfig,
		ovsBridgeClient:      ovsBridgeClient,
		ifaceStore:           ifaceStore,
		hostProcPathPrefix:   hostProcPathPrefix,
		ofClient:             ofClient,
		defaultMTU:           defaultMTU,
		kubeClient:           kubeClient,
		containerAccess:      newContainerAccessArbitrator(),
	}
}

func (s *CNIServer) Initialize() error {
	if err := s.reconcile(); err != nil {
		return fmt.Errorf("error during initial reconciliation for CNI server: %v", err)
	}
	return nil
}

func (s *CNIServer) Run(stopCh <-chan struct{}) {
	klog.Info("Starting CNI server")
	defer klog.Info("Shutting down CNI server")

	// remove before bind to avoid "address already in use" errors
	os.Remove(s.cniSocket)
	listener, err := net.Listen("unix", s.cniSocket)
	if err != nil {
		klog.Errorf("Failed to bind on %s: %v", s.cniSocket, err)
		os.Exit(1)
	}
	rpcServer := grpc.NewServer()

	cnipb.RegisterCniServer(rpcServer, s)
	klog.Info("CNI server is listening ...")
	go func() {
		if err := rpcServer.Serve(listener); err != nil {
			klog.Errorf("Failed to serve connections: %v", err)
		}
	}()
	<-stopCh
}

// reconcile performs startup reconciliation for the CNI server. The CNI server is in charge of
// installing Pod flows, so as part of this reconciliation process we retrieve the Pod list from the
// K8s apiserver and replay the necessary flows.
func (s *CNIServer) reconcile() error {
	klog.Infof("Reconciliation for CNI server")
	pods, err := s.kubeClient.CoreV1().Pods("").List(metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + s.nodeConfig.Name,
	})
	if err != nil {
		return fmt.Errorf("failed to list Pods running on Node %s: %v", s.nodeConfig.Name, err)
	}

	// desiredInterfaces is the exact set of interfaces that should be present, based on the
	// current list of Pods.
	desiredInterfaces := make(map[string]bool)
	// knownInterfaces is the list of interfaces currently in the local cache.
	knownInterfaces := s.ifaceStore.GetInterfaceIDs()

	for _, pod := range pods.Items {
		// Skip Pods for which we are not in charge of the networking.
		if pod.Spec.HostNetwork {
			continue
		}

		// We rely on the interface cache / store - which is initialized from the persistent
		// OVSDB - to map the Pod to its interface configuration. The interface
		// configuration includes the parameters we need to replay the flows.
		containerConfig, found := s.ifaceStore.GetContainerInterface(pod.Name, pod.Namespace)
		if !found {
			// This should not happen since OVSDB is persisted on the Node.
			// TODO: is there anything else we should be doing? Assuming that the Pod's
			// interface still exists, we can repair the interface store since we can
			// retrieve the name of the host interface for the Pod by calling
			// GenerateContainerInterfaceName. One thing we would not be able to
			// retrieve is the container ID which is part of the container configuration
			// we store in the cache, but this ID is not used for anything at the
			// moment. However, if the interface does not exist, there is nothing we can
			// do since we do not have the original CNI parameters.
			klog.Warningf("Interface for Pod %s/%s not found in the interface store", pod.Namespace, pod.Name)
			continue
		}
		klog.V(4).Infof("Syncing interface %s for Pod %s/%s", containerConfig.IfaceName, pod.Namespace, pod.Name)
		if err := s.ofClient.InstallPodFlows(
			containerConfig.IfaceName,
			containerConfig.IP,
			containerConfig.MAC,
			s.nodeConfig.Gateway.MAC,
			uint32(containerConfig.OFPort),
		); err != nil {
			klog.Errorf("Error when re-installing flows for Pod %s/%s", pod.Namespace, pod.Name)
			continue
		}
		desiredInterfaces[containerConfig.IfaceName] = true
	}

	for _, ifaceID := range knownInterfaces {
		if _, found := desiredInterfaces[ifaceID]; found {
			// this interface matches an existing Pod.
			continue
		}
		// clean-up and delete interface
		containerConfig, found := s.ifaceStore.GetInterface(ifaceID)
		if !found {
			// should not happen, nothing should have concurrent access to the interface
			// store.
			klog.Errorf("Interface %s can no longer be found in the interface store", ifaceID)
			continue
		}
		if containerConfig.PodName == "" {
			// not a container interface, skipping.
			continue
		}
		klog.V(4).Infof("Deleting interface %s", ifaceID)
		// ignore error, removeInterfaces already log them
		_ = removeInterfaces(
			s.ovsBridgeClient,
			s.ofClient,
			s.ifaceStore,
			containerConfig.PodName,
			containerConfig.PodNamespace,
			containerConfig.ID,
			"",
			"",
		)
		// interface should no longer be in store after the call to removeInterfaces
	}
	return nil
}

func init() {
	supportedCNIVersionSet = buildVersionSet(supportedCNIVersions)
}
