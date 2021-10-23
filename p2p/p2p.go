package p2p

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	ds "github.com/ipfs/go-datastore"
	"github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	connmgr "github.com/libp2p/go-libp2p-connmgr"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/metrics"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p-core/routing"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	noise "github.com/libp2p/go-libp2p-noise"
	quic "github.com/libp2p/go-libp2p-quic-transport"
	swarm "github.com/libp2p/go-libp2p-swarm"
	tls "github.com/libp2p/go-libp2p-tls"
	basichost "github.com/libp2p/go-libp2p/p2p/host/basic"
	"github.com/libp2p/go-libp2p/p2p/host/relay"
	"github.com/libp2p/go-tcp-transport"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/multierr"
)

const (
	DesiredRelays  = 2
	RelayBootDelay = 10 * time.Second

	DHTProtocolPrefix protocol.ID = "/awl"

	protectedBootstrapPeerTag = "bootstrap"
)

type HostConfig struct {
	PrivKeyBytes   []byte
	ListenAddrs    []multiaddr.Multiaddr
	UserAgent      string
	BootstrapPeers []peer.AddrInfo

	Libp2pOpts  []libp2p.Option
	ConnManager struct {
		LowWater    int
		HighWater   int
		GracePeriod time.Duration
	}
	Peerstore    peerstore.Peerstore
	DHTDatastore ds.Batching
	DHTOpts      []dht.Option
}

type P2p struct {
	// has to be 64-bit aligned
	openedStreams        int64
	totalStreamsInbound  int64
	totalStreamsOutbound int64

	logger    *log.ZapEventLogger
	ctx       context.Context
	ctxCancel func()

	host             host.Host
	basicHost        *basichost.BasicHost
	dht              *dht.IpfsDHT
	bandwidthCounter metrics.Reporter
	connManager      *connmgr.BasicConnMgr
	bootstrapPeers   []peer.AddrInfo
	startedAt        time.Time
}

func NewP2p(ctx context.Context) *P2p {
	newCtx, ctxCancel := context.WithCancel(ctx)
	return &P2p{
		ctx:       newCtx,
		ctxCancel: ctxCancel,
		logger:    log.Logger("awl/p2p"),
	}
}

func (p *P2p) InitHost(hostConfig HostConfig) (host.Host, error) {
	var privKey crypto.PrivKey
	var err error
	if hostConfig.PrivKeyBytes == nil {
		privKey, _, err = crypto.GenerateEd25519Key(rand.Reader)
		if err != nil {
			return nil, err
		}
	} else {
		privKey, err = crypto.UnmarshalEd25519PrivateKey(hostConfig.PrivKeyBytes)
		if err != nil {
			return nil, err
		}
	}

	p.bandwidthCounter = metrics.NewBandwidthCounter()
	p.bootstrapPeers = hostConfig.BootstrapPeers

	p.connManager = connmgr.NewConnManager(
		hostConfig.ConnManager.LowWater,
		hostConfig.ConnManager.HighWater,
		hostConfig.ConnManager.GracePeriod,
	)

	relay.DesiredRelays = DesiredRelays
	relay.BootDelay = RelayBootDelay

	p2pHost, err := libp2p.New(p.ctx,
		libp2p.Peerstore(hostConfig.Peerstore),
		libp2p.Identity(privKey),
		libp2p.UserAgent(hostConfig.UserAgent),
		libp2p.BandwidthReporter(p.bandwidthCounter),
		libp2p.ConnectionManager(p.connManager),
		libp2p.ListenAddrs(hostConfig.ListenAddrs...),
		libp2p.ChainOptions(
			libp2p.Transport(quic.NewTransport),
			libp2p.Transport(tcp.NewTCPTransport),
		),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			opts := []dht.Option{
				dht.Datastore(hostConfig.DHTDatastore),
				dht.ProtocolPrefix(DHTProtocolPrefix),
				dht.BootstrapPeers(p.bootstrapPeers...),
			}
			opts = append(opts, hostConfig.DHTOpts...)
			kademliaDHT, err := dht.New(p.ctx, h, opts...)
			p.dht = kademliaDHT
			p.basicHost = h.(*basichost.BasicHost)
			return p.dht, err
		}),
		libp2p.DefaultMuxers,
		libp2p.ChainOptions(
			libp2p.Security(tls.ID, tls.New),
			libp2p.Security(noise.ID, noise.New),
		),
		libp2p.ChainOptions(hostConfig.Libp2pOpts...),
	)
	if err != nil {
		return nil, err
	}
	p.host = p2pHost
	p.startedAt = time.Now()

	logger := p.logger
	notifyBundle := &network.NotifyBundle{
		OpenedStreamF: func(_ network.Network, stream network.Stream) {
			if p == nil {
				logger.Warn("notifyBundle: unexpected P2p object is nil")
				return
			} else if stream == nil {
				logger.Warn("notifyBundle: unexpected stream object is nil")
				return
			}
			atomic.AddInt64(&p.openedStreams, 1)
			switch stream.Stat().Direction {
			case network.DirInbound:
				atomic.AddInt64(&p.totalStreamsInbound, 1)
			case network.DirOutbound:
				atomic.AddInt64(&p.totalStreamsOutbound, 1)
			}
		},
		ClosedStreamF: func(_ network.Network, _ network.Stream) {
			atomic.AddInt64(&p.openedStreams, -1)
		},
	}
	p.host.Network().Notify(notifyBundle)

	return p2pHost, nil
}

