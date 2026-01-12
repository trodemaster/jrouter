/*
   Copyright 2024 Josh Deprez

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

package router

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"drjosh.dev/jrouter/atalk"
	"drjosh.dev/jrouter/status"
	"github.com/google/gopacket/pcap"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sfiera/multitalk/pkg/ddp"
	"github.com/sfiera/multitalk/pkg/ethernet"
	"github.com/sfiera/multitalk/pkg/ethertalk"
)

// Limit on number of packets buffered while AARP resolves an address
const maxOutboxSize = 1000

// Buffered packets that are too old are dropped.
const outboxTimeout = 10 * time.Second

const outboxStatusTemplate = `
Status: {{.Status}}<br/>
{{with .Outboxes}}
<table>
	<thead><tr>
		<th>DDP addr</th>
		<th>Last packet</th>
		<th>Outbox size</th>
	</tr></thead>
	<tbody>
{{range .}}
		<tr>
			<td>{{.Addr.Network}}.{{.Addr.Node}}</td>
			<td>{{.LastTS | ago}}</td>
			<td>{{.Len}}</td>
		</tr>
{{end}}
	</tbody>
</table>
{{end}}
`

// RouterMode defines how an EtherTalk port operates.
type RouterMode string

const (
	// RouterModeSeed means this port is authoritative for network/zone config.
	RouterModeSeed RouterMode = "seed"

	// RouterModeSoftSeed means this port queries for config but can seed if none available.
	RouterModeSoftSeed RouterMode = "soft-seed"

	// RouterModeNonSeed means this port must query for config (fails if none available).
	RouterModeNonSeed RouterMode = "non-seed"
)

// EtherTalkPort is all the data and helpers needed for EtherTalk on one port.
type EtherTalkPort struct {
	// General references to broader things
	router *Router

	// Port-specific things
	logger          *slog.Logger
	pcapHandle      *pcap.Handle
	aarpMachine     *AARPMachine
	myAddr          ddp.Addr
	device          string
	ethernetAddr    ethernet.Addr
	netStart        ddp.Network
	netEnd          ddp.Network
	defaultZoneName string
	availableZones  Set[string]

	// Router mode configuration
	routerMode       RouterMode
	configuredZone   string      // Zone from config (for validation in soft-seed)
	seedRouterAddr   ddp.Addr    // Specific seed router to query (optional)
	networkLearned   bool        // True if network info was learned from seed router

	// Outbound packet queueing
	outboxesMu        sync.Mutex
	outboxes          map[<-chan struct{}]*outbox
	outboxesChangedCh chan struct{}
}

// EtherTalkPortConfig holds configuration for creating an EtherTalk port.
type EtherTalkPortConfig struct {
	Device          string
	EthernetAddr    ethernet.Addr
	NetStart        ddp.Network
	NetEnd          ddp.Network
	DefaultZoneName string
	AvailableZones  Set[string]
	PcapHandle      *pcap.Handle
	RouterMode      RouterMode
	SeedRouterAddr  ddp.Addr // Optional: specific seed router to query
}

// NewEtherTalkPort defines a new EtherTalk port for the router.
func (router *Router) NewEtherTalkPort(
	device string,
	ethernetAddr ethernet.Addr,
	netStart ddp.Network,
	netEnd ddp.Network,
	defaultZoneName string,
	availableZones Set[string],
	pcapHandle *pcap.Handle) *EtherTalkPort {

	return router.NewEtherTalkPortWithConfig(EtherTalkPortConfig{
		Device:          device,
		EthernetAddr:    ethernetAddr,
		NetStart:        netStart,
		NetEnd:          netEnd,
		DefaultZoneName: defaultZoneName,
		AvailableZones:  availableZones,
		PcapHandle:      pcapHandle,
		RouterMode:      RouterModeSeed, // Default to seed mode for backward compatibility
	})
}

// NewEtherTalkPortWithConfig defines a new EtherTalk port with full configuration.
func (router *Router) NewEtherTalkPortWithConfig(cfg EtherTalkPortConfig) *EtherTalkPort {
	mode := cfg.RouterMode
	if mode == "" {
		mode = RouterModeSeed
	}

	port := &EtherTalkPort{
		// Add router to port
		router: router,

		logger:          router.Logger.With("device", cfg.Device),
		device:          cfg.Device,
		ethernetAddr:    cfg.EthernetAddr,
		netStart:        cfg.NetStart,
		netEnd:          cfg.NetEnd,
		defaultZoneName: cfg.DefaultZoneName,
		availableZones:  cfg.AvailableZones,
		pcapHandle:      cfg.PcapHandle,

		routerMode:       mode,
		configuredZone:   cfg.DefaultZoneName,
		seedRouterAddr:   cfg.SeedRouterAddr,
		networkLearned:   false,

		outboxes:          make(map[<-chan struct{}]*outbox),
		outboxesChangedCh: make(chan struct{}, 1),
	}
	// Add port to router
	router.Ports = append(router.Ports, port)

	// Add AARP to port
	port.aarpMachine = NewAARPMachine(port.logger, port, cfg.EthernetAddr)

	// For seed mode, immediately add routes. For soft-seed/non-seed, routes are
	// added after learning from the seed router.
	if mode == RouterModeSeed {
		if _, err := router.RouteTable.UpsertRoute(port, true /* extended */, cfg.NetStart, cfg.NetEnd, 0); err != nil {
			port.logger.Error("Couldn't create route for EtherTalk port", "error", err)
			os.Exit(1)
		}
		if err := router.RouteTable.AddZonesToNetwork(cfg.NetStart, cfg.AvailableZones.ToSlice()...); err != nil {
			port.logger.Error("Couldn't add zones to route that was just created", "error", err)
			os.Exit(1)
		}
	}

	return port
}

