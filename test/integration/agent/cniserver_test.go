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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/allocator"
	mock "github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	k8sFake "k8s.io/client-go/kubernetes/fake"

	"github.com/vmware-tanzu/antrea/pkg/agent"
	"github.com/vmware-tanzu/antrea/pkg/agent/cniserver"
	"github.com/vmware-tanzu/antrea/pkg/agent/cniserver/ipam"
	ipamtest "github.com/vmware-tanzu/antrea/pkg/agent/cniserver/ipam/testing"
	cniservertest "github.com/vmware-tanzu/antrea/pkg/agent/cniserver/testing"
	openflowtest "github.com/vmware-tanzu/antrea/pkg/agent/openflow/testing"
	"github.com/vmware-tanzu/antrea/pkg/agent/util"
	cnimsg "github.com/vmware-tanzu/antrea/pkg/apis/cni/v1beta1"
	"github.com/vmware-tanzu/antrea/pkg/ovs/ovsconfig"
	ovsconfigtest "github.com/vmware-tanzu/antrea/pkg/ovs/ovsconfig/testing"
)

const (
	IFNAME               = "eth0"
	CONTAINERID          = "dummy-0"
	testSock             = "/tmp/test.sock"
	testPod              = "test-1"
	testPodNamespace     = "t1"
	testPodInfraContaner = "test-111111"
	bridge               = "br0"
)

const (
	netConfStr = `
	"cniVersion": "%s",
	"name": "testConfig",
	"type": "antrea"`

	vlan = `,
	"vlan": %d`

	netDefault = `,
	"isDefaultGateway": true`

	ipamStartStr = `,
    "ipam": {
        "type":    "mock"`

	ipamDataDirStr = `,
        "dataDir": "%s"`

	// Single subnet configuration (legacy)
	subnetConfStr = `,
        "subnet":  "%s"`
	gatewayConfStr = `,
        "gateway": "%s"`

	// Ranges (multiple subnets) configuration
	rangesStartStr = `,
        "ranges": [`
	rangeSubnetConfStr = `
            [{
                "subnet":  "%s"
            }]`
	rangeSubnetGatewayConfStr = `
            [{
                "subnet":  "%s",
                "gateway": "%s"
            }]`
	rangesEndStr = `
        ]`

	ipamEndStr = `
    }`
)

var ipamMock *ipamtest.MockIPAMDriver
var ovsServiceMock *ovsconfigtest.MockOVSBridgeClient
var ofServiceMock *openflowtest.MockClient
var testNodeConfig *agent.NodeConfig

type Net struct {
	Name          string                 `json:"name"`
	CNIVersion    string                 `json:"cniVersion"`
	Type          string                 `json:"type,omitempty"`
	BrName        string                 `json:"ovsBinding"`
	IPAM          *allocator.IPAMConfig  `json:"ipam"`
	DNS           types.DNS              `json:"dns"`
	RawPrevResult map[string]interface{} `json:"prevResult,omitempty"`
	PrevResult    current.Result         `json:"-"`
}

// Range definition for each entry in the ranges list
type rangeInfo struct {
	subnet  string
	gateway string
}

type testCase struct {
	t               *testing.T
	name            string
	cniVersion      string      // CNI Version
	subnet          string      // Single subnet config: Subnet CIDR
	gateway         string      // Single subnet config: Gateway
	ranges          []rangeInfo // Ranges list (multiple subnets config)
	expGatewayCIDRs []string    // Expected gateway addresses in CIDR form
	addresses       []string
	routes          []string
	dns             []string
}

func (tc testCase) netConfJSON(dataDir string) string {
	conf := fmt.Sprintf(netConfStr, tc.cniVersion)
	conf += netDefault
	if tc.subnet != "" || tc.ranges != nil {
		conf += ipamStartStr
		if dataDir != "" {
			conf += fmt.Sprintf(ipamDataDirStr, dataDir)
		}
		if tc.subnet != "" {
			conf += tc.subnetConfig()
		}
		if tc.ranges != nil {
			conf += tc.rangesConfig()
		}
		conf += ipamEndStr
	}
	return "{" + conf + "\n}"
}

