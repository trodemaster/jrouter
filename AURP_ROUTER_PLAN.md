# jrouter as the AURP Router for Netatalk

**Date**: 2026-02-07
**Decision Source**: netatalk repo, beads issue netatalk-e3p.5
**Related**: `netatalk/AURP_DAEMON_FEASIBILITY.md` (protocol-native approach analysis)

## Decision

**jrouter is the AURP routing solution for the netatalk community.** It will not be integrated into netatalk's build system. It ships as a separate, standalone project and companion binary.

Rationale:
- jrouter is already in use by the community
- Adding a Go build process to the netatalk C project is undesirable to the maintainer on simplicity grounds
- jrouter already implements the complete protocol-native architecture and is tested on real hardware
- A C reimplementation would duplicate ~10,000 lines of proven, working Go code

## Current State

### Main Branch
- Stable, production-quality AURP router
- Seed mode only (acts as authoritative router for local network)
- 10,253 lines Go total
- Full packaging: deb (nfpm), systemd service, Docker containers, Prometheus metrics
- Apache 2.0 license (Josh Deprez)
- Upstream: gitea.drjosh.dev/josh/jrouter
- Fork: github.com/trodemaster/jrouter

### feature/soft-seed-mode Branch
- **Fully implemented and tested** (2026-01-12)
- +1,302/-38 lines across 9 files (5 commits)
- Adds soft-seed and non-seed router modes for coexistence with atalkd
- Tested on MacPro 2013: atalkd (seed, 650.37) + jrouter (soft-seed, 650.1)
- AURP peering verified with 177 peers
- **Not yet merged to main**

## Architecture: Protocol-Native Coexistence

jrouter operates as an **independent AppleTalk router node** on the same EtherTalk segment as atalkd. Communication between the two happens entirely via standard AppleTalk protocols — no custom IPC:

```
┌────────────────────────────────┐     ┌────────────────────────────────┐
│          atalkd (seed)          │     │      jrouter (soft-seed)       │
│                                │     │                                │
│  AF_APPLETALK sockets          │     │  select()/poll() loop          │
│  ├─ RTMP broadcast (10s)       │     │  ├─ UDP:387 (AURP peers)       │
│  └─ ZIP/NBP handlers           │     │  ├─ pcap (EtherTalk I/O)       │
│                                │     │  ├─ AARP machine               │
│  Owns:                         │     │  ├─ RTMP broadcast (10s)       │
│  - Local routing table         │     │  ├─ ZIP/NBP responders         │
│  - Local zone table            │     │  └─ timer (peer mgmt)          │
│  - AF_APPLETALK interfaces     │     │                                │
│  - Local EtherTalk services    │     │  Owns:                         │
│                                │     │  - AURP peer state machines    │
│  Sees jrouter as: just another │     │  - AURP protocol (UDP tunnels) │
│  router on the EtherTalk seg   │     │  - Remote routing table        │
│                                │     │  - Remote zone table           │
│  No AURP code. No IPC.         │     │  - Own AppleTalk node address  │
│  No awareness of jrouter.      │     │  - pcap EtherTalk I/O          │
└────────────────────────────────┘     └────────────────────────────────┘
         ▲                                        ▲
         │         Local EtherTalk Segment         │
         └─────────── RTMP / ZIP / NBP ───────────┘
                   (standard protocols)
```

### How Each Protocol Is Used

| Protocol | Direction | Purpose |
|----------|-----------|---------|
| **RTMP** | jrouter → atalkd | Broadcast AURP-learned remote routes every 10s as RTMP Data tuples. atalkd learns them naturally. |
| **RTMP** | atalkd → jrouter | jrouter listens for atalkd's RTMP broadcasts to learn local topology (for RI-Rsp to AURP peers). |
| **ZIP** | atalkd → jrouter | atalkd queries jrouter for zone info on AURP-learned networks. jrouter responds with zones learned from AURP peers. |
| **ZIP** | jrouter → atalkd | jrouter queries atalkd for local zone info to relay to AURP peers. |
| **ZIP GetNetInfo** | jrouter → atalkd | At startup (soft-seed mode), jrouter queries atalkd for network config. |
| **NBP FwdReq** | atalkd → jrouter | atalkd forwards NBP lookups for remote zones to jrouter's node address. jrouter tunnels via AURP. |
| **NBP** | jrouter → atalkd | jrouter delivers NBP replies from AURP tunnels onto local EtherTalk. |
| **DDP** | both directions | jrouter forwards DDP packets between local EtherTalk (pcap) and AURP tunnels (UDP). |
| **AARP** | jrouter ↔ network | jrouter acquires its own AppleTalk node address via AARP probing. |

### Packet Discrimination (Shared Interface)

