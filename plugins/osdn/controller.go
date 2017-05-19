package osdn

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/golang/glog"

	"github.com/openshift/openshift-sdn/pkg/ipcmd"
	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/pkg/ovs"
	osapi "github.com/openshift/origin/pkg/sdn/api"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/util/sysctl"
)

const (
	// rule versioning; increment each time flow rules change
	VERSION        = 1
	VERSION_TABLE  = "table=253"
	VERSION_ACTION = "actions=note:"

	BR    = "br0"
	TUN   = "tun0"
	VXLAN = "vxlan0"

	VXLAN_PORT = "4789"
)

func getPluginVersion(multitenant bool) []string {
	if VERSION > 254 {
		panic("Version too large!")
	}
	version := fmt.Sprintf("%02X", VERSION)
	if multitenant {
		return []string{"01", version}
	}
	// single-tenant
	return []string{"00", version}
}

func alreadySetUp(multitenant bool, localSubnetGatewayCIDR string) bool {
	otx := ovs.NewTransaction(BR)
	flows, err := otx.DumpFlows()
	otx.EndTransaction()
	if err != nil {
		return false
	}
	for _, flow := range flows {
		if !strings.Contains(flow, VERSION_TABLE) {
			continue
		}
		idx := strings.Index(flow, VERSION_ACTION)
		if idx < 0 {
			continue
		}

		// OVS note action format hex bytes separated by '.'; first
		// byte is plugin type (multi-tenant/single-tenant) and second
		// byte is flow rule version
		expected := getPluginVersion(multitenant)
		existing := strings.Split(flow[idx+len(VERSION_ACTION):], ".")
		if len(existing) >= 2 && existing[0] == expected[0] && existing[1] == expected[1] {
			return true
		}
	}

	return false
}

func deleteLocalSubnetRoute(device, localSubnetCIDR string) {
	const (
		timeInterval = 100 * time.Millisecond
		maxIntervals = 20
	)

	for i := 0; i < maxIntervals; i++ {
		itx := ipcmd.NewTransaction(device)
		routes, err := itx.GetRoutes()
		if err != nil {
			glog.Errorf("Could not get routes for dev %s: %v", device, err)
			return
		}
		for _, route := range routes {
			if strings.Contains(route, localSubnetCIDR) {
				itx.DeleteRoute(localSubnetCIDR)
				err = itx.EndTransaction()
				if err != nil {
					glog.Errorf("Could not delete subnet route %s from dev %s: %v", localSubnetCIDR, device, err)
				}
				return
			}
		}

		time.Sleep(timeInterval)
	}

	glog.Errorf("Timed out looking for %s route for dev %s; if it appears later it will not be deleted.", localSubnetCIDR, device)
}