func (tc testCase) subnetConfig() string {
	conf := fmt.Sprintf(subnetConfStr, tc.subnet)
	if tc.gateway != "" {
		conf += fmt.Sprintf(gatewayConfStr, tc.gateway)
	}
	return conf
}

func (tc testCase) rangesConfig() string {
	conf := rangesStartStr
	for i, tcRange := range tc.ranges {
		if i > 0 {
			conf += ","
		}
		if tcRange.gateway != "" {
			conf += fmt.Sprintf(rangeSubnetGatewayConfStr, tcRange.subnet, tcRange.gateway)
		} else {
			conf += fmt.Sprintf(rangeSubnetConfStr, tcRange.subnet)
		}
	}
	return conf + rangesEndStr
}

func (tc testCase) expectedCIDRs() ([]*net.IPNet, []*net.IPNet) {
	var cidrsV4, cidrsV6 []*net.IPNet
	appendSubnet := func(subnet string) {
		ip, cidr, err := net.ParseCIDR(subnet)
		require.Nil(tc.t, err)
		if ipVersion(ip) == "4" {
			cidrsV4 = append(cidrsV4, cidr)
		} else {
			cidrsV6 = append(cidrsV6, cidr)
		}
	}
	if tc.subnet != "" {
		appendSubnet(tc.subnet)
	}
	for _, r := range tc.ranges {
		appendSubnet(r.subnet)
	}
	return cidrsV4, cidrsV6
}

func (tc testCase) createCmdArgs(targetNS ns.NetNS, dataDir string) *cnimsg.CniCmdRequest {
	conf := tc.netConfJSON(dataDir)
	return &cnimsg.CniCmdRequest{
		CniArgs: &cnimsg.CniCmdArgs{
			ContainerId:          CONTAINERID,
			Ifname:               IFNAME,
			Netns:                targetNS.Path(),
			NetworkConfiguration: []byte(conf),
			Args:                 cniservertest.GenerateCNIArgs(testPod, testPodNamespace, testPodInfraContaner),
		},
	}
}

func (tc testCase) createCheckCmdArgs(targetNS ns.NetNS, config *Net, dataDir string) *cnimsg.CniCmdRequest {
	conf, err := json.Marshal(config)
	require.Nil(tc.t, err)

	return &cnimsg.CniCmdRequest{
		CniArgs: &cnimsg.CniCmdArgs{
			ContainerId:          CONTAINERID,
			Ifname:               IFNAME,
			Netns:                targetNS.Path(),
			NetworkConfiguration: conf,
			Args:                 cniservertest.GenerateCNIArgs(testPod, testPodNamespace, testPodInfraContaner),
		},
	}
}

func ipVersion(ip net.IP) string {
	if ip.To4() != nil {
		return "4"
	} else {
		return "6"
	}
}

type cmdAddDelTester struct {
	server   *cniserver.CNIServer
	ctx      context.Context
	testNS   ns.NetNS
	targetNS ns.NetNS
	request  *cnimsg.CniCmdRequest
	vethName string
}

func (tester *cmdAddDelTester) setNS(testNS ns.NetNS, targetNS ns.NetNS) {
	tester.testNS = testNS
	tester.targetNS = targetNS
}

func linkByName(netNS ns.NetNS, name string) (link netlink.Link, err error) {
	err = netNS.Do(func(ns.NetNS) error {
		link, err = netlink.LinkByName(name)
		return err
	})
	return link, err
}

func addrList(netNS ns.NetNS, link netlink.Link, family int) (addrs []netlink.Addr, err error) {
	err = netNS.Do(func(ns.NetNS) error {
		addrs, err = netlink.AddrList(link, family)
		return err
	})
	return addrs, err
}