// IsSeedRouter returns true if this port is operating as a seed router.
func (port *EtherTalkPort) IsSeedRouter() bool {
	return port.routerMode == RouterModeSeed ||
		(port.routerMode == RouterModeSoftSeed && !port.networkLearned)
}

// GetRouterMode returns the router mode for this port.
func (port *EtherTalkPort) GetRouterMode() RouterMode {
	return port.routerMode
}

// GetDevice returns the device name for this port.
func (port *EtherTalkPort) GetDevice() string {
	return port.device
}

// Outbox runs a loop that waits for AARP resolutions to complete, and once they
// are, transmit packets that were buffered.
func (port *EtherTalkPort) Outbox(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	var statusStr atomic.Value
	statusStr.Store("Initialising")
	ctx, _ = status.AddItem(ctx,
		"Outbound",
		outboxStatusTemplate,
		func(ctx context.Context) (any, error) {
			return map[string]any{
				"Status":   statusStr.Load(),
				"Outboxes": port.dumpOutboxes(),
			}, nil
		},
	)
	defer statusStr.Store("EtherTalk Outbox goroutine exited!")

	resolveTicker := time.NewTicker(aarpRequestRetransmit)
	defer resolveTicker.Stop()

	for {
		// First time I'm using [reflect.Select]!
		cases := []reflect.SelectCase{
			0: {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
			1: {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(port.outboxesChangedCh)},
		}
		func() {
			port.outboxesMu.Lock()
			defer port.outboxesMu.Unlock()

			if len(port.outboxes) == 0 {
				statusStr.Store("No outboxes pending")
				return
			}

			statusStr.Store(fmt.Sprintf("Waiting on %d address resolutions", len(port.outboxes)))
			// 2: case <-resolveTicker.C
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(resolveTicker.C)})
			// 3 to N: case <-waitCh
			for waitCh := range port.outboxes {
				cases = append(cases, reflect.SelectCase{
					Dir: reflect.SelectRecv, Chan: reflect.ValueOf(waitCh),
				})
			}
		}()

		chosen, _, _ := reflect.Select(cases)
		switch chosen {
		case 0: // <-ctx.Done()
			statusStr.Store("Context cancelled!")
			return

		case 1: // <-port.outboxesChangedCh:
			// Rebuild the list of channels to wait on.
			statusStr.Store("Refreshing pending outboxes...")
			continue

		case 2: // <-resolveTicker.C
			statusStr.Store("Resending AARP requests...")
			if err := port.resendAARPRequests(); err != nil {
				port.logger.Error("EtherTalk/AARP: broadcasting request", "error", err)
				return
			}
			continue

		default:
			// continue below
		}

		ob, destEth := port.outboxPop(cases[chosen].Chan.Interface().(<-chan struct{}))
		if ob == nil || len(ob.packets) == 0 {
			continue
		}

		statusStr.Store(fmt.Sprintf("Sending buffered packets to %d.%d at %v...", ob.addr.Network, ob.addr.Node, destEth))
		for _, pkt := range ob.packets {
			if err := port.send(destEth, pkt); err != nil {
				port.logger.Error("EtherTalk: couldn't send packet", "error", err)
				return
			}
		}
	}
}

