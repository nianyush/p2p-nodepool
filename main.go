package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand"
	"os"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/sirupsen/logrus"

	"github.com/creachadair/otp"
	ds "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	routedhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/multiformats/go-multiaddr"
)

func TOTP(t int, key string) string {
	cfg := otp.Config{
		Hash:     sha1.New,
		Digits:   6,
		TimeStep: otp.TimeWindow(t),
		Key:      key,
		Format: func(hash []byte, nb int) string {
			return base64.StdEncoding.EncodeToString(hash)[:nb]
		},
	}
	return cfg.TOTP()
}

var peers = make(map[peer.ID]struct{})
var mutext sync.Mutex

func main() {
	logrus.SetLevel(logrus.DebugLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sourcePort := flag.Int("sp", 0, "Source port number")
	dest := flag.String("d", "", "Destination multiaddr string")
	key := flag.String("o", "", "OTP key")
	rendezvous := flag.String("r", "", "Rendezvous string")
	help := flag.Bool("help", false, "Display help")
	flag.Parse()

	if *help {
		fmt.Printf("This program demonstrates a simple p2p chat application using libp2p\n\n")
		fmt.Println("Usage: Run './chat -sp <SOURCE_PORT>' where <SOURCE_PORT> can be any port number.")
		fmt.Println("Now run './chat -d <MULTIADDR>' where <MULTIADDR> is multiaddress of previous listener host.")

		os.Exit(0)
	}

	r := mrand.New(mrand.NewSource(int64(*sourcePort)))

	h, err := makeHost(*sourcePort, r)
	if err != nil {
		logrus.Fatal("failed to create host: ", err)
	}

	defer h.Close()

	logrus.Info("Host created. We are:", h.ID())
	logrus.Info(h.Addrs())

	var bootstrapPeers []peer.AddrInfo
	if dest != nil && len(*dest) > 0 {
		peerAddrInfo, err := getPeerAddrInfo(*dest)
		if err != nil {
			logrus.Fatal("Failed to parse peer multiaddress: ", err)
		}
		logrus.Info("Peer address info: ", peerAddrInfo)
		bootstrapPeers = append(bootstrapPeers, *peerAddrInfo)
		if err := bootstrapConnect(ctx, h, bootstrapPeers); err != nil {
			logrus.Fatal("Failed to bootstrap with peer: ", err)
		}
	}

	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
	// Construct a datastore (needed by the DHT). This is just a simple, in-memory thread-safe datastore.
	dstore := dsync.MutexWrap(ds.NewMapDatastore())
	kademliaDHT, err := dht.New(ctx, h, dht.Datastore(dstore), dht.Mode(dht.ModeServer))
	if err != nil {
		logrus.Fatal("Failed to create DHT: ", err)
	}
	routedHost := routedhost.Wrap(h, kademliaDHT)
	// Bootstrap the DHT. In the default configuration, this spawns a Background
	// thread that will refresh the peer table every five minutes.
	logrus.Debug("Bootstrapping the DHT")
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		panic(err)
	}

	time.Sleep(1 * time.Second)
	routingDiscovery := drouting.NewRoutingDiscovery(kademliaDHT)

	if key == nil {
		defaultKey := "test"
		key = &defaultKey
	}

	addrs := routedHost.Addrs()
	hostAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ipfs/%s", routedHost.ID()))
	for _, addr := range addrs {
		join := fmt.Sprintf("./p2p -d %s -o %s", addr.Encapsulate(hostAddr), *key)
		logrus.Info("Join with: ", join)
	}

	// generate one time rendezvous string as otp
	go func() {
		ttl := 5 * time.Second
		for {
			// generate random string
			otp := TOTP(10, *key)

			if rendezvous != nil && len(*rendezvous) > 0 {
				otp = *rendezvous
			}
			logrus.Info("OTP: ", otp)

			_, _ = routingDiscovery.Advertise(ctx, otp)
			time.Sleep(1 * time.Second)

			peerChans, err := routingDiscovery.FindPeers(ctx, otp)
			if err != nil {
				logrus.Error("Failed to find peers: ", err)
			} else {
				connectToPeers(ctx, routedHost, peerChans)
			}

			time.Sleep(ttl)
		}
	}()

	go func() {
		for {
			logrus.Info("-------------------------")
			logrus.Info("Peers:")
			for p := range peers {
				peer := routedHost.Peerstore().PeerInfo(p)
				logrus.Info("Peer: ", peer)
				if p != h.ID() {
					if err := routedHost.Connect(ctx, peer); err != nil {
						// logrus.Error("Failed to connect to peer: ", err)
						logrus.Info("Removing peer: ", peer)
						routedHost.Peerstore().RemovePeer(p)
						routedHost.Peerstore().ClearAddrs(p)
						_ = routedHost.Network().ClosePeer(p)
						kademliaDHT.RoutingTable().RemovePeer(p)
						mutext.Lock()
						delete(peers, p)
						mutext.Unlock()
						continue
					}
				}

			}
			time.Sleep(5 * time.Second)
		}
	}()

	// Wait forever
	select {}
}