func routeList(netNS ns.NetNS, link netlink.Link) (routes []netlink.Route, err error) {
	err = netNS.Do(func(ns.NetNS) error {
		routes, err = netlink.RouteList(link, netlink.FAMILY_ALL)
		return err
	})
	return routes, err
}

func matchRoute(expectedCIDR string, routes []netlink.Route) (*netlink.Route, error) {
	gwIP, _, err := net.ParseCIDR(expectedCIDR)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if route.Dst == nil && route.Src == nil && route.Gw.Equal(gwIP) {
			return &route, nil
		}
	}
	return nil, nil
}

// checkContainerNetworking checks for the presence of the interface called IFNAME inside the
// container namespace and checks for the presence of a default route through the Pod CIDR gateway.
func (tester *cmdAddDelTester) checkContainerNetworking(tc testCase) {
	require := require.New(tc.t)
	assert := assert.New(tc.t)

	link, err := linkByName(tester.targetNS, IFNAME)
	require.Nil(err)
	require.Equal(IFNAME, link.Attrs().Name)
	require.IsType(&netlink.Veth{}, link)

	expCIDRsV4, _ := tc.expectedCIDRs()
	addrs, err := addrList(tester.targetNS, link, netlink.FAMILY_V4)
	require.Nil(err)
	// make sure that the IP addresses were correctly assigned to the container's interface
	require.Len(addrs, len(expCIDRsV4))
	for _, expAddr := range expCIDRsV4 {
		findAddr := func() bool {
			for _, addr := range addrs {
				if expAddr.Contains(addr.IP) {
					return true
				}
			}
			return false
		}
		found := findAddr()
		assert.Truef(found, "No IP address assigned from subnet %v", expAddr)
	}

	// Check that default route exsists.
	routes, err := routeList(tester.targetNS, link)
	require.Nil(err)
	for _, cidr := range tc.expGatewayCIDRs {
		expectedRoute, err := matchRoute(cidr, routes)
		require.Nil(err)
		require.NotNil(expectedRoute)
	}
}

func (tester *cmdAddDelTester) cmdAddTest(tc testCase, dataDir string) (*current.Result, error) {
	require := require.New(tc.t)
	var err error

	// Generate network config and command arguments.
	tester.request = tc.createCmdArgs(tester.targetNS, dataDir)

	// Execute cmdADD on the plugin.
	var response *cnimsg.CniCmdResponse
	err = tester.testNS.Do(func(ns.NetNS) error {
		response, err = tester.server.CmdAdd(tester.ctx, tester.request)
		return err
	})
	require.Nil(err)

	r, err := current.NewResult(response.CniResult)
	require.Nil(err)

	result, err := current.GetResult(r)
	require.Nil(err)

	require.Len(result.Interfaces, 2)

	require.Equal(IFNAME, result.Interfaces[1].Name)
	require.Len(result.Interfaces[1].Mac, 17) // mac is random
	require.Equal(tester.targetNS.Path(), result.Interfaces[1].Sandbox)

	// Check for the veth link in the test namespace.
	hostIfaceName := util.GenerateContainerInterfaceName(testPod, testPodNamespace)
	require.Equal(hostIfaceName, result.Interfaces[0].Name)
	require.Len(result.Interfaces[0].Mac, 17)

	link, err := linkByName(tester.testNS, result.Interfaces[0].Name)
	require.Nil(err)

	require.IsType(&netlink.Veth{}, link)
	require.Equal(hostIfaceName, link.Attrs().Name)
	require.Equal(result.Interfaces[0].Mac, link.Attrs().HardwareAddr.String())

	var linkList []netlink.Link
	err = tester.targetNS.Do(func(ns.NetNS) error {
		linkList, err = netlink.LinkList()
		return err
	})
	require.Nil(err)
	require.Len(linkList, 2)

	// Find the veth peer in the container namespace and the default route.
	tester.checkContainerNetworking(tc)

	return result, nil
}