func (p *P2p) Close() error {
	p.ctxCancel()
	err := multierr.Append(
		p.dht.Close(),
		p.host.Close(),
	)
	return err
}

func (p *P2p) ClearBackoff(peerID peer.ID) {
	p.host.Network().(*swarm.Swarm).Backoff().Clear(peerID)
}

func (p *P2p) FindPeer(ctx context.Context, id peer.ID) (peer.AddrInfo, error) {
	return p.dht.FindPeer(ctx, id)
}

func (p *P2p) ConnectPeerAddr(ctx context.Context, peerInfo peer.AddrInfo) error {
	return p.host.Connect(ctx, peerInfo)
}

func (p *P2p) ChangeProtectedStatus(peerID peer.ID, tag string, protected bool) {
	if protected {
		p.host.ConnManager().Protect(peerID, tag)
	} else {
		p.host.ConnManager().Unprotect(peerID, tag)
	}
}

func (p *P2p) IsConnected(peerID peer.ID) bool {
	return p.host.Network().Connectedness(peerID) == network.Connected
}

func (p *P2p) UserAgent(peerID peer.ID) string {
	version, _ := p.host.Peerstore().Get(peerID, "AgentVersion")

	if version != nil {
		return version.(string)
	}

	return ""
}

func (p *P2p) ConnsToPeer(peerID peer.ID) []network.Conn {
	return p.host.Network().ConnsToPeer(peerID)
}

func (p *P2p) ConnectedPeersCount() int {
	return len(p.host.Network().Peers())
}

func (p *P2p) RoutingTableSize() int {
	return p.dht.RoutingTable().Size()
}

func (p *P2p) PeersWithAddrsCount() int {
	return len(p.host.Peerstore().PeersWithAddrs())
}

func (p *P2p) AnnouncedAs() []multiaddr.Multiaddr {
	return p.host.Addrs()
}

func (p *P2p) Reachability() network.Reachability {
	return p.basicHost.GetAutoNat().Status()
}

func (p *P2p) TrimOpenConnections() {
	p.connManager.TrimOpenConns(p.ctx)
}

func (p *P2p) OpenConnectionsCount() int {
	return p.connManager.GetInfo().ConnCount
}

func (p *P2p) OpenStreamsCount() int64 {
	return atomic.LoadInt64(&p.openedStreams)
}