// Serve runs a loop that reads AARP or AppleTalk packets from the network
// device, and handles them.
func (port *EtherTalkPort) Serve(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	ctx, setStatus, _ := status.AddSimpleItem(ctx, "Inbound")
	defer setStatus("EtherTalk Serve goroutine exited!")
	setStatus(fmt.Sprintf("Listening on %s", port.device))

	for {
		if ctx.Err() != nil {
			return
		}

		rawPkt, _, err := port.pcapHandle.ReadPacketData()
		if errors.Is(err, pcap.NextErrorTimeoutExpired) {
			continue
		}
		if errors.Is(err, io.EOF) || errors.Is(err, pcap.NextErrorNoMorePackets) {
			return
		}
		if err != nil {
			port.logger.Error("Couldn't read AppleTalk / AARP packet data", "error", err)
			return
		}

		ethFrame := new(ethertalk.Packet)
		if err := ethertalk.Unmarshal(rawPkt, ethFrame); err != nil {
			atalkInvalidPacketsInCounter.With(prometheus.Labels{"port": port.device}).Inc()
			port.logger.Error("Couldn't unmarshal EtherTalk frame", "error", err)
			continue
		}

		// Ignore if sent by me
		if ethFrame.Src == port.ethernetAddr {
			continue
		}

		switch ethFrame.SNAPProto {
		case ethertalk.AARPProto:
			// port.Logger.Debug("Got an AARP frame")
			promLabels := prometheus.Labels{"port": port.device}
			aarpPacketsInCounter.With(promLabels).Inc()
			aarpBytesInCounter.With(promLabels).Add(float64(len(rawPkt)))

			port.aarpMachine.Handle(ctx, ethFrame)

		case ethertalk.AppleTalkProto:
			// port.Logger.Debug("Got an AppleTalk frame")

			// Workaround for strict length checking in sfiera/multitalk
			payload := ethFrame.Payload
			if len(payload) < 2 {
				port.logger.Error("Couldn't unmarshal DDP packet: too small", "payload-length", len(payload))
			}
			if size := binary.BigEndian.Uint16(payload[:2]) & 0x3ff; len(payload) > int(size) {
				payload = payload[:size]
			}

			ddpkt := new(ddp.ExtPacket)
			if err := ddp.ExtUnmarshal(payload, ddpkt); err != nil {
				port.logger.Error("Couldn't unmarshal DDP packet", "error", err)
				continue
			}

			promLabels := prometheus.Labels{
				"port":       port.device,
				"src_net":    strconv.Itoa(int(ddpkt.SrcNet)),
				"src_node":   strconv.Itoa(int(ddpkt.SrcNode)),
				"src_socket": strconv.Itoa(int(ddpkt.SrcSocket)),
				"dst_net":    strconv.Itoa(int(ddpkt.DstNet)),
				"dst_node":   strconv.Itoa(int(ddpkt.DstNode)),
				"dst_socket": strconv.Itoa(int(ddpkt.DstSocket)),
				"proto":      strconv.Itoa(int(ddpkt.Proto)),
			}
			atalkPacketsInCounter.With(promLabels).Inc()
			atalkBytesInCounter.With(promLabels).Add(float64(len(rawPkt)))

			// port.Logger.Debug(fmt.Sprintf("DDP: src (%d.%d s %d) dst (%d.%d s %d) proto %d data len %d",
			// 	ddpkt.SrcNet, ddpkt.SrcNode, ddpkt.SrcSocket,
			// 	ddpkt.DstNet, ddpkt.DstNode, ddpkt.DstSocket,
			// 	ddpkt.Proto, len(ddpkt.Data)))

			// Glean address info for AMT, but only if SrcNet is our net
			// (If it's not our net, then it was routed from elsewhere, and
			// we'd be filling the AMT with entries for a router.)
			if ddpkt.SrcNet >= port.netStart && ddpkt.SrcNet <= port.netEnd {
				srcAddr := ddp.Addr{Network: ddpkt.SrcNet, Node: ddpkt.SrcNode}
				port.aarpMachine.learn(srcAddr, ethFrame.Src)
				// port.Logger.Debug(fmt.Sprintf("DDP: Gleaned that %d.%d -> %v", srcAddr.Network, srcAddr.Node, ethFrame.Src))
			}

			// Packet for us? First, who am I?
			myAddr, ok := port.aarpMachine.Address()
			if !ok {
				continue
			}
			port.myAddr = myAddr.Proto

			// Our network?
			// "The network number 0 is reserved to mean unknown; by default
			// it specifies the local network to which the node is
			// connected. Packets whose destination network number is 0 are
			// addressed to a node on the local network."
			// TODO: more generic routing
			if ddpkt.DstNet != 0 && !(ddpkt.DstNet >= port.netStart && ddpkt.DstNet <= port.netEnd) {
				// Is it for a network in the routing table?
				if err := port.router.Forward(ctx, ddpkt); err != nil {
					port.logger.Error("DDP: Couldn't forward packet", "error", err)
				}
				continue
			}

			// To me?
			// "Node ID 0 indicates any router on the network"- I'm a router
			// "node ID $FF indicates either a network-wide or zone-specific
			// broadcast"- that's relevant
			if ddpkt.DstNode != 0 && ddpkt.DstNode != 0xff && ddpkt.DstNode != myAddr.Proto.Node {
				continue
			}

			switch ddpkt.DstSocket {
			case 1: // The RTMP socket
				if err := port.HandleRTMP(ctx, ddpkt); err != nil {
					port.logger.Error("RTMP: Couldn't handle packet", "error", err)
				}

			case 2: // The NIS (name information socket / NBP socket)
				if err := port.HandleNBP(ctx, ddpkt); err != nil {
					port.logger.Error("NBP: Couldn't handle packet", "error", err)
				}

			case 4: // The AEP socket
				if err := port.router.HandleAEP(ctx, ddpkt); err != nil {
					port.logger.Error("AEP: Couldn't handle packet", "error", err)
				}

			case 6: // The ZIS (zone information socket / ZIP socket)
				if err := port.HandleZIP(ctx, ddpkt); err != nil {
					port.logger.Error("ZIP: Couldn't handle packet", "error", err)
				}

			default:
				port.logger.Error("DDP: No handler for socket", "dst-socket", ddpkt.DstSocket)
			}

		default:
			port.logger.Error("Read unknown packet",
				"ethernet-src", ethFrame.Src,
				"ethernet-dst", ethFrame.Dst,
				"payload", ethFrame.Payload,
			)

		}
	}
}

