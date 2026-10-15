package gateway

import (
	"context"
	"testing"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/store"
)

// reportSource records acknowledgements and fails the certificate report
// while failReport is set.
type reportSource struct {
	acks       []NodeAck
	failReport bool
}

func (s *reportSource) Load(context.Context, string, bool) (store.SnapshotData, error) {
	return store.SnapshotData{}, nil
}
func (s *reportSource) Watch(ctx context.Context, _ func(string)) error {
	<-ctx.Done()
	return ctx.Err()
}
func (s *reportSource) Ack(_ context.Context, a NodeAck) error {
	s.acks = append(s.acks, a)
	if a.CertificatesIncluded && s.failReport {
		return ErrCertificatesNotRecorded
	}
	return nil
}

// A certificate report the control plane failed to record is sent again with
// the next heartbeat, not treated as delivered for the refresh interval; the
// acknowledgement itself still succeeds.
func TestFailedCertificateReportIsResent(t *testing.T) {
	g := newTestGateway(t)
	g.certs.Register(func() []certs.Status { return []certs.Status{{Name: "gateway", Source: "file"}} })
	src := &reportSource{failReport: true}
	w := NewWatcherWithSource(g, src, nil, 0)
	ack := NodeAck{NodeID: "n1", Revision: 1}

	if err := w.sendAck(context.Background(), ack); err != nil {
		t.Fatalf("acknowledgement failed because the report did: %v", err)
	}
	src.failReport = false
	if err := w.sendAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if err := w.sendAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	got := []bool{}
	for _, a := range src.acks {
		got = append(got, a.CertificatesIncluded)
	}
	if len(got) != 3 || !got[0] || !got[1] || got[2] {
		t.Fatalf("certificates included per ack = %v, want [true true false]", got)
	}
}