func connectToPeers(ctx context.Context, h host.Host, peerChans <-chan peer.AddrInfo) {
	logrus.Info("Connecting to peers", len(peerChans))
	for pc := range peerChans {
		if pc.ID == h.ID() || len(pc.Addrs) == 0 {
			continue
		}

		if h.Network().Connectedness(pc.ID) != network.Connected {
			logrus.Info("Found peer: ", pc.ID)
			if err := h.Connect(ctx, pc); err != nil {
				logrus.Error("Failed to connect to peer: ", err)
				continue
			} else {
				logrus.Info("Connected to peer: ", pc.ID)
			}
		} else {
			logrus.Info("Already connected to peer: ", pc.ID)
		}
		mutext.Lock()
		peers[pc.ID] = struct{}{}
		mutext.Unlock()
	}
}

func getPeerAddrInfo(dest string) (*peer.AddrInfo, error) {
	// Turn the destination into a multiaddr.
	maddr, err := multiaddr.NewMultiaddr(dest)
	if err != nil {
		logrus.Error("failed to parse multiaddress: ", err)
		return nil, err
	}

	// Extract the peer ID from the multiaddr.
	info, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		logrus.Error("failed to extract peer ID from multiaddress: ", err)
		return nil, err
	}

	return info, nil
}

func makeHost(port int, randomness io.Reader) (host.Host, error) {
	// Creates a new RSA key pair for this host.
	prvKey, _, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, randomness)
	if err != nil {
		logrus.Println(err)
		return nil, err
	}

	// 0.0.0.0 will listen on any interface device.
	sourceMultiAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port))

	// libp2p.New constructs a new libp2p Host.
	// Other options can be added here.
	return libp2p.New(
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.DefaultTransports,
		libp2p.ListenAddrs(sourceMultiAddr),
		libp2p.Identity(prvKey),
	)
}

func bootstrapConnect(ctx context.Context, ph host.Host, peers []peer.AddrInfo) error {
	if len(peers) < 1 {
		return errors.New("not enough bootstrap peers")
	}

	errs := make(chan error, len(peers))
	var wg sync.WaitGroup
	for _, p := range peers {

		// performed asynchronously because when performed synchronously, if
		// one `Connect` call hangs, subsequent calls are more likely to
		// fail/abort due to an expiring context.
		// Also, performed asynchronously for dial speed.

		wg.Add(1)
		go func(p peer.AddrInfo) {
			defer wg.Done()
			defer logrus.Println(ctx, "bootstrapDial", ph.ID(), p.ID)
			logrus.Printf("%s bootstrapping to %s", ph.ID(), p.ID)

			ph.Peerstore().AddAddrs(p.ID, p.Addrs, peerstore.PermanentAddrTTL)
			if err := ph.Connect(ctx, p); err != nil {
				logrus.Println(ctx, "bootstrapDialFailed", p.ID)
				logrus.Printf("failed to bootstrap with %v: %s", p.ID, err)
				errs <- err
				return
			}
			logrus.Println(ctx, "bootstrapDialSuccess", p.ID)
			logrus.Printf("bootstrapped with %v", p.ID)
		}(p)
	}
	wg.Wait()

	// our failure condition is when no connection attempt succeeded.
	// So drain the errs channel, counting the results.
	close(errs)
	count := 0
	var err error
	for err = range errs {
		if err != nil {
			count++
		}
	}
	if count == len(peers) {
		return fmt.Errorf("failed to bootstrap. %s", err)
	}
	return nil
}
