package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"xip/xip"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func main() {
	var wg sync.WaitGroup
	var x xip.Xip
	var err error
	x.Metrics = &xip.Metrics{}
	// the sole flag, `-etcdHost`, is primarily meant for integration tests
	var etcdEndpoint = flag.String("etcdHost", "localhost:2379", "etcd")
	var blocklistURL = flag.String("blocklistURL", "https://raw.githubusercontent.com/cunnie/sslip.io/main/etc/blocklist.txt", `a list of "forbidden" names`)
	flag.Parse()

	// connect to `etcd`; if there's an error, set etcdCli to `nil` and that to
	// determine whether to use a local key-value store instead
	x.Etcd, err = clientv3New(*etcdEndpoint)
	if err != nil {
		log.Println(fmt.Errorf("failed to connect to etcd at %s; using local key-value store instead: %w", *etcdEndpoint, err))
	} else {
		log.Printf("Successfully connected to etcd at %s\n", *etcdEndpoint)
	}
	// I don't need to `defer etcdCli.Close()` it's redundant in the main routine: when main() exits, everything is closed.

	// set up our global metrics struct, setting our start time
	x.Metrics.Start = time.Now()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 53})
	//  common err hierarchy: net.OpError → os.SyscallError → syscall.Errno
	switch {
	case err == nil:
		log.Println(`Successfully bound to all interfaces, port 53.`)
		wg.Add(1)
		readFrom(conn, &wg, x, *blocklistURL)
	case isErrorPermissionsError(err):
		log.Println("Try invoking me with `sudo` because I don't have permission to bind to port 53.")
		log.Fatal(err.Error())
	case isErrorAddressAlreadyInUse(err):
		log.Println(`I couldn't bind to "0.0.0.0:53" (INADDR_ANY, all interfaces), so I'll try to bind to each address individually.`)
		ipCIDRs := listLocalIPCIDRs()
		var boundIPsPorts, unboundIPs []string
		for _, ipCIDR := range ipCIDRs {
			ip, _, err := net.ParseCIDR(ipCIDR)
			if err != nil {
				log.Printf(`I couldn't parse the local interface "%s".`, ipCIDR)
				continue
			}
			conn, err = net.ListenUDP("udp", &net.UDPAddr{
				IP:   ip,
				Port: 53,
				Zone: "",
			})
			if err != nil {
				unboundIPs = append(unboundIPs, ip.String())
			} else {
				wg.Add(1)
				boundIPsPorts = append(boundIPsPorts, conn.LocalAddr().String())
				go readFrom(conn, &wg, x, *blocklistURL)
			}
		}
		if len(boundIPsPorts) > 0 {
			log.Printf(`I bound to the following: "%s"`, strings.Join(boundIPsPorts, `", "`))
		}
		if len(unboundIPs) > 0 {
			log.Printf(`I couldn't bind to the following IPs: "%s"`, strings.Join(unboundIPs, `", "`))
		}
	default:
		log.Fatal(err.Error())
	}
	wg.Wait()
}

