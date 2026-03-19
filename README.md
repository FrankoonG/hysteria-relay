# hysteria-relay

Bidirectional traffic forwarding through a [Hysteria 2](https://github.com/apernet/hysteria) tunnel.

Once two nodes establish an hy2 connection, **either side can route traffic through the other's network**. There is no fixed entry/exit role — both nodes are symmetric.

## How It Works

```
Node A (e.g. behind NAT)            Node B (e.g. public IP)
┌──────────────────────┐            ┌──────────────────────┐
│  hy2 CLIENT          │──tunnel──► │  hy2 SERVER          │
│  relay.Node          │            │  relay.Node           │
│                      │            │                       │
│  A.DialTCP(addr) ────┼──hy2 TCP──►│  Outbound dials addr  │
│  Outbound dials addr◄┼──control───┤  B.DialTCP(addr)      │
└──────────────────────┘            └───────────────────────┘
```

- **A → B direction** (client side calls `DialTCP`): direct passthrough to `client.TCP(addr)` — standard hy2 behavior, zero relay overhead. The hy2 server's `Outbound.TCP` dials the target.
- **B → A direction** (server side calls `DialTCP`): uses a relay control stream inside the hy2 tunnel, because hy2 server cannot initiate streams to the client. The client dials the target and opens a data stream back.

All traffic flows inside the hy2 tunnel — hy2 handles NAT traversal, encryption, authentication, and Brutal CC.

## Usage

### Node A (hy2 client side)

```go
node := relay.NewNode()

hyClient, _, _ := hyclient.NewClient(&hyclient.Config{
    ServerAddr: serverAddr,
    Auth:       "password",
    TLSConfig:  ...,
})

// Attach opens control streams inside the hy2 tunnel. Blocks.
go node.Attach(ctx, hyClient)

// Traffic exits through Node B's network (direct client.TCP passthrough):
conn, _ := node.DialTCP(ctx, "example.com:443")
```

### Node B (hy2 server side)

```go
node := relay.NewNode()

hyServer, _ := hyserver.NewServer(&hyserver.Config{
    Conn:          udpConn,
    TLSConfig:     ...,
    Authenticator: ...,
    Outbound:      &myOutbound{node: node},
})
go hyServer.Serve()

// Traffic exits through Node A's network (via relay control stream):
conn, _ := node.DialTCP(ctx, "10.0.0.1:22")
```

### Outbound integration

The hy2 server's `Outbound.TCP` must route relay streams to the node:

```go
type myOutbound struct { node *relay.Node }

func (o *myOutbound) TCP(reqAddr string) (net.Conn, error) {
    if relay.IsRelayStream(reqAddr) {
        c1, c2 := net.Pipe()
        go o.node.HandleStream(ctx, reqAddr, c1)
        return c2, nil
    }
    return net.Dial("tcp", reqAddr)  // normal outbound
}
```

## API

```go
func NewNode() *Node
func (n *Node) Attach(ctx context.Context, client hyclient.Client) error
func (n *Node) HandleStream(ctx context.Context, reqAddr string, stream net.Conn)
func (n *Node) DialTCP(ctx context.Context, addr string) (net.Conn, error)
func (n *Node) HasPeer() bool
func IsRelayStream(addr string) bool
```

## Design

- **Symmetric**: single `Node` type, no entry/exit distinction
- **Inside hy2**: all relay traffic flows through hy2 TCP streams
- **Zero overhead on client side**: `DialTCP` is a direct `client.TCP` passthrough
- **No custom transport**: no extra ports, no separate auth, no framing
