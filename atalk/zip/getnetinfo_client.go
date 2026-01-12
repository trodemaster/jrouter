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

package zip

import (
	"context"
	"fmt"
	"time"

	"github.com/sfiera/multitalk/pkg/ddp"
)

// GetNetInfoClient handles querying seed routers for network configuration.
type GetNetInfoClient struct {
	// Socket is the DDP socket to use for queries (typically socket 6)
	Socket ddp.Socket

	// ZoneName is the zone name to include in queries (for validation)
	ZoneName string

	// SeedRouter is the address of a specific seed router to query.
	// If zero, queries are broadcast to all routers.
	SeedRouter ddp.Addr

	// MaxRetries is the number of times to retry the query. Default: 3
	MaxRetries int

	// Timeout is the timeout for each query attempt. Default: 10 seconds
	Timeout time.Duration
}

// Query sends a ZIP GetNetInfo query and waits for a reply.
// Returns the reply or an error if no reply is received within the timeout period.
func (c *GetNetInfoClient) Query(ctx context.Context) (*GetNetInfoReplyPacket, error) {
	if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}

	// Create query packet
	query := &GetNetInfoPacket{
		ZoneName: c.ZoneName,
	}

	queryBytes, err := query.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal GetNetInfo query: %w", err)
	}

	// Destination address
	dest := c.SeedRouter
	if dest == (ddp.Addr{}) {
		// Broadcast to all routers
		dest = ddp.Addr{
			Network: 0,        // Any network
			Node:    255,      // Broadcast
			Socket:  6,        // ZIP socket
		}
	} else {
		// Ensure socket is set
		dest.Socket = 6
	}

	// Create reply channel
	replyChan := make(chan *GetNetInfoReplyPacket, 1)
	errChan := make(chan error, 1)

	// Start goroutine to listen for replies
	go func() {
		for {
			// Read from socket
			packet, from, err := c.Socket.Read()
			if err != nil {
				select {
				case errChan <- fmt.Errorf("socket read error: %w", err):
				case <-ctx.Done():
				}
				return
			}

			// Try to parse as GetNetInfo reply
			if len(packet) < 1 {
				continue
			}
			if packet[0] != FunctionGetNetInfoReply {
				continue
			}

			reply, err := UnmarshalGetNetInfoReplyPacket(packet)
			if err != nil {
				// Invalid reply, keep waiting
				continue
			}

			// If we specified a seed router, verify the reply is from it
			if c.SeedRouter != (ddp.Addr{}) {
				if from.Network != c.SeedRouter.Network || from.Node != c.SeedRouter.Node {
					// Reply from wrong router, ignore
					continue
				}
			}

			// Valid reply received
			select {
			case replyChan <- reply:
			case <-ctx.Done():
			}
			return
		}
	}()

	// Retry loop
	var lastErr error
	for attempt := 0; attempt < c.MaxRetries; attempt++ {
		// Send query
		if err := c.Socket.Write(queryBytes, dest); err != nil {
			lastErr = fmt.Errorf("failed to send GetNetInfo query: %w", err)
			time.Sleep(time.Second) // Brief delay before retry
			continue
		}

		// Wait for reply with timeout
		attemptCtx, cancel := context.WithTimeout(ctx, c.Timeout)
		select {
		case reply := <-replyChan:
			cancel()
			return reply, nil

		case err := <-errChan:
			cancel()
			lastErr = err
			return nil, err

		case <-attemptCtx.Done():
			cancel()
			lastErr = fmt.Errorf("timeout waiting for GetNetInfo reply (attempt %d/%d)", attempt+1, c.MaxRetries)
			// Continue to next retry
		}
	}

	return nil, fmt.Errorf("failed to get network info after %d attempts: %w", c.MaxRetries, lastErr)
}

// Marshal serializes the GetNetInfo query packet.
func (p *GetNetInfoPacket) Marshal() ([]byte, error) {
	if len(p.ZoneName) > 32 {
		return nil, fmt.Errorf("zone name too long [%d > 32]", len(p.ZoneName))
	}

	// Calculate packet size:
	// 1 (command) + 1 (flags) + 4 (reserved) + 1 (zone length) + len(zone name)
	size := 7 + len(p.ZoneName)
	buf := make([]byte, size)

	buf[0] = FunctionGetNetInfo // ZIP command: GetNetInfo (5)
	buf[1] = 0                   // Flags: reserved
	buf[2] = 0                   // Reserved
	buf[3] = 0                   // Reserved
	buf[4] = 0                   // Reserved
	buf[5] = 0                   // Reserved
	buf[6] = byte(len(p.ZoneName))
	if len(p.ZoneName) > 0 {
		copy(buf[7:], p.ZoneName)
	}

	return buf, nil
}

// UnmarshalGetNetInfoReplyPacket parses a ZIP GetNetInfo Reply packet.
func UnmarshalGetNetInfoReplyPacket(data []byte) (*GetNetInfoReplyPacket, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("insufficient input length %d for GetNetInfoReply packet", len(data))
	}

	if data[0] != FunctionGetNetInfoReply {
		return nil, fmt.Errorf("not a GetNetInfoReply packet (ZIP command %d != %d)", data[0], FunctionGetNetInfoReply)
	}

	reply := &GetNetInfoReplyPacket{}

	// Parse flags (byte 1)
	flags := data[1]
	reply.ZoneInvalid = (flags & 0x80) != 0
	reply.UseBroadcast = (flags & 0x40) != 0
	reply.OnlyOneZone = (flags & 0x20) != 0

	// Parse network range (bytes 2-5)
	reply.NetStart = ddp.Network(data[2])<<8 | ddp.Network(data[3])
	reply.NetEnd = ddp.Network(data[4])<<8 | ddp.Network(data[5])

	// Parse zone name
	offset := 6
	if offset >= len(data) {
		return nil, fmt.Errorf("packet too short for zone name length")
	}
	zoneLen := int(data[offset])
	offset++

	if zoneLen > 32 || zoneLen < 0 {
		return nil, fmt.Errorf("invalid zone name length: %d", zoneLen)
	}

	if offset+zoneLen > len(data) {
		return nil, fmt.Errorf("packet too short for zone name (need %d bytes, have %d)", offset+zoneLen, len(data))
	}

	reply.ZoneName = string(data[offset : offset+zoneLen])
	offset += zoneLen

	// Parse multicast address
	if offset >= len(data) {
		return nil, fmt.Errorf("packet too short for multicast address length")
	}
	multicastLen := int(data[offset])
	offset++

	if multicastLen != 6 {
		return nil, fmt.Errorf("unexpected multicast address length: %d (expected 6)", multicastLen)
	}

	if offset+6 > len(data) {
		return nil, fmt.Errorf("packet too short for multicast address")
	}

	copy(reply.MulticastAddr[:], data[offset:offset+6])
	offset += 6

	// If zone is invalid, parse default zone
	if reply.ZoneInvalid {
		if offset >= len(data) {
			return nil, fmt.Errorf("packet too short for default zone name length")
		}
		defaultZoneLen := int(data[offset])
		offset++

		if defaultZoneLen > 32 || defaultZoneLen < 0 {
			return nil, fmt.Errorf("invalid default zone name length: %d", defaultZoneLen)
		}

		if offset+defaultZoneLen > len(data) {
			return nil, fmt.Errorf("packet too short for default zone name")
		}

		reply.DefaultZoneName = string(data[offset : offset+defaultZoneLen])
	}

	return reply, nil
}
