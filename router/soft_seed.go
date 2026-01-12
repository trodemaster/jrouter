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
	"time"

	"drjosh.dev/jrouter/atalk"
	"drjosh.dev/jrouter/atalk/zip"
	"github.com/google/gopacket/pcap"
	"github.com/sfiera/multitalk/pkg/ddp"
	"github.com/sfiera/multitalk/pkg/ethertalk"
)

const (
	// GetNetInfo query timeout
	getNetInfoTimeout = 10 * time.Second

	// Number of GetNetInfo query retries
	getNetInfoRetries = 3

	// Interval between retries
	getNetInfoRetryInterval = 3 * time.Second
)

// SoftSeedResult contains the network information learned from a seed router.
type SoftSeedResult struct {
	NetStart        ddp.Network
	NetEnd          ddp.Network
	ZoneName        string
	DefaultZoneName string
	OnlyOneZone     bool
}

// InitializeSoftSeed queries a seed router for network configuration.
// This is used in soft-seed and non-seed modes to learn network parameters.
func (port *EtherTalkPort) InitializeSoftSeed(ctx context.Context) (*SoftSeedResult, error) {
	port.logger.Info("Soft-seed: Starting network discovery",
		"mode", port.routerMode,
		"configured-zone", port.configuredZone)

	// Create a temporary context with timeout for the entire operation
	totalTimeout := time.Duration(getNetInfoRetries) * (getNetInfoTimeout + getNetInfoRetryInterval)
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= getNetInfoRetries; attempt++ {
		port.logger.Debug("Soft-seed: Sending GetNetInfo query",
			"attempt", attempt,
			"zone", port.configuredZone)

		result, err := port.sendGetNetInfoQuery(ctx)
		if err == nil {
			port.logger.Info("Soft-seed: Received network configuration",
				"net-start", result.NetStart,
				"net-end", result.NetEnd,
				"zone", result.ZoneName,
				"default-zone", result.DefaultZoneName)

			// Update port configuration with learned values
			port.netStart = result.NetStart
			port.netEnd = result.NetEnd
			if result.DefaultZoneName != "" {
				port.defaultZoneName = result.DefaultZoneName
			} else {
				port.defaultZoneName = result.ZoneName
			}
			port.availableZones = MakeSet(port.defaultZoneName)
			port.networkLearned = true

			// Add routes now that we have learned the network config
			if _, err := port.router.RouteTable.UpsertRoute(port, true /* extended */, port.netStart, port.netEnd, 0); err != nil {
				return nil, fmt.Errorf("couldn't create route for soft-seed port: %w", err)
			}
			if err := port.router.RouteTable.AddZonesToNetwork(port.netStart, port.defaultZoneName); err != nil {
				return nil, fmt.Errorf("couldn't add zones to route: %w", err)
			}

			return result, nil
		}

		lastErr = err
		port.logger.Warn("Soft-seed: GetNetInfo query failed",
			"attempt", attempt,
			"error", err)

		if attempt < getNetInfoRetries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(getNetInfoRetryInterval):
			}
		}
	}

	// For soft-seed mode, fall back to seed mode if no response
	if port.routerMode == RouterModeSoftSeed {
		port.logger.Warn("Soft-seed: No seed router found, falling back to seed mode",
			"net-start", port.netStart,
			"net-end", port.netEnd,
			"zone", port.configuredZone)

		// Use configured values and act as seed
		if port.netStart == 0 || port.netEnd == 0 {
			return nil, fmt.Errorf("soft-seed fallback requires net_start and net_end to be configured")
		}

		port.defaultZoneName = port.configuredZone
		port.availableZones = MakeSet(port.defaultZoneName)

		// Add routes with configured values
		if _, err := port.router.RouteTable.UpsertRoute(port, true /* extended */, port.netStart, port.netEnd, 0); err != nil {
			return nil, fmt.Errorf("couldn't create route for soft-seed fallback: %w", err)
		}
		if err := port.router.RouteTable.AddZonesToNetwork(port.netStart, port.defaultZoneName); err != nil {
			return nil, fmt.Errorf("couldn't add zones to route: %w", err)
		}

		return &SoftSeedResult{
			NetStart:        port.netStart,
			NetEnd:          port.netEnd,
			ZoneName:        port.configuredZone,
			DefaultZoneName: port.configuredZone,
			OnlyOneZone:     true,
		}, nil
	}

	// For non-seed mode, fail if no seed router responded
	return nil, fmt.Errorf("non-seed mode: no seed router responded after %d attempts: %w", getNetInfoRetries, lastErr)
}

