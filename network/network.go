package network

import (
	"encoding/hex"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/nftables"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"

	"github.com/outofforest/isolator/lib/firewall"
)

const (
	nftTable               = "isolator"
	nftChainFilterInput    = "FILTER_INPUT"
	nftChainFilterOutput   = "FILTER_OUTPUT"
	nftChainFilterForward  = "FILTER_FORWARD"
	nftChainNATOutput      = "NAT_OUTPUT"
	nftChainNATPrerouting  = "NAT_PREROUTING"
	nftChainNATPostrouting = "NAT_POSTROUTING"
)

var mu = sync.Mutex{}

// ExposedPort defines a port to be exposed from the namespace.
type ExposedPort struct {
	Protocol     string
	ExternalIP   net.IP
	ExternalPort uint16
	InternalPort uint16
	Public       bool
}

// Random selects random available network.
func Random(prefix uint8) (*net.IPNet, func() error, error) {
	mu.Lock()
	defer mu.Unlock()

	network, err := findFreeNetwork(prefix)
	if err != nil {
		return nil, nil, err
	}

	if err := createBridge(network); err != nil {
		return nil, nil, err
	}

	if err := prepareFirewall(network); err != nil {
		return nil, nil, err
	}

	if err := enableForwarding(network); err != nil {
		return nil, nil, err
	}

	return network, func() error {
		mu.Lock()
		defer mu.Unlock()

		if err := cleanFirewall(network); err != nil {
			return err
		}

		return deleteBridge(network)
	}, nil
}

func findFreeNetwork(prefix uint8) (*net.IPNet, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	networks := []*net.IPNet{}
	for _, l := range links {
		addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		for _, addr := range addrs {
			networks = append(networks, &net.IPNet{IP: netIP(addr.IPNet), Mask: addr.Mask})
		}
	}

	for {
		ip := uint32ToIP4(rand.Uint32())
		ip[0] = 10
		ipNet := &net.IPNet{IP: ip, Mask: net.CIDRMask(int(prefix), 32)}
		ipNet.IP = netIP(ipNet)

		var exists bool
		for _, n := range networks {
			p := int(prefix)
			ones, _ := n.Mask.Size()
			if ones < p {
				p = ones
			}

			mask := net.CIDRMask(p, 32)
			n1 := netIP(&net.IPNet{IP: ipNet.IP, Mask: mask})
			n2 := netIP(&net.IPNet{IP: n.IP, Mask: mask})

			if n1.Equal(n2) {
				exists = true
				break
			}
		}

		if !exists {
			return ipNet, nil
		}
	}
}

// Addr returns nth address in the network.
func Addr(network *net.IPNet, index uint32) *net.IPNet {
	return &net.IPNet{IP: uint32ToIP4(ip4ToUint32(netIP(network)) + index), Mask: network.Mask}
}

// Join adds container to the network.
func Join(ip *net.IPNet, exposedPorts []ExposedPort, pid int) (func() error, error) {
	mu.Lock()
	defer mu.Unlock()

	link, err := netlink.LinkByName(bridgeName(ip))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	bridgeLink, ok := link.(*netlink.Bridge)
	if !ok {
		return nil, errors.New("link is not a bridge")
	}

	vethN := vethName(ip.IP)
	hostVETHName := vethN + "0"
	containerVETHName := vethN + "1"

	vethHost := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: hostVETHName,
		},
		PeerName: containerVETHName,
	}

	if err := netlink.LinkAdd(vethHost); err != nil {
		return nil, errors.WithStack(err)
	}

	if err := netlink.LinkSetUp(vethHost); err != nil {
		return nil, errors.WithStack(err)
	}

	if err := netlink.LinkSetMaster(vethHost, bridgeLink); err != nil {
		return nil, errors.WithStack(err)
	}

	vethContainer, err := netlink.LinkByName(containerVETHName)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := netlink.LinkSetNsPid(vethContainer, pid); err != nil {
		return nil, errors.WithStack(err)
	}

	if err := configureFirewall(ip, exposedPorts); err != nil {
		return nil, err
	}

	return func() error {
		mu.Lock()
		defer mu.Unlock()

		if err := cleanFirewall(ip); err != nil {
			return err
		}

		_ = netlink.LinkDel(vethHost)
		return nil
	}, nil
}