// Send sends a DDP packet out this port to the destination node.
// If pkt.DstNode = 0xFF, then the packet is broadcast.
func (port *EtherTalkPort) Send(ctx context.Context, pkt *ddp.ExtPacket) error {
	if pkt.DstNode == 0xFF {
		return port.send(ethertalk.AppleTalkBroadcast, pkt)
	}

	addr := ddp.Addr{Network: pkt.DstNet, Node: pkt.DstNode}
	destEth, waitCh := port.aarpMachine.lookupOrWait(addr)
	if waitCh == nil {
		// Cached address still valid
		return port.send(destEth, pkt)
	}

	port.outboxPush(waitCh, addr, pkt)
	return nil
}

// RunAARP runs the AARP state machine.
func (port *EtherTalkPort) RunAARP(ctx context.Context) (err error) {
	return port.aarpMachine.Run(ctx)
}

// Broadcast broadcasts the DDP packet on this port.
func (port *EtherTalkPort) Broadcast(pkt *ddp.ExtPacket) error {
	return port.send(ethertalk.AppleTalkBroadcast, pkt)
}

// ZoneMulticast broadcasts the DDP packet to a zone multicast hwaddr.
// The specific address used is computed from the zone name.
func (port *EtherTalkPort) ZoneMulticast(zone string, pkt *ddp.ExtPacket) error {
	return port.send(atalk.MulticastAddr(zone), pkt)
}

