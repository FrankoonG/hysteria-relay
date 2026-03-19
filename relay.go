// Package relay provides multi-node traffic forwarding through Hysteria 2 tunnels.
//
// Architecture (Tailscale-like):
//
//	Bridge (hy2 server, public IP) — coordination + relay hub
//	Nodes  (hy2 clients, behind NAT) — connect to bridge, may advertise exit capability
//
//	Any node can route traffic through any other node's network.
//	The bridge facilitates routing and can also serve as an exit itself.
package relay

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
)

const (
	streamRegister   = "_relay_register_:0"
	streamCtrlS2C    = "_relay_s2c_ctrl_:0" // bridge → node dial requests
	streamDataPrefix = "_relay_data_"
	streamDataSuffix = ":0"
)

// IsRelayStream returns true if addr is a relay internal stream.
func IsRelayStream(addr string) bool {
	return addr == streamRegister ||
		addr == streamCtrlS2C ||
		strings.HasPrefix(addr, streamDataPrefix)
}

// --- NodeInfo ---

// NodeInfo describes a connected node.
type NodeInfo struct {
	Name     string
	ExitNode bool
}

// --- Bridge (hy2 server side) ---

// Bridge is the central hub. It tracks connected nodes and routes traffic.
type Bridge struct {
	mu    sync.RWMutex
	nodes map[string]*bridgeNode // name → node
	seq   atomic.Uint64
}

type bridgeNode struct {
	info    NodeInfo
	ctrlW   net.Conn // write dial requests to this node
	writeMu sync.Mutex
	waiting map[string]chan net.Conn
}

// NewBridge creates a bridge.
func NewBridge() *Bridge {
	return &Bridge{nodes: make(map[string]*bridgeNode)}
}

// HandleStream routes relay streams from Outbound.TCP.
func (b *Bridge) HandleStream(ctx context.Context, reqAddr string, stream net.Conn) {
	switch reqAddr {
	case streamRegister:
		b.handleRegister(ctx, stream)

	case streamCtrlS2C:
		// Node opened s2c ctrl — bridge writes dial requests here
		// Read the node name header first
		name, err := readString(stream)
		if err != nil {
			stream.Close()
			return
		}
		b.mu.Lock()
		if n, ok := b.nodes[name]; ok {
			n.ctrlW = stream
		}
		b.mu.Unlock()
		<-ctx.Done()

	default:
		if strings.HasPrefix(reqAddr, streamDataPrefix) {
			id := strings.TrimSuffix(reqAddr[len(streamDataPrefix):], streamDataSuffix)
			// Find which node this data stream belongs to
			b.mu.RLock()
			for _, n := range b.nodes {
				n.writeMu.Lock()
				ch, ok := n.waiting[id]
				if ok {
					delete(n.waiting, id)
					n.writeMu.Unlock()
					b.mu.RUnlock()
					ch <- stream
					return
				}
				n.writeMu.Unlock()
			}
			b.mu.RUnlock()

			// Retry with delay (race condition)
			for i := 0; i < 50; i++ {
				time.Sleep(10 * time.Millisecond)
				b.mu.RLock()
				for _, n := range b.nodes {
					n.writeMu.Lock()
					ch, ok := n.waiting[id]
					if ok {
						delete(n.waiting, id)
						n.writeMu.Unlock()
						b.mu.RUnlock()
						ch <- stream
						return
					}
					n.writeMu.Unlock()
				}
				b.mu.RUnlock()
			}
			stream.Close()
		}
	}
}

func (b *Bridge) handleRegister(ctx context.Context, stream net.Conn) {
	// Read registration: [1-byte flags] [name]
	var flags [1]byte
	if _, err := io.ReadFull(stream, flags[:]); err != nil {
		stream.Close()
		return
	}
	name, err := readString(stream)
	if err != nil {
		stream.Close()
		return
	}

	info := NodeInfo{
		Name:     name,
		ExitNode: flags[0]&0x01 != 0,
	}

	bn := &bridgeNode{
		info:    info,
		waiting: make(map[string]chan net.Conn),
	}

	b.mu.Lock()
	b.nodes[name] = bn
	b.mu.Unlock()

	// Block until disconnect
	<-ctx.Done()

	b.mu.Lock()
	delete(b.nodes, name)
	b.mu.Unlock()
}

// Nodes returns all connected nodes.
func (b *Bridge) Nodes() []NodeInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]NodeInfo, 0, len(b.nodes))
	for _, n := range b.nodes {
		result = append(result, n.info)
	}
	return result
}

