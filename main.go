package main

import (
	"bufio"
	"context"
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

	ds "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	"github.com/libp2p/go-libp2p/core/discovery"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	routedhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/multiformats/go-multiaddr"
)

func handleStream(s network.Stream) {
	logrus.Println("Got a new stream!")

	// Create a buffer stream for non-blocking read and write.
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	go readData(rw)
	go writeData(rw)

	// stream 's' will stay open until you close it (or the other side closes it).
}

func readData(rw *bufio.ReadWriter) {
	for {
		str, _ := rw.ReadString('\n')

		if str == "" {
			return
		}
		if str != "\n" {
			// Green console colour: 	\x1b[32m
			// Reset console colour: 	\x1b[0m
			fmt.Printf("\x1b[32m%s\x1b[0m> ", str)
		}

	}
}

func writeData(rw *bufio.ReadWriter) {
	stdReader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("> ")
		sendData, err := stdReader.ReadString('\n')
		if err != nil {
			logrus.Println(err)
			return
		}

		rw.WriteString(fmt.Sprintf("%s\n", sendData))
		rw.Flush()
	}
}

func main() {
	logrus.SetLevel(logrus.DebugLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sourcePort := flag.Int("sp", 0, "Source port number")
	dest := flag.String("d", "", "Destination multiaddr string")
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

	h.SetStreamHandler("/chat/1.0.0", handleStream)

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
	kademliaDHT := dht.NewDHT(ctx, h, dstore)
	routedHost := routedhost.Wrap(h, kademliaDHT)

	addrs := routedHost.Addrs()
	hostAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ipfs/%s", routedHost.ID()))
	for _, addr := range addrs {
		logrus.Println("Address: ", addr.Encapsulate(hostAddr))
	}

	// Bootstrap the DHT. In the default configuration, this spawns a Background
	// thread that will refresh the peer table every five minutes.
	logrus.Debug("Bootstrapping the DHT")
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		panic(err)
	}
	kademliaDHT.Host()

	time.Sleep(1 * time.Second)

	rendezvous := "cluster-xxx"
	routingDiscovery := drouting.NewRoutingDiscovery(kademliaDHT)
	dutil.Advertise(ctx, routingDiscovery, rendezvous, discovery.TTL(10*time.Second))
	peers, err := routingDiscovery.FindPeers(ctx, rendezvous)
	if err != nil {
		logrus.Fatal("Failed to find peers: ", err)
	}

	go func() {
		for {
			logrus.Info("-------------------------")
			logrus.Info("Peers in the network:")
			for _, p := range routedHost.Peerstore().Peers() {
				peer := routedHost.Peerstore().PeerInfo(p)
				logrus.Info("Peer: ", peer)
				if p != h.ID() {
					if err := routedHost.Connect(ctx, peer); err != nil {
						// logrus.Error("Failed to connect to peer: ", err)
						logrus.Info("Removing peer: ", peer)
						routedHost.Peerstore().RemovePeer(p)
						routedHost.Peerstore().ClearAddrs(p)
						routedHost.Network().ClosePeer(p)
						kademliaDHT.RoutingTable().RemovePeer(p)
						continue
					}
				}

			}
			time.Sleep(5 * time.Second)
		}
	}()

	for peer := range peers {
		if peer.ID == h.ID() {
			continue
		}
		logrus.Info("Found peer:", peer.ID)
	}

	// Wait forever
	select {}
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