func buildOneConfig(name, cniVersion string, orig *Net, prevResult types.Result) (*Net, error) {
	var err error

	inject := map[string]interface{}{
		"name":       name,
		"cniVersion": cniVersion,
	}
	// Add previous plugin result
	if prevResult != nil {
		inject["prevResult"] = prevResult
	}

	// Ensure every config uses the same name and version
	config := make(map[string]interface{})
	confBytes, err := json.Marshal(orig)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(confBytes, &config)
	if err != nil {
		return nil, fmt.Errorf("unmarshal existing network bytes: %s", err)
	}

	for key, value := range inject {
		config[key] = value
	}

	newBytes, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}

	conf := &Net{}
	if err := json.Unmarshal(newBytes, &conf); err != nil {
		return nil, fmt.Errorf("error parsing configuration: %s", err)
	}

	return conf, nil

}

func (tester *cmdAddDelTester) cmdCheckTest(tc testCase, conf *Net, dataDir string) {
	require := require.New(tc.t)
	var err error

	// Generate network config and command arguments.
	tester.request = tc.createCheckCmdArgs(tester.targetNS, conf, dataDir)

	// Execute cmdCHECK on the plugin.
	err = tester.testNS.Do(func(ns.NetNS) error {
		_, err = tester.server.CmdCheck(tester.ctx, tester.request)
		return err
	})
	require.Nil(err)

	// Find the veth peer in the container namespace and the default route.
	tester.checkContainerNetworking(tc)
}

func (tester *cmdAddDelTester) cmdDelTest(tc testCase, dataDir string) {
	require := require.New(tc.t)
	var err error

	tester.request = tc.createCmdArgs(tester.targetNS, dataDir)

	// Execute cmdDEL on the plugin.
	err = tester.testNS.Do(func(ns.NetNS) error {
		_, err := tester.server.CmdDel(tester.ctx, tester.request)
		return err
	})
	require.Nil(err)

	var link netlink.Link

	// Make sure the host veth has been deleted.
	link, err = netlink.LinkByName(IFNAME)
	require.NotNil(err)
	require.Nil(link)

	// Make sure the container veth has been deleted
	link, err = netlink.LinkByName(tester.vethName)
	require.NotNil(err)
	require.Nil(link)
}

func newTester() *cmdAddDelTester {
	tester := &cmdAddDelTester{}
	ifaceStore := agent.NewInterfaceStore()
	tester.server = cniserver.New(testSock, "", 1450, testNodeConfig, ovsServiceMock, ofServiceMock, ifaceStore, k8sFake.NewSimpleClientset())
	ctx, _ := context.WithCancel(context.Background())
	tester.ctx = ctx
	return tester
}

func cmdAddDelCheckTest(testNS ns.NetNS, tc testCase, dataDir string) {
	require := require.New(tc.t)

	require.Equal("0.4.0", tc.cniVersion)

	// Get a Add/Del tester based on test case version
	tester := newTester()

	targetNS, err := testutils.NewNS()
	require.Nil(err)
	defer targetNS.Close()
	tester.setNS(testNS, targetNS)

	ipamResult := ipamtest.GenerateIPAMResult("0.4.0", tc.addresses, tc.routes, tc.dns)
	ipamMock.EXPECT().Add(mock.Any(), mock.Any()).Return(ipamResult, nil).AnyTimes()

	// Mock ovs output while get ovs port external configuration
	ovsPortname := util.GenerateContainerInterfaceName(testPod, testPodNamespace)
	ovsPortUUID := uuid.New().String()
	ovsServiceMock.EXPECT().CreatePort(ovsPortname, ovsPortname, mock.Any()).Return(ovsPortUUID, nil).AnyTimes()
	ovsServiceMock.EXPECT().GetOFPort(ovsPortname).Return(int32(10), nil).AnyTimes()
	ofServiceMock.EXPECT().InstallPodFlows(ovsPortname, mock.Any(), mock.Any(), mock.Any(), mock.Any()).Return(nil)

	// Test ip allocation
	prevResult, err := tester.cmdAddTest(tc, dataDir)
	require.Nil(err)

	require.NotNil(prevResult)

	confString := tc.netConfJSON(dataDir)

	conf := &Net{}
	err = json.Unmarshal([]byte(confString), &conf)
	require.Nil(err)

	conf.IPAM, _, err = allocator.LoadIPAMConfig([]byte(confString), "")
	require.Nil(err)

	newConf, err := buildOneConfig("testConfig", tc.cniVersion, conf, prevResult)
	require.Nil(err)

	// Test CHECK
	tester.cmdCheckTest(tc, newConf, dataDir)

	// Test delete
	ovsServiceMock.EXPECT().DeletePort(ovsPortUUID).Return(nil).AnyTimes()
	ofServiceMock.EXPECT().UninstallPodFlows(ovsPortname).Return(nil)
	tester.cmdDelTest(tc, dataDir)
}

