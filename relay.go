// Package relay provides reverse traffic forwarding through a Hysteria 2 tunnel.
//
// Architecture:
//   - Exit node: runs hy2 CLIENT, connects to entry node, then calls
//     relay.ServeExitNode which opens a control stream and waits for
//     dial requests from the entry node.
//   - Entry node: runs hy2 SERVER, uses a custom Outbound that detects
//     relay streams. Provides DialTCP for user-facing services (SOCKS5 etc).
//
// All traffic flows inside the hy2 tunnel. hy2 handles NAT traversal,
// encryption, authentication, and Brutal CC.
package relay

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
)

const (
	// StreamControl is the hy2 target address for the relay control stream.
	StreamControl = "_relay_ctrl_"
	// StreamData prefix for data streams: "_relay_data_<id>".
	streamDataPrefix = "_relay_data_"
)

// IsRelayStream checks if a target address is a relay internal stream.
func IsRelayStream(addr string) bool {
	return addr == StreamControl || len(addr) > len(streamDataPrefix) && addr[:len(streamDataPrefix)] == streamDataPrefix
}

// --- Entry Node (public IP side) ---

// EntryNode sits on the public-IP side. It provides DialTCP which sends
// traffic through the hy2 tunnel to the exit node.
type EntryNode struct {
	mu      sync.Mutex
	control net.Conn        // control stream to exit node
	pending map[string]chan net.Conn
	ready   chan struct{}
	seq     atomic.Uint64
}

func NewEntryNode() *EntryNode {
	return &EntryNode{
		pending: make(map[string]chan net.Conn),
		ready:   make(chan struct{}),
	}
}

// HandleControlStream is called when the exit node opens the control stream.
// Your hy2 server Outbound.TCP should call this when reqAddr == StreamControl.
// Blocks until the stream is closed.
func (e *EntryNode) HandleControlStream(ctx context.Context, stream net.Conn) {
	e.mu.Lock()
	e.control = stream
	select {
	case <-e.ready:
	default:
		close(e.ready)
	}
	e.mu.Unlock()

	// Block until done
	<-ctx.Done()
	stream.Close()
}

// HandleDataStream is called when the exit node opens a data stream.
// Your hy2 server Outbound.TCP should call this when reqAddr starts with
// streamDataPrefix. Returns the dial ID for logging.
func (e *EntryNode) HandleDataStream(stream net.Conn, reqAddr string) {
	dialID := reqAddr[len(streamDataPrefix):]
	e.mu.Lock()
	ch, ok := e.pending[dialID]
	if ok {
		delete(e.pending, dialID)
	}
	e.mu.Unlock()

	if ok {
		ch <- stream
	} else {
		stream.Close()
	}
}

// DialTCP opens a TCP connection to addr through the exit node.
func (e *EntryNode) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	// Wait for exit node
	select {
	case <-e.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Generate unique dial ID
	id := fmt.Sprintf("%d", e.seq.Add(1))

	// Register pending
	ch := make(chan net.Conn, 1)
	e.mu.Lock()
	e.pending[id] = ch
	ctrl := e.control
	e.mu.Unlock()

	if ctrl == nil {
		return nil, fmt.Errorf("relay: no exit node")
	}

	// Send dial request: [2-byte id_len][id][2-byte addr_len][addr]
	e.mu.Lock()
	err := writeRequest(ctrl, id, addr)
	e.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("relay: write request: %w", err)
	}

	// Wait for exit node to open data stream
	select {
	case conn := <-ch:
		return conn, nil
	case <-ctx.Done():
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (e *EntryNode) HasExitNode() bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}

// --- Exit Node (NAT'd side) ---

// ServeExitNode connects to the entry node's hy2 server, opens a control
// stream, and handles dial requests. Blocks until ctx is cancelled.
func ServeExitNode(ctx context.Context, client hyclient.Client) error {
	// Open control stream
	ctrl, err := client.TCP(StreamControl)
	if err != nil {
		return fmt.Errorf("relay: control stream: %w", err)
	}
	defer ctrl.Close()

	// Read dial requests and handle them
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		id, addr, err := readRequest(ctrl)
		if err != nil {
			return fmt.Errorf("relay: read request: %w", err)
		}

		go handleDial(ctx, client, id, addr)
	}
}

func handleDial(ctx context.Context, client hyclient.Client, dialID, addr string) {
	// Dial target locally
	target, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return // silently fail — entry side will timeout
	}
	defer target.Close()

	// Open data stream back to entry node
	stream, err := client.TCP(streamDataPrefix + dialID)
	if err != nil {
		return
	}
	defer stream.Close()

	// Relay
	go func() { io.Copy(stream, target); stream.Close() }()
	io.Copy(target, stream)
}

// --- Wire protocol (inside hy2 TCP streams) ---

func writeRequest(w io.Writer, id, addr string) error {
	idBytes := []byte(id)
	addrBytes := []byte(addr)
	buf := make([]byte, 2+len(idBytes)+2+len(addrBytes))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(idBytes)))
	copy(buf[2:], idBytes)
	off := 2 + len(idBytes)
	binary.BigEndian.PutUint16(buf[off:off+2], uint16(len(addrBytes)))
	copy(buf[off+2:], addrBytes)
	_, err := w.Write(buf)
	return err
}

func readRequest(r io.Reader) (id, addr string, err error) {
	var lenBuf [2]byte
	if _, err = io.ReadFull(r, lenBuf[:]); err != nil {
		return
	}
	idLen := binary.BigEndian.Uint16(lenBuf[:])
	idBuf := make([]byte, idLen)
	if _, err = io.ReadFull(r, idBuf); err != nil {
		return
	}
	if _, err = io.ReadFull(r, lenBuf[:]); err != nil {
		return
	}
	addrLen := binary.BigEndian.Uint16(lenBuf[:])
	addrBuf := make([]byte, addrLen)
	if _, err = io.ReadFull(r, addrBuf); err != nil {
		return
	}
	return string(idBuf), string(addrBuf), nil
}
