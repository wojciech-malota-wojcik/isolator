package firewall

import (
	"math"
	"net"

	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// Expressions combines expressions into a single list.
func Expressions(exprs ...[]expr.Any) []expr.Any {
	res := []expr.Any{}

	for _, e := range exprs {
		res = append(res, e...)
	}

	return res
}

// Accept accepts packets.
func Accept() []expr.Any {
	return []expr.Any{
		&expr.Counter{},
		&expr.Verdict{
			Kind: expr.VerdictAccept,
		},
	}
}

// Masquerade masquerades packets.
func Masquerade() []expr.Any {
	return []expr.Any{
		&expr.Counter{},
		&expr.Masq{},
	}
}

// DestinationNAT redirects packets to IP and port.
func DestinationNAT(ip net.IP, port uint16) []expr.Any {
	return []expr.Any{
		&expr.Immediate{
			Register: 1,
			Data:     ip.To4(),
		},
		&expr.Immediate{
			Register: 2,
			Data:     binaryutil.BigEndian.PutUint16(port),
		},
		&expr.Counter{},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  1,
			RegProtoMin: 2,
		},
	}
}

// ConnectionEstablished filters established connections.
func ConnectionEstablished() []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, SourceRegister: false, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x00, 0x00, 0x00, 0x00}},
	}
}

// IncomingInterface filters incoming interface.
func IncomingInterface(iface string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte(iface + "\x00"),
		},
	}
}

// Outgoing interface filters outgoing interface.
func OutgoingInterface(iface string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte(iface + "\x00"),
		},
	}
}

// LocalSourceAddress filters local source addresses.
func LocalSourceAddress() []expr.Any {
	return []expr.Any{
		&expr.Fib{
			Register:       1,
			FlagSADDR:      true,
			ResultADDRTYPE: true,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.NativeEndian.PutUint32(2),
		},
	}
}

// SourceNetwork filters traffic comming from network.
func SourceNetwork(network *net.IPNet) []expr.Any {
	if network.IP.Equal(net.IPv4zero) {
		return nil
	}

	return []expr.Any{
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12,
			Len:          4,
		},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           ipMask(network),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     netIP(network),
		},
	}
}

// NotSourceAddress filters out packets coming from IP.
func NotSourceAddress(ip net.IP) []expr.Any {
	if ip.Equal(net.IPv4zero) {
		return nil
	}

	return []expr.Any{
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12,
			Len:          4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 1,
			Data:     ip.To4(),
		},
	}
}

// DestinationAddress filters destination address.
func DestinationAddress(ip net.IP) []expr.Any {
	if ip.Equal(net.IPv4zero) {
		return nil
	}

	return []expr.Any{
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       16,
			Len:          4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ip.To4(),
		},
	}
}

// Protocol filters protocol.
func Protocol(protocol string) []expr.Any {
	var proto byte
	switch protocol {
	case "tcp":
		proto = unix.IPPROTO_TCP
	case "udp":
		proto = unix.IPPROTO_UDP
	default:
		panic(errors.Errorf("unknown proto %q", protocol))
	}

	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{proto},
		},
	}
}

// DestinationPort filters destination port.
func DestinationPort(port uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     binaryutil.BigEndian.PutUint16(port),
		},
	}
}

func ip4ToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP4(val uint32) net.IP {
	return net.IPv4(byte(val>>24), byte(val>>16), byte(val>>8), byte(val)).To4()
}

func netIP(network *net.IPNet) net.IP {
	ones, bits := network.Mask.Size()
	return uint32ToIP4(ip4ToUint32(network.IP) & (uint32(math.MaxUint32) << (bits - ones)))
}

func ipMask(ip *net.IPNet) []byte {
	ones, bits := ip.Mask.Size()
	return binaryutil.BigEndian.PutUint32(math.MaxUint32 << (bits - ones))
}