// SetupContainer sets up networking inside network namespace.
func SetupContainer(ip *net.IPNet) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return errors.WithStack(err)
	}

	if err := netlink.LinkSetUp(lo); err != nil {
		return errors.WithStack(err)
	}

	vethN := vethName(ip.IP)
	containerVETHName := vethN + "1"

	vethContainer, err := netlink.LinkByName(containerVETHName)
	if err != nil {
		return errors.WithStack(err)
	}

	if err := netlink.LinkSetUp(vethContainer); err != nil {
		return errors.WithStack(err)
	}

	if err := netlink.AddrAdd(vethContainer, &netlink.Addr{IPNet: ip}); err != nil {
		return errors.WithStack(err)
	}

	if err := netlink.RouteAdd(&netlink.Route{
		Scope:     netlink.SCOPE_UNIVERSE,
		LinkIndex: vethContainer.Attrs().Index,
		Gw:        firstIP(ip),
	}); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func createBridge(network *net.IPNet) error {
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeName(network),
		},
	}

	if err := netlink.LinkAdd(bridge); err != nil {
		return errors.WithStack(err)
	}

	if err := netlink.AddrAdd(bridge, &netlink.Addr{IPNet: &net.IPNet{IP: firstIP(network), Mask: network.Mask}}); err != nil {
		return errors.WithStack(err)
	}

	if err := netlink.LinkSetUp(bridge); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func prepareFirewall(network *net.IPNet) error {
	bridge := bridgeName(network)
	netAddr := netIP(network)
	c := &nftables.Conn{}

	tables, err := c.ListTables()
	if err != nil {
		return errors.WithStack(err)
	}

	for _, t := range tables {
		if t.Name != nftTable {
			c.DelTable(t)
		}
	}

	table := c.AddTable(&nftables.Table{
		Name:   nftTable,
		Family: nftables.TableFamilyIPv4,
	})

	policyDrop := nftables.ChainPolicyDrop
	policyAccept := nftables.ChainPolicyAccept

	chains, err := c.ListChains()
	if err != nil {
		return errors.WithStack(err)
	}

	var filterInputChain *nftables.Chain
	for _, ch := range chains {
		if ch.Table.Name != nftTable {
			continue
		}
		if ch.Name == nftChainFilterInput {
			filterInputChain = ch
			break
		}
	}

	if filterInputChain == nil {
		filterInputChain = c.AddChain(&nftables.Chain{
			Name:     nftChainFilterInput,
			Table:    table,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookInput,
			Priority: nftables.ChainPriorityFilter,
			Policy:   &policyDrop,
		})

		c.AddRule(&nftables.Rule{
			Table: table,
			Chain: filterInputChain,
			Exprs: firewall.Expressions(
				firewall.ConnectionEstablished(),
				firewall.Accept(),
			),
		})
		c.AddRule(&nftables.Rule{
			Table: table,
			Chain: filterInputChain,
			Exprs: firewall.Expressions(
				firewall.IncomingInterface("lo"),
				firewall.LocalSourceAddress(),
				firewall.Accept(),
			),
		})
	}

	c.AddChain(&nftables.Chain{
		Name:     nftChainFilterOutput,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policyAccept,
	})
	filterForwardChain := c.AddChain(&nftables.Chain{
		Name:     nftChainFilterForward,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policyDrop,
	})
	c.AddChain(&nftables.Chain{
		Name:     nftChainNATOutput,
		Table:    table,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATSource,
		Type:     nftables.ChainTypeNAT,
		Policy:   &policyAccept,
	})
	c.AddChain(&nftables.Chain{
		Name:     nftChainNATPrerouting,
		Table:    table,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
		Type:     nftables.ChainTypeNAT,
		Policy:   &policyAccept,
	})
	natPostroutingChain := c.AddChain(&nftables.Chain{
		Name:     nftChainNATPostrouting,
		Table:    table,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Type:     nftables.ChainTypeNAT,
		Policy:   &policyAccept,
	})

	// forward from namespace
	c.AddRule(&nftables.Rule{
		Table:    table,
		Chain:    filterForwardChain,
		UserData: netAddr,
		Exprs: firewall.Expressions(
			firewall.OutgoingInterface(bridge),
			firewall.ConnectionEstablished(),
			firewall.Accept(),
		),
	})

	// forward to namespace
	c.AddRule(&nftables.Rule{
		Table:    filterForwardChain.Table,
		Chain:    filterForwardChain,
		UserData: netAddr,
		Exprs: firewall.Expressions(
			firewall.IncomingInterface(bridge),
			firewall.Accept(),
		),
	})

	// masquerade namespace
	c.AddRule(&nftables.Rule{
		Table:    table,
		Chain:    natPostroutingChain,
		UserData: netAddr,
		Exprs: firewall.Expressions(
			firewall.IncomingInterface(bridge),
			firewall.Masquerade(),
		),
	})

	return errors.WithStack(c.Flush())
}

