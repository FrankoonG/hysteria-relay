# hysteria-relay

Reverse traffic forwarding through a [Hysteria 2](https://github.com/apernet/hysteria) tunnel.

## The Problem

A machine behind NAT needs to serve as an hy2 exit node. Hysteria 2 does not support reverse connectivity ([#1131](https://github.com/apernet/hysteria/issues/1131)).

## How It Works

```
Exit Node (NAT)                    Entry Node (Public IP)
┌──────────────────┐               ┌──────────────────┐
│  hy2 CLIENT ─────┼── hy2 tunnel──►  hy2 SERVER      │ ◄── users
│                  │  (encrypted,  │                   │
│  relay.ExitNode  │   Brutal CC)  │  relay.EntryNode  │
│  dials targets   │◄── requests ──│  .DialTCP(addr)   │
│       │          │   (inside     │                   │
│       ▼          │    tunnel)    └───────────────────┘
│   Internet       │
└──────────────────┘
```

All relay traffic flows **inside** the hy2 tunnel. hy2 handles NAT traversal, encryption, authentication, and lossy network compensation. The relay library only does reverse forwarding.

## Usage

```go
// Exit node (behind NAT):
hyClient, _, _ := hyclient.NewClient(&hyclient.Config{...})
exit := relay.NewExitNode(hyClient)
exit.Serve(ctx)

// Entry node (public IP):
entry := relay.NewEntryNode()
go entry.HandleClient(ctx, hyClient) // register connected exit node

conn, _ := entry.DialTCP(ctx, "example.com:443") // tunneled through hy2
io.Copy(dst, conn)
```

The caller is responsible for creating hy2 instances and exposing services (SOCKS5, HTTP proxy, etc.) on top.

## Design

- **hy2 is the foundation**: all traffic is inside the hy2 tunnel
- **Relay is just forwarding**: no custom transport, no auth, no crypto
- **Thin API**: `NewEntryNode`, `HandleClient`, `DialTCP`
- **No SOCKS5 built in**: the caller handles user-facing services