func readFrom(conn *net.UDPConn, wg *sync.WaitGroup, x xip.Xip, blocklistURL string) {
	defer wg.Done()
	// We want to make sure that our DNS server isn't used in a DNS amplification attack.
	// The endpoint we're worried about is metrics.status.sslip.io, whose reply is
	// ~400 bytes with a query of ~100 bytes (4x amplification). We accomplish this by
	// using channels with a quarter-second delay. Max throughput 1.2 kBytes/sec.
	//
	// We want to balance this delay against our desire to run tests quickly, so we buffer
	// the channel with enough room to accommodate our tests.
	//
	// We realize that, if we're listening on several network interfaces, we're throttling
	// _per interface_, not from a global standpoint, but we didn't want to clutter
	// main() more than necessary.
	//
	// We also want to have fun playing with channels
	dnsAmplificationAttackDelay := make(chan struct{}, xip.MetricsBufferSize)
	go func() {
		// fill up the channel's buffer so that our tests aren't slowed down (~85 tests)
		for i := 0; i < xip.MetricsBufferSize; i++ {
			dnsAmplificationAttackDelay <- struct{}{}
		}
		// now put on the brakes for users trying to leverage our server in a DNS amplification attack
		for {
			dnsAmplificationAttackDelay <- struct{}{}
			time.Sleep(250 * time.Millisecond)
		}
	}()
	go func() {
		for {
			blocklistStrings, blocklistCDIRs, err := readBlocklist(blocklistURL)
			if err != nil {
				log.Println(fmt.Errorf("couldn't get blocklist at %s: %w", blocklistURL, err))
			} else {
				log.Printf("Successfully downloaded blocklist from %s: %v, %v", blocklistURL, blocklistStrings, blocklistCDIRs)
				x.BlocklistStrings = blocklistStrings
				x.BlocklistCDIRS = blocklistCDIRs
				x.BlocklistUpdated = time.Now()
			}
			time.Sleep(1 * time.Hour)
		}
	}()
	x.DnsAmplificationAttackDelay = dnsAmplificationAttackDelay
	for {
		query := make([]byte, 512)
		_, addr, err := conn.ReadFromUDP(query)
		if err != nil {
			log.Println(err.Error())
			continue
		}
		go func() {
			response, logMessage, err := x.QueryResponse(query, addr.IP)
			if err != nil {
				log.Println(err.Error())
				return
			}
			_, err = conn.WriteToUDP(response, addr)
			log.Printf("%v.%d %s", addr.IP, addr.Port, logMessage)
		}()
	}
}

func listLocalIPCIDRs() []string {
	var ifaces []net.Interface
	var cidrStrings []string
	var err error
	if ifaces, err = net.Interfaces(); err != nil {
		panic(err)
	}
	for _, iface := range ifaces {
		var cidrs []net.Addr
		if cidrs, err = iface.Addrs(); err != nil {
			panic(err)
		}
		for _, cidr := range cidrs {
			cidrStrings = append(cidrStrings, cidr.String())
		}
	}
	return cidrStrings
}

// Thanks https://stackoverflow.com/a/52152912/2510873
func isErrorAddressAlreadyInUse(err error) bool {
	var eOsSyscall *os.SyscallError
	if !errors.As(err, &eOsSyscall) {
		return false
	}
	var errErrno syscall.Errno // doesn't need a "*" (ptr) because it's already a ptr (uintptr)
	if !errors.As(eOsSyscall, &errErrno) {
		return false
	}
	if errErrno == syscall.EADDRINUSE {
		return true
	}
	const WSAEADDRINUSE = 10048
	if runtime.GOOS == "windows" && errErrno == WSAEADDRINUSE {
		return true
	}
	return false
}

func isErrorPermissionsError(err error) bool {
	var eOsSyscall *os.SyscallError
	if errors.As(err, &eOsSyscall) {
		if os.IsPermission(eOsSyscall) {
			return true
		}
	}
	return false
}

// clientv3New attempts to connect to local etcd and retrieve a key to make
// sure the connection works. If for any reason it fails it returns nil +
// error
func clientv3New(etcdEndpoint string) (*clientv3.Client, error) {
	etcdEndpoints := []string{etcdEndpoint}
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   etcdEndpoints,
		DialTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	// Let's do a query to determine if etcd is really, truly there
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*500)
	defer cancel()
	_, err = etcdCli.Get(ctx, "some-silly-key, doesn't matter if it exists")
	if err != nil {
		return nil, err
	}
	return etcdCli, nil
}

// readBlocklist downloads the blocklist of domains & CIDRs that are forbidden
// because they're used for phishing (e.g. "raiffeisen")
func readBlocklist(blocklistURL string) (blocklistStrings []string, blocklistCIDRs []net.IPNet, err error) {
	resp, err := http.Get(blocklistURL)
	if err != nil {
		log.Println(fmt.Errorf(`failed to download blocklist "%s": %w`, blocklistURL, err))
	} else {
		defer resp.Body.Close()
		if resp.StatusCode > 299 {
			log.Printf(`failed to download blocklist "%s", HTTP status: "%d"`, blocklistURL, resp.StatusCode)
		} else {
			blocklistStrings, blocklistCIDRs, err = xip.ReadBlocklist(resp.Body)
			if err != nil {
				log.Println(fmt.Errorf(`failed to parse blocklist "%s": %w`, blocklistURL, err))
			}
		}
	}
	return blocklistStrings, blocklistCIDRs, err
}