func enableForwarding(network *net.IPNet) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o600); err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(os.WriteFile(filepath.Join("/proc/sys/net/ipv4/conf", bridgeName(network), "route_localnet"), []byte("1"), 0o600))
}

func deleteBridge(network *net.IPNet) error {
	links, err := netlink.LinkList()
	if err != nil {
		return errors.WithStack(err)
	}

	bridge := bridgeName(network)
	for _, l := range links {
		if l.Attrs().Name == bridge {
			_ = netlink.LinkDel(l)
			return nil
		}
	}

	return nil
}

func configureFirewall(ip *net.IPNet, exposedPorts []ExposedPort) error {
	c := &nftables.Conn{}

	tables, err := c.ListTables()
	if err != nil {
		return errors.WithStack(err)
	}

	var table *nftables.Table
	for _, t := range tables {
		if t.Name == nftTable {
			table = t
			break
		}
	}

	if table == nil {
		return errors.Errorf("table %s does not exist", nftTable)
	}

	chains, err := c.ListChains()
	if err != nil {
		return errors.WithStack(err)
	}

	var filterForwardChain *nftables.Chain
	var natOutputChain *nftables.Chain
	var natPreroutingChain *nftables.Chain
	var natPostroutingChain *nftables.Chain
	for _, ch := range chains {
		if ch.Table.Name != nftTable {
			continue
		}
		switch ch.Name {
		case nftChainFilterForward:
			filterForwardChain = ch
		case nftChainNATOutput:
			natOutputChain = ch
		case nftChainNATPrerouting:
			natPreroutingChain = ch
		case nftChainNATPostrouting:
			natPostroutingChain = ch
		}
		if filterForwardChain != nil && natOutputChain != nil && natPreroutingChain != nil && natPostroutingChain != nil {
			break
		}
	}

	if filterForwardChain == nil {
		return errors.Errorf("chain %s does not exist", nftChainFilterForward)
	}
	if natOutputChain == nil {
		return errors.Errorf("chain %s does not exist", nftChainNATOutput)
	}
	if natPreroutingChain == nil {
		return errors.Errorf("chain %s does not exist", nftChainNATPrerouting)
	}
	if natPostroutingChain == nil {
		return errors.Errorf("chain %s does not exist", nftChainNATPostrouting)
	}

	bridge := bridgeName(ip)
	hostIP := firstIP(ip)
	for _, p := range exposedPorts {
		// redirecting requests originating from the host machine
		c.AddRule(&nftables.Rule{
			Table:    table,
			Chain:    natOutputChain,
			UserData: ip.IP,
			Exprs: firewall.Expressions(
				firewall.DestinationAddress(p.ExternalIP),
				firewall.Protocol(p.Protocol),
				firewall.DestinationPort(p.ExternalPort),
				firewall.DestinationNAT(ip.IP, p.InternalPort),
			),
		})

		// redirecting requests from local addresses.
		c.AddRule(&nftables.Rule{
			Table:    table,
			Chain:    natPostroutingChain,
			UserData: ip.IP,
			Exprs: firewall.Expressions(
				firewall.OutgoingInterface(bridge),
				firewall.NotSourceAddress(hostIP),
				firewall.LocalSourceAddress(),
				firewall.DestinationAddress(ip.IP),
				firewall.Protocol(p.Protocol),
				firewall.DestinationPort(p.InternalPort),
				firewall.Masquerade(),
			),
		})

		if p.Public {
			// enable forwarding
			c.AddRule(&nftables.Rule{
				Table:    table,
				Chain:    filterForwardChain,
				UserData: ip.IP,
				Exprs: firewall.Expressions(
					firewall.DestinationAddress(ip.IP),
					firewall.Protocol(p.Protocol),
					firewall.DestinationPort(p.InternalPort),
					firewall.Accept(),
				),
			})

			// redirecting external requests
			c.AddRule(&nftables.Rule{
				Table:    table,
				Chain:    natPreroutingChain,
				UserData: ip.IP,
				Exprs: firewall.Expressions(
					firewall.DestinationAddress(p.ExternalIP),
					firewall.Protocol(p.Protocol),
					firewall.DestinationPort(p.ExternalPort),
					firewall.DestinationNAT(ip.IP, p.InternalPort),
				),
			})

			// redirecting requests from other namespaces attached to the same bridge (loop).
			c.AddRule(&nftables.Rule{
				Table:    table,
				Chain:    natPostroutingChain,
				UserData: ip.IP,
				Exprs: firewall.Expressions(
					firewall.OutgoingInterface(bridge),
					firewall.SourceNetwork(ip),
					firewall.NotSourceAddress(hostIP),
					firewall.DestinationAddress(ip.IP),
					firewall.Protocol(p.Protocol),
					firewall.DestinationPort(p.InternalPort),
					firewall.Masquerade(),
				),
			})
		}
	}

	return errors.WithStack(c.Flush())
}