func (plugin *OsdnNode) SetupSDN(localSubnetCIDR, clusterNetworkCIDR, servicesNetworkCIDR string, mtu uint) (bool, error) {
	_, ipnet, err := net.ParseCIDR(localSubnetCIDR)
	localSubnetMaskLength, _ := ipnet.Mask.Size()
	localSubnetGateway := netutils.GenerateDefaultGateway(ipnet).String()

	glog.V(5).Infof("[SDN setup] node pod subnet %s gateway %s", ipnet.String(), localSubnetGateway)

	gwCIDR := fmt.Sprintf("%s/%d", localSubnetGateway, localSubnetMaskLength)
	if alreadySetUp(plugin.multitenant, gwCIDR) {
		glog.V(5).Infof("[SDN setup] no SDN setup required")
		return false, nil
	}
	glog.V(5).Infof("[SDN setup] full SDN setup required")

	otx := ovs.NewTransaction(BR)
	otx.AddBridge("fail-mode=secure", "protocols=OpenFlow13")
	otx.AddPort(VXLAN, 1, "type=vxlan", `options:remote_ip="flow"`, `options:key="flow"`)
	otx.AddPort(TUN, 2, "type=internal")

	// Table 0: initial dispatch based on in_port
	// vxlan0
	otx.AddFlow("table=0, priority=200, in_port=1, arp, nw_src=%s, nw_dst=%s, actions=move:NXM_NX_TUN_ID[0..31]->NXM_NX_REG0[],goto_table:1", clusterNetworkCIDR, localSubnetCIDR)
	otx.AddFlow("table=0, priority=200, in_port=1, ip, nw_src=%s, nw_dst=%s, actions=move:NXM_NX_TUN_ID[0..31]->NXM_NX_REG0[],goto_table:1", clusterNetworkCIDR, localSubnetCIDR)
	otx.AddFlow("table=0, priority=150, in_port=1, actions=drop")
	// tun0
	otx.AddFlow("table=0, priority=200, in_port=2, arp, nw_src=%s, nw_dst=%s, actions=goto_table:5", localSubnetGateway, clusterNetworkCIDR)
	otx.AddFlow("table=0, priority=200, in_port=2, ip, actions=goto_table:5")
	otx.AddFlow("table=0, priority=150, in_port=2, actions=drop")
	// else, from a container
	otx.AddFlow("table=0, priority=100, arp, actions=goto_table:2")
	otx.AddFlow("table=0, priority=100, ip, actions=goto_table:2")
	otx.AddFlow("table=0, priority=0, actions=drop")

	// Table 1: VXLAN ingress filtering; filled in by AddHostSubnetRules()
	// eg, "table=1, priority=100, tun_src=${remote_node_ip}, actions=goto_table:5"
	otx.AddFlow("table=1, priority=0, actions=drop")

	// Table 2: from OpenShift container; validate IP/MAC, assign tenant-id; filled in by openshift-sdn-ovs
	// eg, "table=2, priority=100, in_port=${ovs_port}, arp, nw_src=${ipaddr}, arp_sha=${macaddr}, actions=load:${tenant_id}->NXM_NX_REG0[], goto_table:5"
	//     "table=2, priority=100, in_port=${ovs_port}, ip, nw_src=${ipaddr}, actions=load:${tenant_id}->NXM_NX_REG0[], goto_table:3"
	// (${tenant_id} is always 0 for single-tenant)
	otx.AddFlow("table=2, priority=0, actions=drop")

	// Table 3: from OpenShift container; service vs non-service
	otx.AddFlow("table=3, priority=100, ip, nw_dst=%s, actions=goto_table:4", servicesNetworkCIDR)
	otx.AddFlow("table=3, priority=0, actions=goto_table:5")

	// Table 4: from OpenShift container; service dispatch; filled in by AddServiceRules()
	otx.AddFlow("table=4, priority=200, reg0=0, actions=output:2")
	// eg, "table=4, priority=100, reg0=${tenant_id}, ${service_proto}, nw_dst=${service_ip}, tp_dst=${service_port}, actions=output:2"
	otx.AddFlow("table=4, priority=0, actions=drop")

	// Table 5: general routing
	otx.AddFlow("table=5, priority=300, arp, nw_dst=%s, actions=output:2", localSubnetGateway)
	otx.AddFlow("table=5, priority=300, ip, nw_dst=%s, actions=output:2", localSubnetGateway)
	otx.AddFlow("table=5, priority=200, arp, nw_dst=%s, actions=goto_table:6", localSubnetCIDR)
	otx.AddFlow("table=5, priority=200, ip, nw_dst=%s, actions=goto_table:7", localSubnetCIDR)
	otx.AddFlow("table=5, priority=100, arp, nw_dst=%s, actions=goto_table:8", clusterNetworkCIDR)
	otx.AddFlow("table=5, priority=100, ip, nw_dst=%s, actions=goto_table:8", clusterNetworkCIDR)
	otx.AddFlow("table=5, priority=0, ip, actions=output:2")
	otx.AddFlow("table=5, priority=0, arp, actions=drop")

	// Table 6: ARP to container, filled in by openshift-sdn-ovs
	// eg, "table=6, priority=100, arp, nw_dst=${container_ip}, actions=output:${ovs_port}"
	otx.AddFlow("table=6, priority=0, actions=drop")

	// Table 7: IP to container; filled in by openshift-sdn-ovs
	// eg, "table=7, priority=100, reg0=0, ip, nw_dst=${ipaddr}, actions=output:${ovs_port}"
	// eg, "table=7, priority=100, reg0=${tenant_id}, ip, nw_dst=${ipaddr}, actions=output:${ovs_port}"
	otx.AddFlow("table=7, priority=0, actions=drop")

	// Table 8: to remote container; filled in by AddHostSubnetRules()
	// eg, "table=8, priority=100, arp, nw_dst=${remote_subnet_cidr}, actions=move:NXM_NX_REG0[]->NXM_NX_TUN_ID[0..31], set_field:${remote_node_ip}->tun_dst,output:1"
	// eg, "table=8, priority=100, ip, nw_dst=${remote_subnet_cidr}, actions=move:NXM_NX_REG0[]->NXM_NX_TUN_ID[0..31], set_field:${remote_node_ip}->tun_dst,output:1"
	otx.AddFlow("table=8, priority=0, actions=drop")

	err = otx.EndTransaction()
	if err != nil {
		return false, err
	}

	itx := ipcmd.NewTransaction(TUN)
	itx.AddAddress(gwCIDR)
	defer deleteLocalSubnetRoute(TUN, localSubnetCIDR)
	itx.SetLink("mtu", fmt.Sprint(mtu))
	itx.SetLink("up")
	itx.AddRoute(clusterNetworkCIDR, "proto", "kernel", "scope", "link")
	itx.AddRoute(servicesNetworkCIDR)
	err = itx.EndTransaction()
	if err != nil {
		return false, err
	}

	// Clean up docker0 since docker won't
	itx = ipcmd.NewTransaction("docker0")
	itx.SetLink("down")
	itx.IgnoreError()
	itx.DeleteLink()
	itx.IgnoreError()
	_ = itx.EndTransaction()

	// Enable IP forwarding for ipv4 packets
	err = sysctl.SetSysctl("net/ipv4/ip_forward", 1)
	if err != nil {
		return false, fmt.Errorf("Could not enable IPv4 forwarding: %s", err)
	}
	err = sysctl.SetSysctl(fmt.Sprintf("net/ipv4/conf/%s/forwarding", TUN), 1)
	if err != nil {
		return false, fmt.Errorf("Could not enable IPv4 forwarding on %s: %s", TUN, err)
	}

	// Table 253: rule version; note action is hex bytes separated by '.'
	otx = ovs.NewTransaction(BR)
	pluginVersion := getPluginVersion(plugin.multitenant)
	otx.AddFlow("%s, %s%s.%s", VERSION_TABLE, VERSION_ACTION, pluginVersion[0], pluginVersion[1])
	err = otx.EndTransaction()
	if err != nil {
		return false, err
	}

	return true, nil
}