// Forward is another name for Send.
// (EtherTalk ports can be used as a route target.)
func (port *EtherTalkPort) Forward(ctx context.Context, pkt *ddp.ExtPacket) error {
	return port.Send(ctx, pkt)
}

// RouteTargetKey returns "EtherTalkPort|device name".
func (port *EtherTalkPort) RouteTargetKey() string {
	return "EtherTalkPort|" + port.device
}

// Class returns TargetClassDirect.
func (port *EtherTalkPort) Class() TargetClass { return TargetClassDirect }

func (port *EtherTalkPort) String() string {
	return port.device
}

const portStatusTmpl = `Device: {{.Device}}<br/>
Ethernet address: {{.EthernetAddr}}<br/>
AppleTalk network: {{.NetStart}}{{if ne .NetStart .NetEnd}}-{{.NetEnd}}{{end}} (extended)<br/>
Available zones (default zone in bold): <ul>{{range .AvailZones}}<li{{if eq . $.DefaultZone}} style="font-weight:bold"{{end}}>{{.}}</li>{{end}}</ul><br/>`

// StatusCtx returns a context with a new status grouping specifically for this
// port.
func (port *EtherTalkPort) StatusCtx(ctx context.Context) context.Context {
	ctx, _ = status.AddItem(ctx, "EtherTalk on "+port.device, portStatusTmpl, func(context.Context) (any, error) {
		return map[string]any{
			"Device":       port.device,
			"EthernetAddr": port.ethernetAddr,
			"NetStart":     port.netStart,
			"NetEnd":       port.netEnd,
			"DefaultZone":  port.defaultZoneName,
			"AvailZones":   port.availableZones.ToSlice(),
		}, nil
	},
	)
	return ctx
}

// send is used to send EtherTalk packets. dstEth is either the destination node
// or another AppleTalk router that will forward the packet. Or it's a broadcast
// packet and dstEth should be a broadcast hwaddr.
func (port *EtherTalkPort) send(dstEth ethernet.Addr, pkt *ddp.ExtPacket) error {
	outFrame, err := ethertalk.AppleTalk(port.ethernetAddr, *pkt)
	if err != nil {
		return err
	}
	outFrame.Dst = dstEth
	outFrameRaw, err := ethertalk.Marshal(*outFrame)
	if err != nil {
		return err
	}
	if len(outFrameRaw) < 64 {
		outFrameRaw = append(outFrameRaw, make([]byte, 64-len(outFrameRaw))...)
	}

	promLabels := prometheus.Labels{
		"port":       port.device,
		"src_net":    strconv.Itoa(int(pkt.SrcNet)),
		"src_node":   strconv.Itoa(int(pkt.SrcNode)),
		"src_socket": strconv.Itoa(int(pkt.SrcSocket)),
		"dst_net":    strconv.Itoa(int(pkt.DstNet)),
		"dst_node":   strconv.Itoa(int(pkt.DstNode)),
		"dst_socket": strconv.Itoa(int(pkt.DstSocket)),
		"proto":      strconv.Itoa(int(pkt.Proto)),
	}
	atalkPacketsOutCounter.With(promLabels).Inc()
	atalkBytesOutCounter.With(promLabels).Add(float64(len(outFrameRaw)))

	return port.pcapHandle.WritePacketData(outFrameRaw)
}

type outbox struct {
	packets []*ddp.ExtPacket
	addr    ddp.Addr // may the packet address or next-hop router address
	lastTS  time.Time
}