func cleanFirewall(ip *net.IPNet) error {
	c := &nftables.Conn{}

	tables, err := c.ListTables()
	if err != nil {
		return errors.WithStack(err)
	}

	var table *nftables.Table
	for _, t := range tables {
		if t.Name == nftTable {
			table = t
			break
		}
	}

	if table == nil {
		return nil
	}

	chains, err := c.ListChains()
	if err != nil {
		return errors.WithStack(err)
	}

	for _, ch := range chains {
		if ch.Table.Name != nftTable {
			continue
		}

		rules, err := c.GetRules(table, ch)
		if err != nil {
			return errors.WithStack(err)
		}

		for _, r := range rules {
			if len(r.UserData) != 4 {
				continue
			}
			if !net.IP(r.UserData).Equal(ip.IP) && !netIP(&net.IPNet{IP: r.UserData, Mask: ip.Mask}).Equal(ip.IP) {
				continue
			}
			if err := c.DelRule(r); err != nil {
				return errors.WithStack(err)
			}
		}
	}

	return errors.WithStack(c.Flush())
}

func ip4ToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP4(val uint32) net.IP {
	return net.IPv4(byte(val>>24), byte(val>>16), byte(val>>8), byte(val)).To4()
}

func firstIP(network *net.IPNet) net.IP {
	return uint32ToIP4(ip4ToUint32(netIP(network)) + 1)
}

func netIP(network *net.IPNet) net.IP {
	ones, bits := network.Mask.Size()
	return uint32ToIP4(ip4ToUint32(network.IP) & (uint32(math.MaxUint32) << (bits - ones)))
}

func bridgeName(ip *net.IPNet) string {
	return "islbr" + hex.EncodeToString(netIP(ip))
}

func vethName(ip net.IP) string {
	return "islve" + hex.EncodeToString(ip.To4())
}
