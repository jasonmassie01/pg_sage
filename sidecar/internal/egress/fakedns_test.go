package egress

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeResolver answers every A query with addr from a UDP server on
// loopback, so a test can make any hostname resolve to a chosen address
// without touching real DNS.
func fakeResolver(t *testing.T, addr netip.Addr) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake DNS: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go serveFakeDNS(pc, addr)
	server := pc.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", server)
		},
	}
}

func serveFakeDNS(pc net.PacketConn, addr netip.Addr) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		reply, err := fakeDNSReply(buf[:n], addr)
		if err == nil {
			_, _ = pc.WriteTo(reply, from)
		}
	}
}

func fakeDNSReply(query []byte, addr netip.Addr) ([]byte, error) {
	var p dnsmessage.Parser
	header, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	question, err := p.Question()
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: header.ID, Response: true, Authoritative: true,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(question); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	if question.Type == dnsmessage.TypeA && addr.Is4() {
		rh := dnsmessage.ResourceHeader{
			Name: question.Name, Type: dnsmessage.TypeA,
			Class: dnsmessage.ClassINET, TTL: 60,
		}
		if err := b.AResource(rh, dnsmessage.AResource{A: addr.As4()}); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}
