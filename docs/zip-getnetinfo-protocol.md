# ZIP GetNetInfo Protocol Flow - Netatalk Reference

## Overview

ZIP GetNetInfo is used by non-seed routers to discover network configuration from a seed router. This document analyzes Netatalk's implementation to guide jrouter's soft-seed mode.

## Packet Structure

### ZIP GetNetInfo Query (ZIPOP_GNI = 5)
```
Destination: DDP socket 6
DDP Type: 6
Structure:
  - ZIP command (1 byte): 5 (ZIPOP_GNI)
  - Flags (1 byte): 0 (reserved)
  - Reserved (4 bytes): 0
  - Zone name length (1 byte)
  - Zone name (variable, max 32 bytes)
```

### ZIP GetNetInfo Reply (ZIPOP_GNIREPLY = 6)
```
Source: DDP socket 6
DDP Type: 6
Structure:
  - ZIP command (1 byte): 6 (ZIPOP_GNIREPLY)
  - Flags (1 byte):
    - 0x80: ZIPGNI_INVALID - zone name in request is invalid
    - 0x40: ZIPGNI_USEBROADCAST - data link doesn't support multicast
    - 0x20: ZIPGNI_ONEZONE - network's zone list contains only one zone
  - Network start (2 bytes): First network number in range
  - Network end (2 bytes): Last network number in range
  - Zone name length (1 byte)
  - Zone name (variable)
  - Multicast address length (1 byte): Always 6 for Ethernet
  - Multicast address (6 bytes): AppleTalk multicast MAC (09:00:07:ff:ff:ff)
  - If ZIPGNI_INVALID flag set:
    - Default zone length (1 byte)
    - Default zone name (variable)
```

## Netatalk Implementation Flow

### When Non-Seed Router Starts (`zip_getnetinfo()` - line 952)

**Location**: `/home/blake/code/netatalk/etc/atalkd/zip.c:952`

**Called from**:
- `rtmp.c:600` - After receiving RTMP packet from gateway
- `main.c:202` - During interface initialization
- `main.c:1332` - During address acquisition

**Function Flow**:

```c
int zip_getnetinfo(struct interface *iface)
{
    // 1. Log the request
    LOG(log_info, logtype_atalkd, "zip_getnetinfo for %s", iface->i_name);
    
    // 2. Find ZIP socket (DDP socket 6)
    for (ap = iface->i_ports; ap; ap = ap->ap_next) {
        if (ap->ap_packet == zip_packet) {
            break;
        }
    }
    
    // 3. Construct GetNetInfo query packet
    memset(&sat, 0, sizeof(struct sockaddr_at));
    sat.sat_family = AF_APPLETALK;
    sat.sat_addr.s_net = ATADDR_ANYNET;    // Broadcast
    sat.sat_addr.s_node = ATADDR_ANYNODE;  // Broadcast
    sat.sat_port = DDP_SOCKET_ZIP;         // Socket 6
    
    // 4. Build packet
    data = packet;
    *data++ = ZIPOP_GNI;     // Command: GetNetInfo (5)
    *data++ = 0;             // Flags
    *data++ = 0; *data++ = 0; // Reserved
    *data++ = 0; *data++ = 0; // Reserved
    
    // If zone specified, include it (for validation)
    if (iface->i_czt) {
        *data++ = iface->i_czt->zt_len;
        memcpy(data, iface->i_czt->zt_name, iface->i_czt->zt_len);
        data += iface->i_czt->zt_len;
    } else {
        *data++ = 0;  // No zone name
    }
    
    // 5. Send query (broadcast to all routers)
    if (sendto(ap->ap_fd, packet, data - packet, 0,
               (struct sockaddr *)&sat, sizeof(sat)) < 0) {
        LOG(log_error, logtype_atalkd, "zip_getnetinfo sendto: %s", 
            strerror(errno));
        return -1;
    }
    
    return 0;
}
```

### Receiving GetNetInfo Reply (`zip_packet()` - case ZIPOP_GNIREPLY)

**Location**: `/home/blake/code/netatalk/etc/atalkd/zip.c:~610` (case ZIPOP_GNIREPLY)

**Processing**:

```c
case ZIPOP_GNIREPLY:
    // 1. Parse flags
    zh.zh_flags = *data++;
    
    // 2. Extract network range
    memcpy(&net, data, sizeof(uint16_t));
    net = ntohs(net);
    data += sizeof(uint16_t);
    memcpy(&nodenet, data, sizeof(uint16_t));
    nodenet = ntohs(nodenet);
    data += sizeof(uint16_t);
    
    // 3. Extract zone name
    zlen = *data++;
    memcpy(zname, data, zlen);
    zname[zlen] = '\0';
    data += zlen;
    
    // 4. Skip multicast address (6 bytes + length byte)
    data += 1 + 6;
    
    // 5. If zone invalid, extract default zone
    if (zh.zh_flags & ZIPGNI_INVALID) {
        zlen = *data++;
        memcpy(zname, data, zlen);
        zname[zlen] = '\0';
        data += zlen;
    }
    
    // 6. Configure interface with received information
    LOG(log_info, logtype_atalkd, "zip gnireply from %u.%u (%s %d)",
        ntohs(from->sat_addr.s_net), from->sat_addr.s_node,
        iface->i_name, from->sat_port);
        
    // 7. Store network configuration
    iface->i_rt->rt_firstnet = net;
    iface->i_rt->rt_lastnet = nodenet;
    
    // 8. Store zone
    zip_addzone(iface, zname, zlen);
    
    // 9. Log completion
    LOG(log_info, logtype_atalkd, "zip_packet configured %s from %u.%u",
        iface->i_name, ntohs(from->sat_addr.s_net), 
        from->sat_addr.s_node);
```

## Key Observations

### 1. **Timing**
- GetNetInfo is sent multiple times during initialization
- Called after receiving first RTMP packet from gateway
- May be retried if no response

### 2. **Broadcast vs Unicast**
- Initial query is **broadcast** (`ATADDR_ANYNET.ATADDR_ANYNODE`)
- Any seed router on the network can respond
- Optional: Can specify seed router address for direct query

### 3. **Zone Handling**
- Query can include a zone name for validation
- If zone is invalid (not in network's zone list), reply has ZIPGNI_INVALID flag
- Reply includes default zone if requested zone is invalid
- If no zone specified in query, seed router returns default zone

### 4. **Network Configuration**
- Reply provides network range (first and last network numbers)
- For Phase 2, can be a range (e.g., 650-650 or 100-200)
- Interface configures itself within this range

### 5. **Integration with RTMP**
- GetNetInfo is triggered after seeing RTMP broadcasts
- RTMP provides gateway/router discovery
- GetNetInfo provides network configuration details

## jrouter Implementation Plan

Based on this analysis, jrouter soft-seed mode should:

### 1. **Startup Sequence**
```
1. Bring up interface (no RTMP broadcasts yet)
2. Listen for RTMP packets from seed router
3. When RTMP received, send ZIP GetNetInfo query
4. Wait for GetNetInfo reply (timeout: 30 seconds, 3 retries)
5. Parse reply and configure network/zone
6. Perform AARP to acquire unique address
7. Start AURP with learned configuration
```

### 2. **GetNetInfo Client**
- Broadcast to `0.0:6` (all nodes, socket 6)
- Include zone name from config for validation
- Handle retries (3 attempts with 10-second timeout each)
- Parse reply and extract network range + zone

### 3. **State Management**
- Store learned network configuration
- Mark port as non-seed (don't respond to GetNetInfo queries)
- Continue normal RTMP listening for routing updates

### 4. **Error Handling**
- Timeout: Retry up to 3 times
- Invalid zone: Log error, use default zone from reply
- No response: Fail startup (cannot operate without network info)

## Test Validation

From our packet captures (2026-01-12):
```
Jan 12 00:13:01 atalkd: zip_getnetinfo for enp12s0
Jan 12 00:13:01 atalkd: zip gnireply from 650.1 (enp12s0 12)
Jan 12 00:13:01 atalkd: zip_packet configured enp12s0 from 650.1
Jan 12 00:13:06 atalkd: rtmp_packet gateway 650.1 up
```

This confirms:
- Query sent at startup
- Reply received from 650.1 (jrouter)
- Interface configured successfully
- Gateway (router) detected via RTMP

## References

- Netatalk source: `/home/blake/code/netatalk/etc/atalkd/zip.c`
- ZIP header: `/home/blake/code/netatalk/include/atalk/zip.h`
- jrouter existing types: `/home/blake/code/jrouter/atalk/zip/getnetinfo.go`
- Inside AppleTalk, 2nd Edition, Chapter 9 (ZIP)
