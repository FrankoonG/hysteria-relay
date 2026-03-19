// Package relay provides bidirectional traffic forwarding through a Hysteria 2 tunnel.
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
	streamCtrlC2S    = "_relay_c2s_ctrl_:0"
	streamCtrlS2C    = "_relay_s2c_ctrl_:0"
	streamDataPrefix = "_relay_data_"  // suffixed with id:0
	streamDataSuffix = ":0"
)

func IsRelayStream(addr string) bool {
	return addr == streamCtrlC2S || addr == streamCtrlS2C ||
		strings.HasPrefix(addr, streamDataPrefix)
}

type Node struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	ctrlW   net.Conn                 // write dial requests to peer
	client  hyclient.Client          // non-nil if we're the hy2 client side
	waiting map[string]chan net.Conn
	ready   chan struct{}
	seq     atomic.Uint64
}

func NewNode() *Node {
	return &Node{
		waiting: make(map[string]chan net.Conn),
		ready:   make(chan struct{}),
	}
}

// Attach is called on the hy2 CLIENT side.
func (n *Node) Attach(ctx context.Context, client hyclient.Client) error {
	c2sCtrl, err := client.TCP(streamCtrlC2S)
	if err != nil {
		return err
	}
	defer c2sCtrl.Close()

	s2cCtrl, err := client.TCP(streamCtrlS2C)
	if err != nil {
		return err
	}
	defer s2cCtrl.Close()

	n.mu.Lock()
	n.ctrlW = c2sCtrl
	n.client = client
	select {
	case <-n.ready:
	default:
		close(n.ready)
	}
	n.mu.Unlock()

	// Handle dial requests from server (B→A direction)
	go func() {
		for {
			id, addr, err := readRequest(s2cCtrl)
			if err != nil {
				return
			}
			go n.dialAndStream(ctx, client, id, addr)
		}
	}()

	<-ctx.Done()
	return ctx.Err()
}

// dialAndStream dials target locally and opens data stream via client.TCP.
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

// HandleStream is called on the hy2 SERVER side from Outbound.TCP.
func (n *Node) HandleStream(ctx context.Context, reqAddr string, stream net.Conn) {
	switch reqAddr {
	case streamCtrlC2S:
		// Client's dial requests → server dials targets, waits for data streams
		n.serveDials(ctx, stream)

	case streamCtrlS2C:
		// Server's outbound ctrl → server writes dial requests here
		n.mu.Lock()
		n.ctrlW = stream
		select {
		case <-n.ready:
		default:
			close(n.ready)
		}
		n.mu.Unlock()
		<-ctx.Done()

	default:
		if strings.HasPrefix(reqAddr, streamDataPrefix) {
			id := strings.TrimSuffix(reqAddr[len(streamDataPrefix):], streamDataSuffix)
			// Try to deliver to a waiter, with retries for race conditions
			for i := 0; i < 50; i++ {
				n.mu.Lock()
				ch, ok := n.waiting[id]
				if ok {
					delete(n.waiting, id)
					n.mu.Unlock()
					ch <- stream
					return
				}
				n.mu.Unlock()
				time.Sleep(10 * time.Millisecond)
			}
			stream.Close()
		}
	}
}

// serveDials handles dial requests from client side.
// Server dials target, registers in waiting, client opens data stream.
func (n *Node) serveDials(ctx context.Context, ctrl net.Conn) {
	for {
		id, addr, err := readRequest(ctrl)
		if err != nil {
			return
		}
		go func() {
			target, err := net.DialTimeout("tcp", addr, 10*time.Second)
			if err != nil {
				return
			}

			ch := make(chan net.Conn, 1)
			n.mu.Lock()
			n.waiting[id] = ch
			n.mu.Unlock()

			select {
			case stream := <-ch:
				defer stream.Close()
				defer target.Close()
				go func() { io.Copy(stream, target); stream.Close() }()
				io.Copy(target, stream)
			case <-time.After(15 * time.Second):
				n.mu.Lock()
				delete(n.waiting, id)
				n.mu.Unlock()
				target.Close()
			case <-ctx.Done():
				target.Close()
			}
		}()
	}
}

// DialTCP dials addr through the peer's network.
func (n *Node) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	select {
	case <-n.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	id := fmt.Sprintf("%d", n.seq.Add(1))

	n.mu.Lock()
	ctrl := n.ctrlW
	client := n.client // non-nil if we're hy2 client side
	n.mu.Unlock()

	if ctrl == nil {
		return nil, fmt.Errorf("relay: no peer")
	}

	// Send dial request to peer
	n.writeMu.Lock()
	err := writeRequest(ctrl, id, addr)
	n.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	if client != nil {
		// We're hy2 client side. Peer (server) dials target and waits in
		// waiting map. We open data stream to deliver it.
		stream, err := client.TCP(streamDataPrefix + id + streamDataSuffix)
		if err != nil {
			return nil, err
		}
		return stream, nil
	}

	// We're hy2 server side. Peer (client) will dial target and open
	// data stream to us. Wait for it.
	ch := make(chan net.Conn, 1)
	n.mu.Lock()
	n.waiting[id] = ch
	n.mu.Unlock()

	select {
	case conn := <-ch:
		if conn == nil {
			return nil, fmt.Errorf("relay: dial failed")
		}
		return conn, nil
	case <-ctx.Done():
		n.mu.Lock()
		delete(n.waiting, id)
		n.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (n *Node) HasPeer() bool {
	select {
	case <-n.ready:
		return true
	default:
		return false
	}
}

func writeRequest(w io.Writer, id, addr string) error {
	idB, addrB := []byte(id), []byte(addr)
	buf := make([]byte, 2+len(idB)+2+len(addrB))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(idB)))
	copy(buf[2:], idB)
	off := 2 + len(idB)
	binary.BigEndian.PutUint16(buf[off:off+2], uint16(len(addrB)))
	copy(buf[off+2:], addrB)
	_, err := w.Write(buf)
	return err
}

func readRequest(r io.Reader) (id, addr string, err error) {
	var lb [2]byte
	if _, err = io.ReadFull(r, lb[:]); err != nil {
		return
	}
	idBuf := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err = io.ReadFull(r, idBuf); err != nil {
		return
	}
	if _, err = io.ReadFull(r, lb[:]); err != nil {
		return
	}
	addrBuf := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err = io.ReadFull(r, addrBuf); err != nil {
		return
	}
	return string(idBuf), string(addrBuf), nil
}