// sendGetNetInfoQuery sends a single GetNetInfo query and waits for a reply.
func (port *EtherTalkPort) sendGetNetInfoQuery(ctx context.Context) (*SoftSeedResult, error) {
	// Build the GetNetInfo query packet
	queryData := make([]byte, 7+len(port.configuredZone))
	queryData[0] = zip.FunctionGetNetInfo // ZIP command: GetNetInfo (5)
	queryData[1] = 0                       // Flags: reserved
	queryData[2] = 0                       // Reserved
	queryData[3] = 0                       // Reserved
	queryData[4] = 0                       // Reserved
	queryData[5] = 0                       // Reserved
	queryData[6] = byte(len(port.configuredZone))
	copy(queryData[7:], port.configuredZone)

	// Create DDP packet
	// For GetNetInfo, we use network 0 (any network) and broadcast
	ddpPkt := &ddp.ExtPacket{
		ExtHeader: ddp.ExtHeader{
			Size:      uint16(len(queryData)) + atalk.DDPExtHeaderSize,
			Cksum:     0,
			DstNet:    0,    // Any network
			DstNode:   0xFF, // Broadcast
			DstSocket: 6,    // ZIP socket
			SrcNet:    0,    // Unknown yet
			SrcNode:   0,    // Unknown yet
			SrcSocket: 6,    // ZIP socket
			Proto:     ddp.ProtoZIP,
		},
		Data: queryData,
	}

	// Marshal to EtherTalk frame
	frame, err := ethertalk.AppleTalk(port.ethernetAddr, *ddpPkt)
	if err != nil {
		return nil, fmt.Errorf("couldn't create EtherTalk frame: %w", err)
	}
	frame.Dst = ethertalk.AppleTalkBroadcast

	frameRaw, err := ethertalk.Marshal(*frame)
	if err != nil {
		return nil, fmt.Errorf("couldn't marshal EtherTalk frame: %w", err)
	}
	if len(frameRaw) < 64 {
		frameRaw = append(frameRaw, make([]byte, 64-len(frameRaw))...)
	}

	// Send the query
	if err := port.pcapHandle.WritePacketData(frameRaw); err != nil {
		return nil, fmt.Errorf("couldn't send GetNetInfo query: %w", err)
	}

	port.logger.Debug("Soft-seed: Sent GetNetInfo query, waiting for reply")

	// Wait for reply with timeout
	timeout := time.NewTimer(getNetInfoTimeout)
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, errors.New("timeout waiting for GetNetInfo reply")
		default:
		}

		// Read packet with short timeout
		rawPkt, _, err := port.pcapHandle.ReadPacketData()
		if errors.Is(err, pcap.NextErrorTimeoutExpired) {
			continue
		}
		if errors.Is(err, io.EOF) || errors.Is(err, pcap.NextErrorNoMorePackets) {
			return nil, errors.New("pcap handle closed")
		}
		if err != nil {
			return nil, fmt.Errorf("couldn't read packet: %w", err)
		}

		// Try to parse as EtherTalk
		ethFrame := new(ethertalk.Packet)
		if err := ethertalk.Unmarshal(rawPkt, ethFrame); err != nil {
			continue // Not a valid EtherTalk frame
		}

		// Skip our own packets
		if ethFrame.Src == port.ethernetAddr {
			continue
		}

		// Must be AppleTalk protocol
		if ethFrame.SNAPProto != ethertalk.AppleTalkProto {
			continue
		}

		// Parse DDP
		payload := ethFrame.Payload
		if len(payload) < 2 {
			continue
		}
		if size := binary.BigEndian.Uint16(payload[:2]) & 0x3ff; len(payload) > int(size) {
			payload = payload[:size]
		}

		ddpkt := new(ddp.ExtPacket)
		if err := ddp.ExtUnmarshal(payload, ddpkt); err != nil {
			continue
		}

		// Must be ZIP protocol on socket 6
		if ddpkt.Proto != ddp.ProtoZIP || ddpkt.SrcSocket != 6 {
			continue
		}

		// Parse ZIP packet
		if len(ddpkt.Data) < 1 {
			continue
		}

		// Check if it's a GetNetInfo reply
		if ddpkt.Data[0] != zip.FunctionGetNetInfoReply {
			continue
		}

		// Parse the reply
		reply, err := parseGetNetInfoReply(ddpkt.Data)
		if err != nil {
			port.logger.Debug("Soft-seed: Couldn't parse GetNetInfo reply", "error", err)
			continue
		}

		port.logger.Debug("Soft-seed: Received GetNetInfo reply",
			"from", fmt.Sprintf("%d.%d", ddpkt.SrcNet, ddpkt.SrcNode),
			"net-start", reply.NetStart,
			"net-end", reply.NetEnd,
			"zone", reply.ZoneName)

		return reply, nil
	}
}

// parseGetNetInfoReply parses a ZIP GetNetInfo Reply packet.
func parseGetNetInfoReply(data []byte) (*SoftSeedResult, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("insufficient length %d for GetNetInfo reply", len(data))
	}

	result := &SoftSeedResult{}

	// Parse flags (byte 1)
	flags := data[1]
	zoneInvalid := (flags & 0x80) != 0
	result.OnlyOneZone = (flags & 0x20) != 0

	// Parse network range (bytes 2-5)
	result.NetStart = ddp.Network(binary.BigEndian.Uint16(data[2:4]))
	result.NetEnd = ddp.Network(binary.BigEndian.Uint16(data[4:6]))

	// Parse zone name
	offset := 6
	if offset >= len(data) {
		return nil, fmt.Errorf("packet too short for zone name length")
	}
	zoneLen := int(data[offset])
	offset++

	if zoneLen > 32 || offset+zoneLen > len(data) {
		return nil, fmt.Errorf("invalid zone name length: %d", zoneLen)
	}
	result.ZoneName = string(data[offset : offset+zoneLen])
	offset += zoneLen

	// Skip multicast address (1 byte length + 6 bytes address)
	if offset >= len(data) {
		return nil, fmt.Errorf("packet too short for multicast address")
	}
	multicastLen := int(data[offset])
	offset++
	if multicastLen != 6 || offset+6 > len(data) {
		return nil, fmt.Errorf("invalid multicast address length: %d", multicastLen)
	}
	offset += 6

	// If zone is invalid, parse default zone
	if zoneInvalid {
		if offset >= len(data) {
			return nil, fmt.Errorf("packet too short for default zone length")
		}
		defaultZoneLen := int(data[offset])
		offset++

		if defaultZoneLen > 32 || offset+defaultZoneLen > len(data) {
			return nil, fmt.Errorf("invalid default zone name length: %d", defaultZoneLen)
		}
		result.DefaultZoneName = string(data[offset : offset+defaultZoneLen])
	}

	return result, nil
}