// outboxPush adds packets to the outbox.
func (port *EtherTalkPort) outboxPush(waitCh <-chan struct{}, addr ddp.Addr, pkt *ddp.ExtPacket) {
	now := time.Now()

	port.outboxesMu.Lock()
	defer port.outboxesMu.Unlock()
	ob := port.outboxes[waitCh]
	if ob != nil {
		// Add more packets to the existing outbox.
		ob.packets = append(ob.packets, pkt)
		ob.lastTS = now

		// Drop packets to remain within max size.
		if obLen := len(ob.packets); obLen > maxOutboxSize {
			drop := obLen - maxOutboxSize
			ob.packets = ob.packets[drop:]
			port.logger.Warn("EtherTalk: packet outbox overflow, dropping packets to stay within max size",
				"dst-net", addr.Network,
				"dst-node", addr.Node,
				"drop", drop,
			)
		}
		return
	}

	// Create a new outbox
	ob = &outbox{
		packets: []*ddp.ExtPacket{pkt},
		addr:    addr,
		lastTS:  now,
	}
	port.outboxes[waitCh] = ob

	// New outbox pending AARP resolution, so get the ball rolling.
	if err := port.aarpMachine.request(addr); err != nil {
		port.logger.Error("EtherTalk/AARP: broadcasting request", "error", err)
	}

	// Notify the Outbox goroutine that there's a new outbox.
	// Don't block (when the channel is full, there's already a new outbox,
	// and it creates the whole slice each time).
	select {
	case port.outboxesChangedCh <- struct{}{}:
	default:
	}
}

// outboxPop retrieves an outbox that can be processed.
func (port *EtherTalkPort) outboxPop(waitCh <-chan struct{}) (*outbox, ethernet.Addr) {
	port.outboxesMu.Lock()
	defer port.outboxesMu.Unlock()

	ob := port.outboxes[waitCh]
	delete(port.outboxes, waitCh)

	// it was resolved!
	destEth, newWaitCh := port.aarpMachine.lookupOrWait(ob.addr)
	if newWaitCh == nil {
		return ob, destEth
	}

	// oh no! actually it wasn't resolved...
	// this should pretty much never happen because the AARP
	// timeout is 10x longer than the outbox ticker
	// But drop the outbox if it is stale.
	if time.Since(ob.lastTS) > outboxTimeout {
		port.logger.Warn("EtherTalk: dropping stale buffered packets",
			"dst-net", ob.addr.Network,
			"dst-node", ob.addr.Node,
			"drop", len(ob.packets),
		)
		return nil, destEth
	}
	// Put the outbox back, under the new wait channel.
	port.outboxes[newWaitCh] = ob
	return nil, destEth
}

// resendAARPRequests removes stale outboxes, and retransmits AARP requests for
// every remaining pending outbox.
func (port *EtherTalkPort) resendAARPRequests() error {
	port.outboxesMu.Lock()
	defer port.outboxesMu.Unlock()
	for waitCh, ob := range port.outboxes {
		// Drop stale outboxes.
		if time.Since(ob.lastTS) > outboxTimeout {
			delete(port.outboxes, waitCh)
			port.logger.Warn("EtherTalk: dropping stale buffered packets",
				"dst-net", ob.addr.Network,
				"dst-node", ob.addr.Node,
				"drop", len(ob.packets),
			)
			return nil
		}
		// Outbox not stale, so re-send AARP request.
		if err := port.aarpMachine.request(ob.addr); err != nil {
			return err
		}
	}
	return nil
}

type outboxSummary struct {
	Addr   ddp.Addr
	LastTS time.Time
	Len    int
}

// dumpOutboxes creates a slice of outbox summaries.
func (port *EtherTalkPort) dumpOutboxes() []outboxSummary {
	port.outboxesMu.Lock()
	defer port.outboxesMu.Unlock()
	obs := make([]outboxSummary, 0, len(port.outboxes))
	for _, ob := range port.outboxes {
		obs = append(obs, outboxSummary{
			Addr:   ob.addr,
			LastTS: ob.lastTS,
			Len:    len(ob.packets),
		})
	}
	return obs
}
