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
│  A.DialTCP(addr) ────┼──request──►│  dials addr, relays   │
│  dials addr, relays ◄┼──request───┤  B.DialTCP(addr)      │
└──────────────────────┘            └───────────────────────┘
```

- Node A runs hy2 client, calls `node.Attach(ctx, hyClient)`
- Node B runs hy2 server, routes relay streams via `node.HandleStream()` in its `Outbound.TCP`
- Either node calls `node.DialTCP(ctx, addr)` to open a TCP connection that exits through the peer's network
- All traffic flows inside the hy2 tunnel — hy2 handles NAT traversal, encryption, authentication, and Brutal CC

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

// Traffic exits through Node B's network:
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

// Traffic exits through Node A's network:
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
// NewNode creates a symmetric relay endpoint.
func NewNode() *Node

// Attach connects to a peer via an hy2 client. Call on the hy2 client side.
// Opens control streams inside the tunnel. Blocks until ctx is cancelled.
func (n *Node) Attach(ctx context.Context, client hyclient.Client) error

// HandleStream routes incoming relay streams. Call from hy2 server Outbound.TCP
// when IsRelayStream(reqAddr) returns true.
func (n *Node) HandleStream(ctx context.Context, reqAddr string, stream net.Conn)

// DialTCP opens a TCP connection to addr through the peer's network.
// Can be called from either side.
func (n *Node) DialTCP(ctx context.Context, addr string) (net.Conn, error)

// HasPeer returns true if a peer is connected.
func (n *Node) HasPeer() bool

// IsRelayStream returns true if the address is a relay internal stream.
func IsRelayStream(addr string) bool
```

## Design

- **Symmetric**: single `Node` type, no entry/exit distinction
- **Inside hy2**: all relay traffic flows through hy2 TCP streams
- **No custom transport**: no extra ports, no separate auth, no framing
- **hy2 handles everything**: NAT traversal, encryption, authentication, Brutal CC