When jrouter and atalkd share the same network interface, jrouter's pcap sees all AppleTalk traffic. Discrimination happens at the protocol handler level:

| Packet Type | jrouter Action | Rationale |
|-------------|---------------|-----------|
| AARP for jrouter's address | Respond | Standard AARP |
| AARP for other addresses | Ignore / learn AMT | Not for jrouter |
| RTMP Data from atalkd | Process | Learn local topology |
| RTMP Data from jrouter itself | Ignore | Source MAC filter |
| ZIP GetNetInfo Request | **Do NOT respond** (soft-seed) | atalkd is the seed router |
| ZIP Query for AURP-learned network | Respond | jrouter is authoritative for remote networks |
| ZIP Query for local network | Do NOT respond | atalkd handles local zones |
| NBP FwdReq to jrouter's node | Process | Forward to AURP peer |
| NBP for local zones | Ignore | atalkd handles it |
| DDP to AURP-learned network | Forward via AURP | jrouter advertises these routes |
| DDP to local network | Ignore | atalkd handles local delivery |

**Key insight**: Discrimination is destination-network-based. jrouter only handles traffic for networks it advertises in RTMP (AURP-learned remote networks). The AppleTalk routing model naturally directs packets to the right router.

## Raw Socket / Pcap Approach

jrouter uses **libpcap (via gopacket)** for all EtherTalk I/O:

```go
// BPF filter — only AppleTalk/AARP frames for this node or multicast
handle, _ := pcap.OpenLive(device, 4096, true, 100*time.Millisecond)
bpfFilter := fmt.Sprintf("(atalk or aarp) and (ether multicast or ether dst %s)", myHWAddr)
handle.SetBPFFilter(bpfFilter)
```

- Promiscuous mode to see all AppleTalk traffic
- BPF filter for kernel-level efficiency
- 64-byte minimum frame padding on transmit
- **No kernel AppleTalk module required** — fully userspace
- This means jrouter could potentially run on **modern macOS** (10.15+) where the kernel AppleTalk stack was removed

## Soft-Seed Mode Details

### Implementation (router/soft_seed.go)

Startup sequence in soft-seed mode:
1. Send ZIP GetNetInfo broadcast query with configured zone name
2. Wait up to 10s for reply, retry up to 3 times (3s between retries)
3. Parse reply: learn network range, zone name, default zone, multicast address
4. Update port config with learned values
5. Add routes to routing table
6. Start AARP to acquire node address
7. Begin RTMP/ZIP/NBP/DDP operation

Fallback (soft-seed only): If no seed router responds, use configured values and act as seed.
Non-seed mode: Fails if no seed router responds.

### Configuration

```yaml
ethertalk:
  - device: eth0
    router_mode: soft-seed    # seed | soft-seed | non-seed
    zone_name: netjibbing     # For validation and fallback
    net_start: 650            # Required for fallback
    net_end: 650
    # seed_router: "650.37"   # Optional: specific router to query
```

### ZIP GetNetInfo Protocol Flow

```
jrouter (soft-seed)                    atalkd (seed)
    |                                      |
    |--- ZIP GetNetInfo (broadcast) ------>|
    |    DstNet=0, DstNode=0xFF, Socket=6  |
    |    Zone="netjibbing"                 |
    |                                      |
    |<-- ZIP GetNetInfo Reply -------------|
    |    NetStart=650, NetEnd=650          |
    |    Zone="netjibbing"                 |
    |    Multicast=09:00:07:XX:XX:XX       |
    |                                      |
    |--- AARP Probes (10x @ 200ms) ------>|
    |    (acquire node address)            |
    |                                      |
    |--- RTMP Data (every 10s) ---------->|
    |    (advertise AURP-learned routes)   |
```

### Verified Test Results (2026-01-12, MacPro 2013)

- atalkd seed at 650.37, jrouter soft-seed at 650.1
- Netatalk AFP shares visible via AppleTalk (650.37:128)
- jrouter registered as AppleRouter (650.1:253)
- AURP peers connected, 27+ zones from remote networks visible
- No conflicts, no errors in either atalkd or jrouter logs

## Known Gaps / Improvements Needed

### From Soft-Seed Review

1. **Single zone learning**: `InitializeSoftSeed()` only learns one zone per network. Multi-zone networks would need additional ZIP queries after startup.
2. **No periodic re-query**: Network config is learned once at startup. If atalkd changes zones, jrouter won't notice until restart.
3. **Binary committed to repo**: `jrouter-softseed` (20MB ELF binary) is tracked in git on the feature branch. Should be in `.gitignore`.
4. **Branch not merged**: soft-seed-mode changes need to be merged to main.

### General jrouter Improvements

