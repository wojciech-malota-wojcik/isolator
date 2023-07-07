package network

import (
	"bytes"
	"encoding/hex"
	"math"
	"math/rand"
	"net"
	"os"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

const (
	nftTable            = "isolator"
	nftChainInput       = "INPUT"
	nftChainOutput      = "OUTPUT"
	nftChainForward     = "FORWARD"
	nftChainPostrouting = "POSTROUTING"
)

var mu = sync.Mutex{}

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

	if err := prepareFirewall(); err != nil {
		return nil, nil, err
	}

	if err := enableForwarding(); err != nil {
		return nil, nil, err
	}

	return network, func() error {
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

			if bytes.Equal(n1, n2) {
				exists = true
				break
			}
		}

		if !exists {
			return ipNet, nil
		}
	}
}

func Addr(network *net.IPNet, index uint32) *net.IPNet {
	return &net.IPNet{IP: uint32ToIP4(ip4ToUint32(netIP(network)) + index), Mask: network.Mask}
}

func Join(ip *net.IPNet, pid int) (func() error, error) {
	bridgeLink, err := netlink.LinkByName(bridgeName(ip))
	if err != nil {
		return nil, errors.WithStack(err)
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

	if err := netlink.LinkSetMaster(vethHost, bridgeLink.(*netlink.Bridge)); err != nil {
		return nil, errors.WithStack(err)
	}

	vethContainer, err := netlink.LinkByName(containerVETHName)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := netlink.LinkSetNsPid(vethContainer, pid); err != nil {
		return nil, errors.WithStack(err)
	}

	if err := configureFirewall(ip); err != nil {
		return nil, err
	}

	return func() error {
		return errors.WithStack(netlink.LinkDel(vethHost))
	}, nil
}

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

func prepareFirewall() error {
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

	var inputChain *nftables.Chain
	for _, ch := range chains {
		if ch.Table.Name != nftTable {
			continue
		}
		if ch.Name == nftChainInput {
			inputChain = ch
			break
		}
	}

	if inputChain == nil {
		inputChain = c.AddChain(&nftables.Chain{
			Name:     nftChainInput,
			Table:    table,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookInput,
			Priority: nftables.ChainPriorityFilter,
			Policy:   &policyDrop,
		})

		c.AddRule(&nftables.Rule{
			Table: table,
			Chain: inputChain,
			Exprs: []expr.Any{
				&expr.Ct{Register: 1, SourceRegister: false, Key: expr.CtKeySTATE},
				&expr.Bitwise{
					SourceRegister: 1,
					DestRegister:   1,
					Len:            4,
					Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
					Xor:            binaryutil.NativeEndian.PutUint32(0),
				},
				&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x00, 0x00, 0x00, 0x00}},
				&expr.Counter{},
				&expr.Verdict{
					Kind: expr.VerdictAccept,
				},
			},
		})
		c.AddRule(&nftables.Rule{
			Table: table,
			Chain: inputChain,
			Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     []byte("lo\x00"),
				},
				&expr.Counter{},
				&expr.Verdict{
					Kind: expr.VerdictAccept,
				},
			},
		})
	}

	c.AddChain(&nftables.Chain{
		Name:     nftChainOutput,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policyAccept,
	})
	c.AddChain(&nftables.Chain{
		Name:     nftChainForward,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policyDrop,
	})
	c.AddChain(&nftables.Chain{
		Name:     nftChainPostrouting,
		Table:    table,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Type:     nftables.ChainTypeNAT,
		Policy:   &policyAccept,
	})

	return errors.WithStack(c.Flush())
}

func enableForwarding() error {
	return errors.WithStack(os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o600))
}

func deleteBridge(network *net.IPNet) error {
	bridge := bridgeName(network)
	_, err := net.InterfaceByName(bridge)
	if err != nil {
		return nil
	}

	bridgeLink, err := netlink.LinkByName(bridge)
	if err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(netlink.LinkDel(bridgeLink))
}

func configureFirewall(network *net.IPNet) error {
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

	var forwardChain *nftables.Chain
	var postroutingChain *nftables.Chain
	for _, ch := range chains {
		if ch.Table.Name != nftTable {
			continue
		}
		switch ch.Name {
		case nftChainForward:
			forwardChain = ch
		case nftChainPostrouting:
			postroutingChain = ch
		}
		if forwardChain != nil && postroutingChain != nil {
			break
		}
	}

	if forwardChain == nil {
		return errors.Errorf("chain %s does not exist", nftChainForward)
	}
	if postroutingChain == nil {
		return errors.Errorf("chain %s does not exist", nftChainPostrouting)
	}

	bridge := bridgeName(network)
	netAddr := netIP(network)
	c.AddRule(&nftables.Rule{
		Table:    table,
		Chain:    forwardChain,
		UserData: netAddr,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte(bridge + "\x00"),
			},
			&expr.Ct{Register: 1, SourceRegister: false, Key: expr.CtKeySTATE},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
				Xor:            binaryutil.NativeEndian.PutUint32(0),
			},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x00, 0x00, 0x00, 0x00}},
			&expr.Counter{},
			&expr.Verdict{
				Kind: expr.VerdictAccept,
			},
		},
	})
	c.AddRule(&nftables.Rule{
		Table:    forwardChain.Table,
		Chain:    forwardChain,
		UserData: netAddr,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte(bridge + "\x00"),
			},
			&expr.Counter{},
			&expr.Verdict{
				Kind: expr.VerdictAccept,
			},
		},
	})

	c.AddRule(&nftables.Rule{
		Table:    table,
		Chain:    postroutingChain,
		UserData: netAddr,
		Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     []byte(bridge + "\x00"),
			},
			&expr.Counter{},
			&expr.Masq{},
		},
	})

	return errors.WithStack(c.Flush())
}

func cleanFirewall(network *net.IPNet) error {
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

	netAddr := netIP(network)
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
			if !bytes.Equal(netIP(&net.IPNet{IP: r.UserData, Mask: network.Mask}), netAddr) {
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

func netSize(networkNet *net.IPNet) uint32 {
	ones, bits := networkNet.Mask.Size()
	return uint32(1 << (bits - ones))
}

func bridgeName(ip *net.IPNet) string {
	return "islbr" + hex.EncodeToString(netIP(ip))
}

func vethName(ip net.IP) string {
	return "islve" + hex.EncodeToString(ip.To4())
}
