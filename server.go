package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"time"

	"github.com/coredhcp/coredhcp/plugins/allocators"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/pin/tftp"
	web "github.com/poseidon/matchbox/matchbox/http"
	matchboxServer "github.com/poseidon/matchbox/matchbox/server"
	"github.com/poseidon/matchbox/matchbox/storage"
)

type DHCPRecord struct {
	IP      net.IP
	expires time.Time
}

// A Server boots machines using a Booter.
type Server struct {
	ServerRoot string

	IP   net.IP
	GWIP net.IP

	Net *net.IPNet

	ForwardDns []string

	Intf string

	Controlplane string

	ProxyDHCP bool

	DHCPLock      sync.Mutex
	DHCPRecords   map[string]*DHCPRecord
	DHCPAllocator allocators.Allocator

	DNSRWLock    sync.RWMutex
	DNSRecordsv4 map[string][]net.IP
	DNSRecordsv6 map[string][]net.IP
	DNSRRecords  map[string][]string

	// These ports can technically be set for testing, but the
	// protocols burned in firmware on the client side hardcode these,
	// so if you change them in production, nothing will work.
	DHCPPort int
	TFTPPort int
	PXEPort  int
	HTTPPort int
	DNSPort  int

	errs chan error
	// pointers to servers needed for shutdowns
	serverHttp *http.Server
	serverTFTP *tftp.Server
	serverDHCP *server4.Server
	serverDNS  *dnsserver.Server

	// the PXE does not have server object just a socket that we close when Serve() exits
	closeServers chan struct{}
}

// Serve listens for machines attempting to boot, and uses Booter to
// help them.
func (s *Server) Serve() error {
	if s.DHCPPort == 0 {
		s.DHCPPort = portDHCP
	}
	if s.TFTPPort == 0 {
		s.TFTPPort = portTFTP
	}
	if s.PXEPort == 0 {
		s.PXEPort = portPXE
	}
	if s.HTTPPort == 0 {
		s.HTTPPort = portHTTP
	}
	if s.DNSPort == 0 {
		s.DNSPort = portDNS
	}

	if len(s.ForwardDns) == 0 {
		s.ForwardDns = []string{forwardDns}
	}

	cTftp, err := net.ListenPacket("udp", fmt.Sprintf("%s:%d", s.IP, s.TFTPPort))
	if err != nil {
		return err
	}
	defer cTftp.Close()
	cPxe, err := net.ListenPacket("udp4", fmt.Sprintf("%s:%d", s.IP, s.PXEPort))
	if err != nil {
		return err
	}
	defer cPxe.Close()
	cHttp, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.IP, s.HTTPPort))
	if err != nil {
		return err
	}
	defer cHttp.Close()
	cDns, err := net.ListenPacket("udp", fmt.Sprintf("%s:%d", s.IP, s.DNSPort))
	if err != nil {
		return err
	}
	defer cDns.Close()

	log.Info("Starting servers")

	go func() { s.errs <- s.servePXE(cPxe) }()
	go func() { s.errs <- s.serveTFTP(cTftp) }()
	go func() { s.errs <- s.startMatchbox(cHttp) }()
	go func() { s.errs <- s.startDhcp() }()
	go func() { s.errs <- s.serveDNS(cDns) }()

	// Wait for either a fatal error, or Shutdown().
	err = <-s.errs
	return err
}

func NewServer(serverRoot, interfaceName, controlplane string) *Server {
	return &Server{
		ServerRoot:   serverRoot,
		Intf:         interfaceName,
		Controlplane: controlplane,
		DHCPRecords:  make(map[string]*DHCPRecord),
		DNSRecordsv4: make(map[string][]net.IP),
		DNSRecordsv6: make(map[string][]net.IP),
		DNSRRecords:  make(map[string][]string),
		closeServers: make(chan struct{}),
		// 6 buffer slots, one for each goroutine, plus one for
		// Shutdown(). We only ever pull the first error out, but shutdown
		// will likely generate some spurious errors from the other
		// goroutines, and we want them to be able to dump them without
		// blocking.
		errs: make(chan error, 6),
	}
}

func (s *Server) startMatchbox(l net.Listener) error {
	store := storage.NewFileStore(&storage.Config{
		Root: s.ServerRoot,
	})

	server := matchboxServer.NewServer(&matchboxServer.Config{
		Store: store,
	})

	config := &web.Config{
		Core:       server,
		Logger:     log,
		AssetsPath: filepath.Join(s.ServerRoot, "assets"),
	}

	s.serverHttp = &http.Server{
		Handler: s.ipxeWrapperMenuHandler(web.NewServer(config).HTTPHandler()),
	}

	if err := s.serverHttp.Serve(l); err != nil {
		return fmt.Errorf("Matchbox server shut down: %s", err)
	}

	return nil
}

// Shutdown causes Serve() to exit, cleaning up behind itself.
func (s *Server) Shutdown() {
	//if s.closed {
	//	return
	//}
	//
	//s.closed = true
	select {
	case s.errs <- nil:
	default:
	}
}

func getPrivateAddress() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr).IP

	return localAddr, nil
}

func getInterface(addr net.IP) (*net.Interface, net.IPMask, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}

	for _, iface := range ifaces {
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			return nil, nil, err
		}

		for _, ifaceAddr := range ifaceAddrs {
			switch v := ifaceAddr.(type) {
			case *net.IPAddr:
				if v.IP.Equal(addr) {
					return &iface, v.IP.DefaultMask(), nil
				}

			case *net.IPNet:
				if v.IP.Equal(addr) {
					return &iface, v.Mask, nil
				}
			}
		}
	}

	return nil, nil, fmt.Errorf("Could not find interface for address")
}

func getValidInterfaces() ([]net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var validInterfaces []net.Interface

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		validInterfaces = append(validInterfaces, iface)
	}

	if len(validInterfaces) == 0 {
		return nil, fmt.Errorf("Could not find any non-loopback interfaces that are active")
	}

	return validInterfaces, nil
}

// ipxeWrapperMenuHandler
func (s *Server) ipxeWrapperMenuHandler(primaryHandler http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "ipxe" && req.URL.Path != "/ipxe" {
			primaryHandler.ServeHTTP(w, req)
			return
		}

		rr := httptest.NewRecorder()
		primaryHandler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			if err := req.ParseForm(); err != nil {
				log.Errorf("Error ParseForm: %v", err)
				return
			}
			machineType := req.Form.Get("type")
			remoteIp := net.ParseIP(req.Form.Get("ip"))
			log.Infof("Selecting %s for %s", machineType, remoteIp)

			if machineType == "init" || machineType == "controlplane" {
				s.registerDNSEntry(s.Controlplane, remoteIp)
			}

			for key, values := range rr.HeaderMap {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}

			w.WriteHeader(rr.Code)

			if _, err := w.Write(rr.Body.Bytes()); err != nil {
				log.Errorf("Error wrtiting body bytes %v", err)
			}
		} else {
			log.Info("Serving menu")

			if err := ipxeMenuTemplate.Execute(w, s); err != nil {
				log.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	}

	return http.HandlerFunc(fn)
}