// ExitNodes returns nodes advertising exit capability.
func (b *Bridge) ExitNodes() []NodeInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var result []NodeInfo
	for _, n := range b.nodes {
		if n.info.ExitNode {
			result = append(result, n.info)
		}
	}
	return result
}

// DialTCP dials addr through a specific node's network.
func (b *Bridge) DialTCP(ctx context.Context, nodeName string, addr string) (net.Conn, error) {
	b.mu.RLock()
	bn, ok := b.nodes[nodeName]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("relay: node %q not found", nodeName)
	}

	if bn.ctrlW == nil {
		return nil, fmt.Errorf("relay: node %q control stream not ready", nodeName)
	}

	id := fmt.Sprintf("%d", b.seq.Add(1))

	// Send dial request to node
	bn.writeMu.Lock()
	ch := make(chan net.Conn, 1)
	bn.waiting[id] = ch
	err := writeRequest(bn.ctrlW, id, addr)
	bn.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	// Node dials target, opens data stream back
	select {
	case conn := <-ch:
		if conn == nil {
			return nil, fmt.Errorf("relay: dial via %q failed", nodeName)
		}
		return conn, nil
	case <-ctx.Done():
		bn.writeMu.Lock()
		delete(bn.waiting, id)
		bn.writeMu.Unlock()
		return nil, ctx.Err()
	}
}

// HasNode checks if a node is connected.
func (b *Bridge) HasNode(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.nodes[name]
	return ok
}

// --- Node (hy2 client side) ---

// NodeConfig configures a node.
type NodeConfig struct {
	Name     string // unique name for this node
	ExitNode bool   // advertise as exit node
}

// Node connects to a bridge and can route traffic through other nodes.
type Node struct {
	cfg    NodeConfig
	mu     sync.Mutex
	client hyclient.Client
	ready  chan struct{}
}

// NewNode creates a node with the given config.
func NewNode(cfg NodeConfig) *Node {
	return &Node{
		cfg:   cfg,
		ready: make(chan struct{}),
	}
}

// Attach connects to the bridge, registers, and handles relay requests. Blocks.
func (n *Node) Attach(ctx context.Context, client hyclient.Client) error {
	// Register with bridge
	regStream, err := client.TCP(streamRegister)
	if err != nil {
		return fmt.Errorf("relay: register: %w", err)
	}
	defer regStream.Close()

	var flags byte
	if n.cfg.ExitNode {
		flags |= 0x01
	}
	regStream.Write([]byte{flags})
	writeString(regStream, n.cfg.Name)

	// Open s2c ctrl — bridge writes dial requests here, we read and dial
	s2cCtrl, err := client.TCP(streamCtrlS2C)
	if err != nil {
		return fmt.Errorf("relay: s2c ctrl: %w", err)
	}
	defer s2cCtrl.Close()
	// Send our name so bridge knows which node this ctrl belongs to
	writeString(s2cCtrl, n.cfg.Name)

	n.mu.Lock()
	n.client = client
	select {
	case <-n.ready:
	default:
		close(n.ready)
	}
	n.mu.Unlock()

	// Serve dial requests from bridge — blocks until stream breaks
	errCh := make(chan error, 1)
	go func() {
		for {
			id, addr, err := readRequest(s2cCtrl)
			if err != nil {
				errCh <- err
				return
			}
			go n.dialAndStream(ctx, client, id, addr)
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("relay: control stream closed: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *Node) dialAndStream(ctx context.Context, client hyclient.Client, id, addr string) {
	target, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return
	}
	defer target.Close()

	stream, err := client.TCP(streamDataPrefix + id + streamDataSuffix)
	if err != nil {
		return
	}
	defer stream.Close()

	go func() { io.Copy(stream, target); stream.Close() }()
	io.Copy(target, stream)
}

// DialTCP dials addr through the bridge's network (standard hy2 passthrough).
func (n *Node) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	select {
	case <-n.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	n.mu.Lock()
	client := n.client
	n.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("relay: not connected")
	}
	return client.TCP(addr)
}

// HasPeer returns true if connected to the bridge.
func (n *Node) HasPeer() bool {
	select {
	case <-n.ready:
		return true
	default:
		return false
	}
}

// --- wire helpers ---

func writeString(w io.Writer, s string) error {
	b := []byte(s)
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(len(b)))
	if _, err := w.Write(lb[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readString(r io.Reader) (string, error) {
	var lb [2]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return "", err
	}
	buf := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeRequest(w io.Writer, id, addr string) error {
	if err := writeString(w, id); err != nil {
		return err
	}
	return writeString(w, addr)
}

func readRequest(r io.Reader) (id, addr string, err error) {
	id, err = readString(r)
	if err != nil {
		return
	}
	addr, err = readString(r)
	return
}