5. **Packet splitting**: README notes "Some packet types aren't currently split correctly to fit within limits. This mainly affects routers that advertise lots of routes or zones."
6. **AURP 99.5% complete**: README says implementation is about 99.5% complete — identify the remaining 0.5%.
7. **Test coverage**: 6 test files exist but coverage could be expanded, especially for soft-seed integration scenarios.

### Operational

8. **Systemd dependency on atalkd**: In soft-seed mode, jrouter should start after atalkd. The systemd service only has `After=network.target` — may need `After=atalkd.service`.
9. **Documentation for netatalk users**: Need a clear guide for running jrouter alongside netatalk with soft-seed mode.

## Impact on Netatalk

### What Happens in Netatalk (setupaurp branch)

The AURP code currently integrated into atalkd will be **removed** (pure deletion, ~124 lines of integration + ~5,048 lines of AURP code):

| File | Action |
|------|--------|
| `etc/atalkd/aurp.c` (2,848 lines) | Delete |
| `etc/atalkd/aurp_peer.c` (1,681 lines) | Delete |
| `etc/atalkd/aurp_config.c` (203 lines) | Delete |
| `etc/atalkd/aurp.h` (320 lines) | Delete |
| `etc/atalkd/main.c` (~100 lines) | Remove AURP init, select loop, timer, raw socket functions |
| `etc/atalkd/config.c` (~10 lines) | Remove `aurp-*` config delegation |
| `etc/atalkd/nbp.c` (~8 lines) | Remove AURP tracking hooks |
| `etc/atalkd/rtmp.h` (1 line) | Remove `RTMPTAB_AURP` flag |
| `etc/atalkd/nbp.h` (2 lines) | Remove aurp_peer forward declarations |
| `etc/atalkd/meson.build` (3 lines) | Remove AURP source files |

**Net change to atalkd: -5,172 lines, +0 lines.** atalkd becomes a clean, AURP-unaware AppleTalk router.

The netatalk upstream PR would include:
1. AURP removal from atalkd (pure deletion)
2. Standalone netatalk fixes identified in netatalk-e3p.2
3. Documentation pointing to jrouter as the companion AURP router

### What Stays in Netatalk

- The `setupaurp` branch preserves the full history of AURP development in atalkd
- `AURP_IMPLEMENTATION_PLAN.md` and `AURP_PACKET_DETAILS.md` remain as reference
- `AURP_DAEMON_FEASIBILITY.md` documents the architectural decision

## Key Source Files Reference

### jrouter — Protocol Implementation

| File | Lines | Purpose |
|------|-------|---------|
| `router/etalk_port.go` | 614 | Pcap I/O, frame marshal/unmarshal, protocol dispatch, outbox queueing |
| `router/aarp.go` | 410 | AARP state machine, AMT, address probing, conflict detection |
| `router/rtmp.go` | 291 | RTMP Data broadcast (10s), route learning, split-horizon |
| `router/zip.go` | 329 | ZIP Query/Reply, GetNetInfo, ATP zone lists |
| `router/nbp.go` | 263 | NBP BrRq/FwdReq/LkUp handling, zone multicast |
| `router/router.go` | — | DDP routing, hop counting, forwarding decisions |
| `router/route_table.go` | 444 | Network→target mapping, route upsert/lookup |
| `router/soft_seed.go` | 348 | Soft-seed/non-seed mode, ZIP GetNetInfo client |
| `router/config.go` | — | YAML config with router_mode, seed_router fields |

### jrouter — AURP Implementation

| File | Lines | Purpose |
|------|-------|---------|
| `aurp/transport.go` | 421 | AURP UDP transport layer |
| `aurp/aurp.go` | 380 | AURP packet types, marshaling |
| `aurp/routing_info.go` | 306 | RI-Req/RI-Rsp/RI-Upd/RI-Ack |
| `aurp/zone_info.go` | 459 | ZI-Req/ZI-Rsp |
| `aurp/open.go` | 198 | Open-Req/Open-Rsp connection setup |
| `aurp/domain.go` | 182 | Domain header parsing |
| `router/aurp_peer.go` | 1,365 | Peer state machine, route events, timers |
| `router/aurp_peer_table.go` | 313 | Peer connection management |
| `router/aurp.go` | 175 | AURP UDP listener, DDP encapsulation ingress |

### Netatalk — AURP Code (to be removed)

| File | Lines | Purpose |
|------|-------|---------|
| `etc/atalkd/aurp.c` | 2,848 | AURP core: UDP, packet codec, forwarding, raw sockets |
| `etc/atalkd/aurp_peer.c` | 1,681 | Peer state machines, route management, timers |
| `etc/atalkd/aurp_config.c` | 203 | Config parsing for aurp-* directives |
| `etc/atalkd/aurp.h` | 320 | Protocol constants, data structures |
