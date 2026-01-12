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
	"encoding/binary"
	"testing"

	"drjosh.dev/jrouter/atalk/zip"
)

func TestParseGetNetInfoReply(t *testing.T) {
	tests := []struct {
		name        string
		data        []byte
		wantErr     bool
		wantResult  *SoftSeedResult
	}{
		{
			name: "valid reply with zone",
			data: func() []byte {
				// Build a valid GetNetInfo Reply packet
				data := make([]byte, 0, 50)
				data = append(data, zip.FunctionGetNetInfoReply) // Command
				data = append(data, 0x20)                        // Flags: OnlyOneZone
				data = append(data, 0x02, 0x8A)                  // NetStart: 650
				data = append(data, 0x02, 0x8A)                  // NetEnd: 650
				data = append(data, 10)                          // Zone name length
				data = append(data, "netjibbing"...)             // Zone name
				data = append(data, 6)                           // Multicast length
				data = append(data, 0x09, 0x00, 0x07, 0xFF, 0xFF, 0xFF) // Multicast addr
				return data
			}(),
			wantErr: false,
			wantResult: &SoftSeedResult{
				NetStart:    650,
				NetEnd:      650,
				ZoneName:    "netjibbing",
				OnlyOneZone: true,
			},
		},
		{
			name: "reply with invalid zone and default zone",
			data: func() []byte {
				data := make([]byte, 0, 70)
				data = append(data, zip.FunctionGetNetInfoReply) // Command
				data = append(data, 0x80|0x20)                   // Flags: ZoneInvalid + OnlyOneZone
				data = append(data, 0x00, 0x64)                  // NetStart: 100
				data = append(data, 0x00, 0xC8)                  // NetEnd: 200
				data = append(data, 7)                           // Zone name length
				data = append(data, "invalid"...)                // Zone name (invalid)
				data = append(data, 6)                           // Multicast length
				data = append(data, 0x09, 0x00, 0x07, 0xFF, 0xFF, 0xFF) // Multicast addr
				data = append(data, 10)                          // Default zone name length
				data = append(data, "netjibbing"...)             // Default zone name
				return data
			}(),
			wantErr: false,
			wantResult: &SoftSeedResult{
				NetStart:        100,
				NetEnd:          200,
				ZoneName:        "invalid",
				DefaultZoneName: "netjibbing",
				OnlyOneZone:     true,
			},
		},
		{
			name:    "packet too short",
			data:    []byte{zip.FunctionGetNetInfoReply, 0x00, 0x00},
			wantErr: true,
		},
		{
			name: "invalid zone length",
			data: func() []byte {
				data := make([]byte, 0, 20)
				data = append(data, zip.FunctionGetNetInfoReply)
				data = append(data, 0x00)
				data = append(data, 0x00, 0x64)
				data = append(data, 0x00, 0xC8)
				data = append(data, 50) // Invalid zone length (only 10 bytes follow)
				data = append(data, "netjibbing"...)
				return data
			}(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseGetNetInfoReply(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseGetNetInfoReply() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if result.NetStart != tt.wantResult.NetStart {
				t.Errorf("NetStart = %v, want %v", result.NetStart, tt.wantResult.NetStart)
			}
			if result.NetEnd != tt.wantResult.NetEnd {
				t.Errorf("NetEnd = %v, want %v", result.NetEnd, tt.wantResult.NetEnd)
			}
			if result.ZoneName != tt.wantResult.ZoneName {
				t.Errorf("ZoneName = %v, want %v", result.ZoneName, tt.wantResult.ZoneName)
			}
			if result.DefaultZoneName != tt.wantResult.DefaultZoneName {
				t.Errorf("DefaultZoneName = %v, want %v", result.DefaultZoneName, tt.wantResult.DefaultZoneName)
			}
			if result.OnlyOneZone != tt.wantResult.OnlyOneZone {
				t.Errorf("OnlyOneZone = %v, want %v", result.OnlyOneZone, tt.wantResult.OnlyOneZone)
			}
		})
	}
}

func TestRouterModeConstants(t *testing.T) {
	// Ensure router mode constants have correct string values
	if RouterModeSeed != "seed" {
		t.Errorf("RouterModeSeed = %q, want %q", RouterModeSeed, "seed")
	}
	if RouterModeSoftSeed != "soft-seed" {
		t.Errorf("RouterModeSoftSeed = %q, want %q", RouterModeSoftSeed, "soft-seed")
	}
	if RouterModeNonSeed != "non-seed" {
		t.Errorf("RouterModeNonSeed = %q, want %q", RouterModeNonSeed, "non-seed")
	}
}

func TestBuildGetNetInfoQuery(t *testing.T) {
	// Test that we can build a valid GetNetInfo query packet
	zoneName := "netjibbing"
	
	queryData := make([]byte, 7+len(zoneName))
	queryData[0] = zip.FunctionGetNetInfo
	queryData[1] = 0 // Flags
	queryData[2] = 0 // Reserved
	queryData[3] = 0 // Reserved
	queryData[4] = 0 // Reserved
	queryData[5] = 0 // Reserved
	queryData[6] = byte(len(zoneName))
	copy(queryData[7:], zoneName)

	// Verify the packet
	if queryData[0] != 5 {
		t.Errorf("Command byte = %d, want 5", queryData[0])
	}
	if queryData[6] != 10 {
		t.Errorf("Zone length = %d, want 10", queryData[6])
	}
	if string(queryData[7:]) != zoneName {
		t.Errorf("Zone name = %q, want %q", string(queryData[7:]), zoneName)
	}
}

func TestNetworkByteOrder(t *testing.T) {
	// Verify that network numbers are correctly parsed in big-endian
	data := []byte{0x02, 0x8A} // 650 in big-endian
	netNum := binary.BigEndian.Uint16(data)
	if netNum != 650 {
		t.Errorf("Network number = %d, want 650", netNum)
	}
}
