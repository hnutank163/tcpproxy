package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
	_ "net/http/pprof"
	"net/http"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/songgao/water"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func newTun(name string) *water.Interface {
	t, e := water.NewTUN(name)
	if e != nil {
		log.Panicf("new tun %v", e)
	}

	return t
}

var inx uint64

func onAccept(conn *gonet.TCPConn) {
	remote := conn.RemoteAddr()
	log.Printf("remote is %v, local %v", remote, conn.LocalAddr())

	upconn, err := net.Dial("tcp", "127.0.0.1:12345")
	if err != nil {
		log.Printf("faile to connect upstream %v", err)
		return
	}

	// handshake
	var b []byte
	for i := 0; i < 30; i++ {
		b = append(b, ' ')
	}

	if strings.Contains(conn.LocalAddr().String(), "1314") {
		copy(b, []byte("127.0.0.1:1314"))
	} else if strings.Contains(conn.LocalAddr().String(), "1315") {
		copy(b, []byte("127.0.0.1:1315"))
    } else {
		copy(b, []byte("127.0.0.1:1316"))
    }
	upconn.Write(b)
	r := make([]byte, 2)
	upconn.Read(r)
	if string(r) != "ok" {
		log.Printf("handshake failed")
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		_, err := io.Copy(conn, upconn)
		if err != nil {
			log.Printf("down -> upstream error %v", err)
		}
		wg.Done()
		upconn.Close()
		conn.Close()
	}()

	go func() {
		_, err := io.Copy(upconn, conn)
		if err != nil {
			log.Printf("upstream -> down error %v", err)
		}
		wg.Done()
		upconn.Close()
		conn.Close()
	}()

	wg.Wait()
	log.Printf("shutdown %v -> %v", conn, upconn)

}

var m sync.Map

func tunToChannel(t *water.Interface, ch *channel.Endpoint) {
	go func() {
		for {
			pkt := ch.ReadContext(context.TODO())

			if pkt == nil {
				log.Printf("nil pkt ")
				continue
			}

			v := pkt.Views()
			vV := buffer.NewVectorisedView(pkt.Size(), v)
			b := vV.ToView()

			p := gopacket.NewPacket(b, layers.IPProtocolIPv4, gopacket.Default)
			if p != nil {
				l := p.Layer(layers.LayerTypeIPv4)
				ip := l.(*layers.IPv4)
				t, ok := m.Load(fmt.Sprintf("%s-%s", ip.DstIP.String(), ip.SrcIP.String()))
				if !ok {
					log.Printf("can't find tunnel for %v", ip)
				}

				m.Store(fmt.Sprintf("%s-%s", ip.SrcIP.String(), ip.DstIP.String()), t)
				log.Printf("read from channel %v, %p", len(v), ch)
				_, e := t.(*water.Interface).Write(vV.ToView())
				if e != nil {
					log.Panicf("tun write %v, error %v", vV.ToView(), e)
				}
			}

			pkt.DecRef()
		}

	}()

	for {
		b := make([]byte, 1500)
		n, _ := t.Read(b)
		// var ethHdr header.Ethernet
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			// ReserveHeaderBytes: len(ethHdr),
			Data: buffer.View(b[:n]).ToVectorisedView(),
			// IsForwardedPacket: true,
		})

		if !ch.IsAttached() {
			log.Panic("channel not attach..")
		}

		p := gopacket.NewPacket(b[:n], layers.IPProtocolIPv4, gopacket.Default)
		if p != nil {
			l := p.Layer(layers.LayerTypeIPv4)
			ip := l.(*layers.IPv4)
			m.Store(fmt.Sprintf("%s-%s", ip.SrcIP.String(), ip.DstIP.String()), t)
		}

		log.Printf("write to channel  %p", ch)
		ch.InjectInbound(ipv4.ProtocolNumber, pkt)
		// ch.AddNotify()
		log.Printf("write packet %d", n)
		pkt.DecRef()
	}
}