func (plugin *OsdnNode) AddHostSubnetRules(subnet *osapi.HostSubnet) error {
	glog.Infof("AddHostSubnetRules for %s", hostSubnetToString(subnet))
	otx := ovs.NewTransaction(BR)

	otx.AddFlow("table=1, priority=100, tun_src=%s, actions=goto_table:5", subnet.HostIP)
	otx.AddFlow("table=8, priority=100, arp, nw_dst=%s, actions=move:NXM_NX_REG0[]->NXM_NX_TUN_ID[0..31],set_field:%s->tun_dst,output:1", subnet.Subnet, subnet.HostIP)
	otx.AddFlow("table=8, priority=100, ip, nw_dst=%s, actions=move:NXM_NX_REG0[]->NXM_NX_TUN_ID[0..31],set_field:%s->tun_dst,output:1", subnet.Subnet, subnet.HostIP)

	err := otx.EndTransaction()
	if err != nil {
		return fmt.Errorf("Error adding OVS flows for subnet: %v, %v", subnet, err)
	}
	return nil
}

func (plugin *OsdnNode) DeleteHostSubnetRules(subnet *osapi.HostSubnet) error {
	glog.Infof("DeleteHostSubnetRules for %s", hostSubnetToString(subnet))

	otx := ovs.NewTransaction(BR)
	otx.DeleteFlows("table=1, tun_src=%s", subnet.HostIP)
	otx.DeleteFlows("table=8, nw_dst=%s", subnet.Subnet)
	err := otx.EndTransaction()
	if err != nil {
		return fmt.Errorf("Error deleting OVS flows for subnet: %v, %v", subnet, err)
	}
	return nil
}

func (plugin *OsdnNode) AddServiceRules(service *kapi.Service, netID uint) error {
	if !plugin.multitenant {
		return nil
	}

	glog.V(5).Infof("AddServiceRules for %v", service)

	otx := ovs.NewTransaction(BR)
	for _, port := range service.Spec.Ports {
		otx.AddFlow(generateAddServiceRule(netID, service.Spec.ClusterIP, port.Protocol, int(port.Port)))
		err := otx.EndTransaction()
		if err != nil {
			return fmt.Errorf("Error adding OVS flows for service: %v, netid: %d, %v", service, netID, err)
		}
	}
	return nil
}

func (plugin *OsdnNode) DeleteServiceRules(service *kapi.Service) error {
	if !plugin.multitenant {
		return nil
	}

	glog.V(5).Infof("DeleteServiceRules for %v", service)

	otx := ovs.NewTransaction(BR)
	for _, port := range service.Spec.Ports {
		otx.DeleteFlows(generateDeleteServiceRule(service.Spec.ClusterIP, port.Protocol, int(port.Port)))
		err := otx.EndTransaction()
		if err != nil {
			return fmt.Errorf("Error deleting OVS flows for service: %v, %v", service, err)
		}
	}
	return nil
}

func generateBaseServiceRule(IP string, protocol kapi.Protocol, port int) string {
	return fmt.Sprintf("table=4, %s, nw_dst=%s, tp_dst=%d", strings.ToLower(string(protocol)), IP, port)
}

func generateAddServiceRule(netID uint, IP string, protocol kapi.Protocol, port int) string {
	baseRule := generateBaseServiceRule(IP, protocol, port)
	if netID == 0 {
		return fmt.Sprintf("%s, priority=100, actions=output:2", baseRule)
	} else {
		return fmt.Sprintf("%s, priority=100, reg0=%d, actions=output:2", baseRule, netID)
	}
}

func generateDeleteServiceRule(IP string, protocol kapi.Protocol, port int) string {
	return generateBaseServiceRule(IP, protocol, port)
}