func getContainerIPMacConfig(ipamResult *current.Result) (string, string) {
	containerMAC := ipamResult.Interfaces[1].Mac
	containerIP := ""
	for _, ipc := range ipamResult.IPs {
		if ipc.Version == "4" {
			containerIP = ipc.Address.IP.String()
			break
		}
	}
	return containerIP, containerMAC
}

func TestAntreaServerFunc(t *testing.T) {
	controller := mock.NewController(t)
	defer controller.Finish()
	ipamMock = ipamtest.NewMockIPAMDriver(controller)
	_ = ipam.RegisterIPAMDriver("mock", ipamMock)
	ovsServiceMock = ovsconfigtest.NewMockOVSBridgeClient(controller)
	ofServiceMock = openflowtest.NewMockClient(controller)

	var originalNS ns.NetNS
	var dataDir string

	setup := func() {
		// Create a new netNS so we don't modify the host
		var err error
		originalNS, err = testutils.NewNS()
		require.Nil(t, err)

		dataDir, err = ioutil.TempDir("", "antrea_server_test")
		require.Nil(t, err)

		ipamMock.EXPECT().Del(mock.Any(), mock.Any()).Return(nil).AnyTimes()
		ipamMock.EXPECT().Check(mock.Any(), mock.Any()).Return(nil).AnyTimes()

		ovsServiceMock.EXPECT().GetPortList().Return([]ovsconfig.OVSPortData{}, nil).AnyTimes()
	}

	teardown := func() {
		assert.Nil(t, os.RemoveAll(dataDir))
		assert.Nil(t, originalNS.Close())
	}

	testCases := []testCase{
		{
			name:       "ADD/DEL/CHECK for 0.4.0 config",
			cniVersion: "0.4.0",
			// IPv4 only
			ranges: []rangeInfo{{
				subnet: "10.1.2.0/24",
			}},
			expGatewayCIDRs: []string{"10.1.2.1/24"},
			addresses:       []string{"10.1.2.100/24,10.1.2.1,4"},
			routes:          []string{"10.0.0.0/8,10.1.2.1", "0.0.0.0/0,10.1.2.1"},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			setup()
			defer teardown()
			tc.t = t
			cmdAddDelCheckTest(originalNS, tc, dataDir)
		})
	}
}

func init() {
	nodeName := "node1"
	gwIP := net.ParseIP("192.168.1.1")
	gwMAC, _ := net.ParseMAC("11:11:11:11:11:11")
	nodeGateway := &agent.Gateway{IP: gwIP, MAC: gwMAC, Name: "gw"}
	_, nodePodeCIDR, _ := net.ParseCIDR("192.168.1.0/24")

	testNodeConfig = &agent.NodeConfig{bridge, nodeName, nodePodeCIDR, nodeGateway}
}