func main() {
	opts := stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
		},
	}

	s := stack.New(opts)
	s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)
	nicIDs := []tcpip.NICID{tcpip.NICID(s.UniqueID()), tcpip.NICID(s.UniqueID()), tcpip.NICID(s.UniqueID())}
	// s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
	// 	Protocol: ipv4.ProtocolNumber,
	// 	AddressWithPrefix: tcpip.AddressWithPrefix{
	// 		Address:   tcpip.Address("3.0.0.0"),
	// 		PrefixLen: 8,
	// 	},
	// }, stack.AddressProperties{})

	// subnet, err := tcpip.NewSubnet(
	// 	tcpip.Address(string([]byte{192, 168, 0, 0})),
	// 	tcpip.AddressMask(string([]byte{255, 255, 0, 0})))

	ops := []Option{WithDefault()}
	for _, op := range ops {
		op(s)
	}

	opt := tcpip.TCPModerateReceiveBufferOption(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt); err != nil {
	    log.Panic("set opt")

	}

	for _, nicID := range nicIDs {
		s.SetRouteTable([]tcpip.Route{
			{
				Destination: header.IPv4EmptySubnet,
				NIC:         nicID,
			},
		})
	}

	// s.SetForwardingDefaultAndAllNICs(tcp.ProtocolNumber, true)

	tuns := []*water.Interface{newTun("tuntest"), newTun("tuntest2"), newTun("tuntest3")}

	chs := []*channel.Endpoint{channel.New(102400, 1500, "7c:b2:7d:eb:91:63"), channel.New(102400, 1500, "7c:b1:7d:eb:91:63"), channel.New(102400, 1500, "71:b1:7d:eb:91:63")}

	for i := range tuns {
		t, c := tuns[i], chs[i]
		go tunToChannel(t, c)
	}

	var wq waiter.Queue

	forwarder := tcp.NewForwarder(s, 10*1024*1024, 2<<10, func(r *tcp.ForwarderRequest) {
		var (
			ep  tcpip.Endpoint
			err tcpip.Error
			// id  := r.ID()
		)

		log.Printf("on tcp handle...")
		ep, err = r.CreateEndpoint(&wq)
		if err != nil {
			log.Printf("create endpoint error %v", err)
			r.Complete(true)
			return
		}
		defer r.Complete(false)

		log.Printf("create endpoint success")
		conn := gonet.NewTCPConn(&wq, ep)
		log.Printf("new tcp conn success")
		onAccept(conn)

	})

	go startProxy("127.0.0.1:12345")
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, forwarder.HandlePacket)

	for i := range chs {
		e := s.CreateNICWithOptions(nicIDs[i], chs[i], stack.NICOptions{
			Disabled: false,
			QDisc:    nil,
		})
		s.EnableNIC(nicIDs[i])
		if e != nil {
			log.Printf("create nic error %v", e)
		}
		s.SetSpoofing(nicIDs[i], true)
		s.SetPromiscuousMode(nicIDs[i], true)
		log.Printf("create nic success")
	}

	// var wgq waiter.Queue
	// ep, err := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wgq)
	// if err != nil {
	// log.Panicf("new endpoint %v", err)
	// }

	// e = ep.Bind(tcpip.FullAddress{
	// NIC:  nicID,
	// Addr: "",
	// Port: 11111,
	// })
	// if e != nil {
	// log.Panicf("bind error %v", e)
	// }

	// waitEntry, notifyCh := waiter.NewChannelEntry(waiter.EventIn)
	// wgq.EventRegister(&waitEntry)
	// defer wq.EventUnregister(&waitEntry)
	// err = ep.Listen(100)
	// if err != nil {
	//     log.Panicf("ep listen %v", err)
	// }

	// go func() {

	// for {
	// ep, _ , err := ep.Accept(nil)
	// log.Printf("ep accept %v %v", err, ep)
	//         if err != nil {
	// <-notifyCh
	// continue
	//         } else {
	//             log.Printf("ep accept error %v", err)
	//         }

	//         addr, _ := ep.GetLocalAddress()
	// log.Printf("ep accept %v", addr)
	// }
	// }()

    http.ListenAndServe("0.0.0.0:33333", nil)
	for {
		time.Sleep(time.Minute)
	}
}

func startProxy(addr string) {
	listner, err := net.Listen("tcp", addr)
	if err != nil {
		log.Panicf("listen error %v", err)
	}

	for {
		conn, e := listner.Accept()
		if e != nil {
			log.Printf("proxy accept error %v", e)
			continue
		}

		go func(c net.Conn) {
			addr := make([]byte, 30)
			c.Read(addr)

			remote := strings.Trim(string(addr), " ")
			log.Printf("proxy remote is %v", remote)

			upstream, e := net.Dial("tcp", remote)
			if e != nil {
				log.Printf("connect upstream error %v", e)
				return
			}
			c.Write([]byte("ok"))

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				io.Copy(c, upstream)
				wg.Done()
			}()

			go func() {
				io.Copy(upstream, c)
				wg.Done()
			}()

			wg.Wait()

		}(conn)
	}
}
