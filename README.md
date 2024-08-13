# Simple node pool implementation using libp2p

## Usage

1. Build

```shell
go build -o p2p main.go
```

2. Run init node

```shell
./p2p -sp 9000
```

It will output the node id and ip of the init node

```
INFO[0000] Address:  /ip4/192.168.0.66/tcp/9000/p2p/QmYWJTzfzBZyjZQRgmkrHpcJXZvnLpVTVUi5wMAUZbTeXd
```

3. Run other nodes to join the network

```shell
./p2p -d /ip4/192.168.0.66/tcp/9000/p2p/QmYWJTzfzBZyjZQRgmkrHpcJXZvnLpVTVUi5wMAUZbTeXd
```

Note:

- Inactive nodes will be removed from the network every 5 seconds