func (p *P2p) TotalStreamsInbound() int64 {
	return atomic.LoadInt64(&p.totalStreamsInbound)
}

func (p *P2p) TotalStreamsOutbound() int64 {
	return atomic.LoadInt64(&p.totalStreamsOutbound)
}

func (p *P2p) OpenStreamStats() map[protocol.ID]map[string]int {
	stats := make(map[protocol.ID]map[string]int)

	for _, conn := range p.host.Network().Conns() {
		for _, stream := range conn.GetStreams() {
			direction := ""
			switch stream.Stat().Direction {
			case network.DirInbound:
				direction = "inbound"
			case network.DirOutbound:
				direction = "outbound"
			case network.DirUnknown:
				direction = "unknown"
			}
			protocolStats, ok := stats[stream.Protocol()]
			if !ok {
				protocolStats = make(map[string]int)
				stats[stream.Protocol()] = protocolStats
			}
			protocolStats[direction]++
		}
	}

	return stats
}

func (p *P2p) ConnectionsLastTrimAgo() time.Duration {
	lastTrim := p.connManager.GetInfo().LastTrim
	if lastTrim.IsZero() {
		lastTrim = p.startedAt
	}
	return time.Since(lastTrim)
}

func (p *P2p) OwnObservedAddrs() []multiaddr.Multiaddr {
	return p.basicHost.IDService().OwnObservedAddrs()
}

func (p *P2p) NetworkStats() metrics.Stats {
	return p.bandwidthCounter.GetBandwidthTotals()
}

func (p *P2p) NetworkStatsByProtocol() map[protocol.ID]metrics.Stats {
	return p.bandwidthCounter.GetBandwidthByProtocol()
}

func (p *P2p) NetworkStatsByPeer() map[peer.ID]metrics.Stats {
	return p.bandwidthCounter.GetBandwidthByPeer()
}

func (p *P2p) NetworkStatsForPeer(peerID peer.ID) metrics.Stats {
	return p.bandwidthCounter.GetBandwidthForPeer(peerID)
}

// BootstrapPeersStats returns total peers count and connected count.
func (p *P2p) BootstrapPeersStats() (int, int) {
	connected := 0
	for _, peerAddr := range p.bootstrapPeers {
		if p.IsConnected(peerAddr.ID) {
			connected += 1
		}
	}

	return len(p.bootstrapPeers), connected
}

func (p *P2p) SubscribeConnectionEvents(onConnected, onDisconnected func(network.Network, network.Conn)) {
	notifyBundle := &network.NotifyBundle{
		ConnectedF:    onConnected,
		DisconnectedF: onDisconnected,
	}
	p.host.Network().Notify(notifyBundle)
}

func (p *P2p) NewStream(ctx context.Context, id peer.ID, proto protocol.ID) (network.Stream, error) {
	stream, err := p.host.NewStream(ctx, id, proto)
	return stream, err
}

func (p *P2p) Bootstrap() error {
	p.logger.Debug("Bootstrapping the DHT")
	// connect to the bootstrap nodes first
	ctx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup

	for _, peerAddr := range p.bootstrapPeers {
		wg.Add(1)
		peerAddr := peerAddr
		p.ChangeProtectedStatus(peerAddr.ID, protectedBootstrapPeerTag, true)

		go func() {
			defer wg.Done()
			if err := p.host.Connect(ctx, peerAddr); err != nil && err != context.Canceled {
				p.logger.Warnf("Connect to bootstrap node %v: %v", peerAddr.ID, err)
			} else if err == nil {
				p.logger.Infof("Connection established with bootstrap node: %v", peerAddr.ID)
			}
		}()
	}
	wg.Wait()
	p.logger.Info("Connection established with all bootstrap nodes")

	if err := p.dht.Bootstrap(p.ctx); err != nil {
		return fmt.Errorf("bootstrap dht: %v", err)
	}

	return nil
}

func (p *P2p) Uptime() time.Duration {
	return time.Since(p.startedAt)
}
