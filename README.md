# jrouter

Home-grown alternative implementation of Apple Internet Router 3.0

## Goals

- Full compatibility with Apple Internet Router 3.0
- Function on modern operating systems
- EtherTalk support
- Be observable (there's a HTTP server with `/status` and `/metrics` pages)

### Stretch goals

- Direct TashTalk support

## Things that used to be caveats

- Previously it would listen for all EtherTalk traffic, regardless of
  destination. Now it doesn't do that, which should help it co-exist with other
  routers on the same host.
- You can configure an alternate Ethernet address if you are reusing the same
  network interface for multiple different EtherTalk software.
- In addition to the configured EtherTalk network and zone, it now learns
  routes and zones from other EtherTalk routers, and should share them across
  AURP.
- **NEW:** Soft-seed and non-seed router modes are now supported, enabling
  coexistence with Netatalk's atalkd on the same network (see below).
- There's a status endpoint that outputs diagnostic information about the
  state of the server. Set the `monitoring_addr` config option and then browse
  to `http://[your router]:[port you configured]/status` to see information
  about the state of jrouter.

## Caveats & known bugs

- Some packet types aren't currently split correctly to fit within limits. This
  mainly affects routers try to that advertise lots of routes or zones.
- The AURP implementation is about 99.5% complete.

The issues in this repo should be updated as things get fixed.

## Router Modes

jrouter supports three router modes for EtherTalk ports:

### Seed Mode (default)

In seed mode, jrouter is authoritative for the network configuration. It defines
the network number range and zone names, and responds to GetNetInfo queries from
other nodes.

```yaml
ethertalk:
  - device: eth0
    router_mode: seed       # Optional, this is the default
    zone_name: MyZone
    net_start: 100
    net_end: 100
```

### Soft-Seed Mode

In soft-seed mode, jrouter first queries for an existing seed router on the
network. If found, it learns the network configuration from the seed router. If
no seed router responds, it falls back to seed mode using its configured values.

This mode is useful for coexisting with Netatalk's atalkd when atalkd is
configured as the seed router:

```yaml
ethertalk:
  - device: eth0
    router_mode: soft-seed
    zone_name: MyZone       # Used for validation and fallback
    net_start: 100          # Required for fallback
    net_end: 100
    # seed_router: 100.37   # Optional: specific seed router to query
```

### Non-Seed Mode

In non-seed mode, jrouter must learn its configuration from a seed router. If no
seed router responds, startup fails. This is the safest mode when another router
(like atalkd) is definitely the seed.

```yaml
ethertalk:
  - device: eth0
    router_mode: non-seed
    zone_name: MyZone       # Used for validation
    # net_start/net_end not needed - learned from seed
```

### Coexisting with Netatalk

To run jrouter alongside Netatalk's atalkd on the same network:

1. Configure atalkd as the seed router:
   ```
   # /etc/atalkd.conf
   eth1 -router -phase 2 -net 650 -addr 650.37 -zone "MyZone"
   ```

2. Configure jrouter in soft-seed or non-seed mode:
   ```yaml
   # jrouter.yaml
   ethertalk:
     - device: eth0
       router_mode: soft-seed
       zone_name: MyZone
       net_start: 650
       net_end: 650
       ethernet_addr: '08:00:07:FE:DC:BA'  # Use different MAC to avoid conflicts
   ```

3. Start atalkd first, then jrouter

The key points are:
- Use separate network interfaces if possible (eth0 for jrouter, eth1 for atalkd)
- If on the same interface, use `ethernet_addr` to give jrouter a different MAC
- Let atalkd be the seed router, jrouter will learn from it
- jrouter will still provide AURP tunneling to remote networks

## How to use

WARNING: It Sorta Works™. See "Caveats & known bugs" above.

First, write a `jrouter.yaml` config file.
Use [the jrouter.yaml in this repo](/josh/jrouter/src/branch/main/jrouter.yaml)
as both an example and for documentation of config options.

Then choose from the options below:

### Installing on Debian / Raspbian directly

There's not an APT repository yet, but you can always directly install .debs:

1. Download a `jrouter_(VERSION)_linux_arm64.deb` from the Releases page
2. `sudo dpkg -i jrouter_..._arm64.deb`
3. Put `jrouter.yaml` into `/etc/jrouter/`

Then (assuming you are using systemd, which you probably are):

4. `sudo systemctl enable --now jrouter.service`
5. To see logs, use `journalctl -f -u jrouter.service`

### Running with Docker

Multiarch (x86_64 and arm64) container images are available from this server.

- `gitea.drjosh.dev/josh/jrouter:latest` - latest release version
- `gitea.drjosh.dev/josh/jrouter:0.0.12` - specific patch version
- `gitea.drjosh.dev/josh/jrouter:0.0` - latest patch release for minor version
- `gitea.drjosh.dev/josh/jrouter:0` - latest minor & patch release for major version
- `gitea.drjosh.dev/josh/jrouter:dev` - pre-release that I'm currently testing

Example `docker run` command:

```shell
# Run using a config file ./cfg/jrouter.yaml
docker run \
  -v ./cfg:/etc/jrouter \
  --cap-add NET_RAW \
  --net host \
  --name jrouter \
  gitea.drjosh.dev/josh/jrouter:latest
```

Notes:

- Put `jrouter.yaml` inside a `cfg` directory (or some path of your choice and bind-mount it at `/etc/jrouter`) for it to find the config file.
- `--cap-add NET_RAW` and `--net host` is needed for EtherTalk access to the network interface.
- By using `--net host`, the default AURP port (387) will be bound without `-p`.

### Docker Compose

Example `docker-compose.yml` file:

```yaml
services:
  jrouter:
    image: gitea.drjosh.dev/josh/jrouter:latest
    restart: unless-stopped
    volumes:
      - type: bind
        source: ./jrouter
        target: /etc/jrouter
    network_mode: host
    cap_add:
      - NET_RAW
```

### Building and running manually

These instructions ignore `mage` or containerised builds, and build the binary
directly.

1. Install [Go](https://go.dev/dl).
2. Run these commands (for Debian-variety Linuxen, e.g. Ubuntu, Raspbian, Mint...):

```shell
sudo apt install git build-essential libpcap-dev
go install drjosh.dev/jrouter@latest   # or substitute @latest with @(version) e.g. @v0.0.12
sudo setcap 'CAP_NET_BIND_SERVICE=ep CAP_NET_RAW=ep' ~/go/bin/jrouter
```

3. Configure `jrouter.yaml`
4. To run:

```shell
~/go/bin/jrouter
```

Notes:

- `git` is needed for `go install` to fetch the module
- `build-essential` and `libpcap-dev` are needed for [gopacket](https://github.com/google/gopacket), which uses [CGo](https://pkg.go.dev/cmd/cgo)
- `NET_BIND_SERVICE` is needed for `jrouter` to bind UDP port 387 (for talking between AIRs)
- `NET_RAW` is needed for `jrouter` to listen for and send EtherTalk packets
- By default `jrouter` looks for `jrouter` in the current directory. It can be
  changed with the `config` flag:

  ```shell
  jrouter -config /etc/jrouter/jrouter.yaml
  ```

TODO: instructions for non-Linux / non-Debian-like machines

### Building and running with Docker manually

These instructions ignore `mage`, and use `Dockerfile` which builds the binary
specifically for the container.

1.  Install Docker.
2.  Clone the repo and `cd` into it.
3.  `docker build -t jrouter .`

Example `docker run` command:

    ```shell
    docker run \
      -v ./cfg:/etc/jrouter \
      --cap-add NET_RAW \
      --net host \
      --name jrouter \
      jrouter
    ```

Notes:

- Put `jrouter.yaml` inside a `cfg` directory (or some path of your choice and bind-mount it at `/etc/jrouter`) for it to find the config file.
- Both `--cap-add NET_RAW` and `--net host` is needed for EtherTalk access to the network interface.
- By using `--net host`, the default AURP port (387) will be bound without `-p`.

### Building with Mage

For managing building everything at once, I use [mage](https://magefile.org/).
You are welcome to do likewise. Depending on what you want to build, you will
need Go and/or Docker installed. Output files are stored in `./dist` and
container image builds then assume they can use pre-built binaries from there.

`mage binary` runs locally and needs the build-time dependencies like
libpcap-dev. Most other mage targets run commands within containers.

## Bibliography / Acknowledgements

This software wouldn't be possible without:

- [Sidhu, G S, Andrews, R F, & Oppenheimer, A B (1990), _Inside AppleTalk_, 2nd edn, Addison-Wesley, Reading, Mass.](https://vintageapple.org/macbooks/pdf/Inside_AppleTalk_Second_Edition_1990.pdf)
- [Apple Computer Inc. (1993), _AppleTalk Update-Based Routing Protocol: Enhanced AppleTalk Routing_, Cupertino, Calif.](/josh/jrouter/src/branch/main/docs/AURP_Enhanced_ATalk_Routing.pdf)
- [Apple Internet Router 3.0](https://macintoshgarden.org/apps/apple-internet-router) itself
- [sfiera/multitalk](https://github.com/sfiera/multitalk)
- [tcpdump and libpcap](https://www.tcpdump.org/)
- [google/gopacket](https://github.com/google/gopacket)
- Encouragement from #GlobalTalk and #MARCHintosh

## Non-acknowledgements

Aside from standard reformatting and analysis tools (gofmt, gopls) this software
is 100% organically written.

I do not use LLMs or generative "AI" in my work. I _will not_ use LLMs or
generative "AI" in my work. I write enough bugs on my own, I don't need a
stochastic parrot to hallucinate more for me.

## Bug reports? Feature requests? Complaints? Praise?

You can contact me on the Fediverse at @DrJosh9000@cloudisland.nz, or email me at josh.deprez@gmail.com.
