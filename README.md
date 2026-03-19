# hysteria-relay

Multi-node traffic forwarding through [Hysteria 2](https://github.com/apernet/hysteria) tunnels.

A central **bridge** (hy2 server) coordinates multiple **nodes** (hy2 clients). Each node can advertise itself as an exit node. The bridge selects which node to route traffic through — like [Tailscale](https://tailscale.com)'s exit node model, but over hy2.

## Architecture

```
                 ┌──────────────────┐
                 │   Bridge          │
                 │   (hy2 server)    │
                 │                   │
                 │  SOCKS5 :1080     │
                 │  exit_node: "jp"  │──► routes user traffic to JP
                 └─┬───────┬───────┬┘
                   │       │       │
          ┌────────┘       │       └────────┐
     ┌────▼──────┐   ┌────▼──────┐   ┌─────▼─────┐
     │ Node "au" │   │ Node "jp" │   │ Node "us" │
     │ exit: yes │   │ exit: yes │   │ exit: yes │
     │           │   │           │   │           │
     │ 203.x.x.x│   │ 154.x.x.x│   │ ...       │
     └───────────┘   └───────────┘   └───────────┘
```

- Nodes connect outward to the bridge (NAT traversal via hy2)
- Each node registers a name and whether it's an exit
- The bridge picks which node to forward traffic through
- Nodes can also exit through the bridge's own network
- All traffic flows inside hy2 tunnels (encrypted, authenticated, Brutal CC)

## Usage

### Bridge (hy2 server side)

```go
bridge := relay.NewBridge()

hyServer, _ := hyserver.NewServer(&hyserver.Config{
    Conn:          udpConn,
    Outbound:      &myOutbound{bridge: bridge},
    ...
})
go hyServer.Serve()

// Route through a specific exit node:
conn, _ := bridge.DialTCP(ctx, "jp", "example.com:443")

// Or list available exits:
for _, n := range bridge.ExitNodes() {
    fmt.Println(n.Name)
}
```

### Node (hy2 client side)

```go
node := relay.NewNode(relay.NodeConfig{
    Name:     "jp",
    ExitNode: true,
})

hyClient, _, _ := hyclient.NewClient(&hyclient.Config{...})
go node.Attach(ctx, hyClient) // registers with bridge, handles dial requests

// Exit through bridge's network (standard hy2 passthrough):
conn, _ := node.DialTCP(ctx, "example.com:443")
```

### Outbound integration

```go
func (o *myOutbound) TCP(reqAddr string) (net.Conn, error) {
    if relay.IsRelayStream(reqAddr) {
        c1, c2 := net.Pipe()
        go o.bridge.HandleStream(ctx, reqAddr, c1)
        return c2, nil
    }
    return net.Dial("tcp", reqAddr)
}
```

## API

```go
// Bridge
func NewBridge() *Bridge
func (b *Bridge) HandleStream(ctx, reqAddr, stream)
func (b *Bridge) DialTCP(ctx, nodeName, addr) (net.Conn, error)
func (b *Bridge) Nodes() []NodeInfo
func (b *Bridge) ExitNodes() []NodeInfo
func (b *Bridge) HasNode(name) bool

// Node
func NewNode(NodeConfig) *Node
func (n *Node) Attach(ctx, client) error   // blocks, auto-reconnects on return
func (n *Node) DialTCP(ctx, addr) (net.Conn, error)  // exits via bridge
func (n *Node) HasPeer() bool

// Helpers
func IsRelayStream(addr string) bool
```

## Design

- **Hub-and-spoke**: bridge is the central relay, nodes connect outward
- **Inside hy2**: all relay traffic flows through hy2 TCP streams
- **Node.DialTCP**: direct `client.TCP` passthrough, zero overhead
- **Bridge.DialTCP**: sends request to node via control stream, node dials target
- **Auto-reconnect**: `Attach` returns on disconnect, caller retries
