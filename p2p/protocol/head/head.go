package head

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p-core/host"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	gostream "github.com/libp2p/go-libp2p-gostream"
	multistream "github.com/multiformats/go-multistream"
)

const closeTimeout = 30 * time.Second

var log = logging.Logger("go-legs/head")

type Publisher struct {
	rl     sync.RWMutex
	root   cid.Cid
	server *http.Server
}

func NewPublisher() *Publisher {
	p := &Publisher{
		server: &http.Server{},
	}
	p.server.Handler = http.Handler(p)
	return p
}

func deriveProtocolID(topic string) protocol.ID {
	return protocol.ID(path.Join("/legs/head", topic, "0.0.1"))
}

func (p *Publisher) Serve(host host.Host, topic string) error {
	pid := deriveProtocolID(topic)
	l, err := gostream.Listen(host, pid)
	if err != nil {
		log.Errorw("Failed to listen to gostream with protocol", "host", host.ID(), "protocolID", pid)
		return err
	}
	log.Infow("Serving gostream", "host", host.ID(), "protocolID", pid)
	return p.server.Serve(l)
}

func QueryRootCid(ctx context.Context, host host.Host, topic string, peerID peer.ID) (cid.Cid, error) {
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				addrInfo := peer.AddrInfo{
					ID: peerID,
				}
				err := host.Connect(ctx, addrInfo)
				if err != nil {
					return nil, err
				}
				conn, err := gostream.Dial(ctx, host, peerID, deriveProtocolID(topic))
				if err != nil {
					// If protocol ID is wrong, then try the old "double-slashed" protocol ID.
					//
					// TODO: remove this code when all providers have upgraded.
					if !errors.Is(err, multistream.ErrNotSupported) {
						return nil, err
					}
					oldProtoID := protocol.ID("/legs/head/" + topic + "/0.0.1")
					conn, err = gostream.Dial(ctx, host, peerID, oldProtoID)
					if err != nil {
						return nil, err
					}
					log.Infow("Peer head CID server uses old protocol ID", "peer", peerID, "proto", oldProtoID)
				}
				return conn, err
			},
		},
	}

	// The httpclient expects there to be a host here. `.invalid` is a reserved
	// TLD for this purpose. See
	// https://datatracker.ietf.org/doc/html/rfc2606#section-2
	resp, err := client.Get("http://unused.invalid/head")
	if err != nil {
		return cid.Undef, err
	}
	defer resp.Body.Close()

	cidStr, err := io.ReadAll(resp.Body)
	if err != nil {
		return cid.Undef, fmt.Errorf("cannot fully read response body: %w", err)
	}
	if len(cidStr) == 0 {
		log.Debug("No head is set; returning cid.Undef")
		return cid.Undef, nil
	}

	cs := string(cidStr)
	decode, err := cid.Decode(cs)
	if err != nil {
		return cid.Undef, fmt.Errorf("failed to decode CID %s: %w", cs, err)
	}

	log.Debugw("Sucessfully queried latest head", "head", decode)
	return decode, nil
}

func (p *Publisher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	base := path.Base(r.URL.Path)
	if base != "head" {
		log.Debug("Only head is supported; rejecting request with different base path")
		http.Error(w, "", http.StatusNotFound)
		return
	}

	p.rl.RLock()
	defer p.rl.RUnlock()
	var out []byte
	if p.root != cid.Undef {
		currentHead := p.root.String()
		log.Debug("Found current head: %s", currentHead)
		out = []byte(currentHead)
	} else {
		log.Debug("No head is set; responding with empty")
	}

	_, err := w.Write(out)
	if err != nil {
		log.Errorw("Failed to write response", "err", err)
	}
}

func (p *Publisher) UpdateRoot(_ context.Context, c cid.Cid) error {
	p.rl.Lock()
	defer p.rl.Unlock()
	p.root = c
	return nil
}

func (p *Publisher) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	return p.server.Shutdown(ctx)
}
