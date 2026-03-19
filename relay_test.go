package relay_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	hyserver "github.com/apernet/hysteria/core/v2/server"
	relay "github.com/FrankoonG/hysteria-relay"
)

// TestBidirectional verifies both nodes can dial through each other.
func TestBidirectional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Echo servers on both sides
	echoA, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echoA.Close()
	go serveEcho(echoA)

	echoB, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echoB.Close()
	go serveEcho(echoB)

	// Node B: hy2 server side
	nodeB := relay.NewNode()
	cert := selfSignedCert(t)
	serverConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	hyServer, _ := hyserver.NewServer(&hyserver.Config{
		Conn:          serverConn,
		TLSConfig:     hyserver.TLSConfig{Certificates: []tls.Certificate{cert}},
		Authenticator: &allowAll{},
		Outbound:      &relayOutbound{node: nodeB, ctx: ctx},
	})
	go hyServer.Serve()
	defer hyServer.Close()

	// Node A: hy2 client side
	nodeA := relay.NewNode()
	hyClient, _, err := hyclient.NewClient(&hyclient.Config{
		ServerAddr: serverConn.LocalAddr().(*net.UDPAddr),
		Auth:       "test",
		TLSConfig:  hyclient.TLSConfig{InsecureSkipVerify: true, ServerName: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hyClient.Close()

	go nodeA.Attach(ctx, hyClient)
	time.Sleep(500 * time.Millisecond)

	// Direction 1: A dials through B's network (reaches echoB)
	t.Run("A→B", func(t *testing.T) {
		conn, err := nodeA.DialTCP(ctx, echoB.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		testEcho(t, conn, "hello from A through B")
	})

	// Direction 2: B dials through A's network (reaches echoA)
	t.Run("B→A", func(t *testing.T) {
		conn, err := nodeB.DialTCP(ctx, echoA.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		testEcho(t, conn, "hello from B through A")
	})
}

func TestConcurrentBidirectional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go serveEcho(echo)

	nodeB := relay.NewNode()
	cert := selfSignedCert(t)
	serverConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	hyServer, _ := hyserver.NewServer(&hyserver.Config{
		Conn:          serverConn,
		TLSConfig:     hyserver.TLSConfig{Certificates: []tls.Certificate{cert}},
		Authenticator: &allowAll{},
		Outbound:      &relayOutbound{node: nodeB, ctx: ctx},
	})
	go hyServer.Serve()
	defer hyServer.Close()

	nodeA := relay.NewNode()
	hyClient, _, _ := hyclient.NewClient(&hyclient.Config{
		ServerAddr: serverConn.LocalAddr().(*net.UDPAddr),
		Auth:       "test",
		TLSConfig:  hyclient.TLSConfig{InsecureSkipVerify: true, ServerName: "test"},
	})
	defer hyClient.Close()
	go nodeA.Attach(ctx, hyClient)
	time.Sleep(500 * time.Millisecond)

	const N = 10
	errs := make(chan error, N*2)

	// N streams A→B
	for i := 0; i < N; i++ {
		go func() {
			conn, err := nodeA.DialTCP(ctx, echo.Addr().String())
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			errs <- echoCheck(conn, "A→B")
		}()
	}
	// N streams B→A
	for i := 0; i < N; i++ {
		go func() {
			conn, err := nodeB.DialTCP(ctx, echo.Addr().String())
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			errs <- echoCheck(conn, "B→A")
		}()
	}

	failures := 0
	for i := 0; i < N*2; i++ {
		if err := <-errs; err != nil {
			failures++
		}
	}
	if failures > 2 {
		t.Fatalf("%d/%d streams failed", failures, N*2)
	}
	t.Logf("%d/%d bidirectional streams OK", N*2-failures, N*2)
}

// --- helpers ---

type relayOutbound struct{ node *relay.Node; ctx context.Context }

func (o *relayOutbound) TCP(reqAddr string) (net.Conn, error) {
	if relay.IsRelayStream(reqAddr) {
		c1, c2 := net.Pipe()
		go o.node.HandleStream(o.ctx, reqAddr, c1)
		return c2, nil
	}
	return net.DialTimeout("tcp", reqAddr, 5*time.Second)
}

func (o *relayOutbound) UDP(addr string) (hyserver.UDPConn, error) {
	return &dummyUDP{}, nil
}

func serveEcho(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); io.Copy(c, c) }()
	}
}

func testEcho(t *testing.T, conn net.Conn, msg string) {
	t.Helper()
	conn.Write([]byte(msg))
	buf := make([]byte, 1500)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != msg {
		t.Fatalf("got %q, want %q", buf[:n], msg)
	}
	t.Logf("echo OK: %q", msg)
}

func echoCheck(conn net.Conn, msg string) error {
	conn.Write([]byte(msg))
	buf := make([]byte, 100)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	if string(buf[:n]) != msg {
		return io.ErrUnexpectedEOF
	}
	return nil
}

type allowAll struct{}

func (a *allowAll) Authenticate(addr net.Addr, auth string, tx uint64) (bool, string) {
	return true, "user"
}

type dummyUDP struct{}

func (d *dummyUDP) ReadFrom(b []byte) (int, string, error) { select {} }
func (d *dummyUDP) WriteTo(b []byte, addr string) (int, error) { return len(b), nil }
func (d *dummyUDP) Close() error                               { return nil }

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Verify IsRelayStream
func TestIsRelayStream(t *testing.T) {
	for _, tc := range []struct{ addr string; want bool }{
		{"_relay_c2s_ctrl_", true},
		{"_relay_s2c_ctrl_", true},
		{"_relay_data_123", true},
		{"example.com:443", false},
		{"10.0.0.1:22", false},
	} {
		if got := relay.IsRelayStream(tc.addr); got != tc.want {
			t.Errorf("IsRelayStream(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
