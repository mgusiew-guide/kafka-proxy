package proxy

import (
	"crypto/tls"
	"fmt"
	"github.com/grepplabs/kafka-proxy/config"
	"github.com/sirupsen/logrus"
	"net"
	"sync"
)

type ListenFunc func(cfg config.ListenerConfig) (l net.Listener, err error)

type Listeners struct {
	// Source of new connections to Kafka broker.
	connSrc chan Conn
	// listen IP for dynamically start
	defaultListenerIP string
	// socket TCP options
	tcpConnOptions TCPConnOptions

	listenFunc ListenFunc

	disableDynamicListeners bool

	brokerToListenerAddresses map[string]string
	lock                      sync.RWMutex
}

func NewListeners(cfg *config.Config) (*Listeners, error) {

	defaultListenerIP := cfg.Proxy.DefaultListenerIP

	tcpConnOptions := TCPConnOptions{
		KeepAlive:       cfg.Proxy.ListenerKeepAlive,
		ReadBufferSize:  cfg.Proxy.ListenerReadBufferSize,
		WriteBufferSize: cfg.Proxy.ListenerWriteBufferSize,
	}

	var tlsConfig *tls.Config
	if cfg.Proxy.TLS.Enable {
		var err error
		tlsConfig, err = newTLSListenerConfig(cfg)
		if err != nil {
			return nil, err
		}
	}

	listenFunc := func(cfg config.ListenerConfig) (net.Listener, error) {
		if tlsConfig != nil {
			return tls.Listen("tcp", cfg.ListenerAddress, tlsConfig)
		}
		return net.Listen("tcp", cfg.ListenerAddress)
	}

	brokerToListenerAddresses := make(map[string]string)

	// add mapping without starting local listeners
	for _, v := range cfg.Proxy.ExternalServers {
		if la, ok := brokerToListenerAddresses[v.BrokerAddress]; ok {
			if la != v.ListenerAddress {
				return nil, fmt.Errorf("broker to listener address mapping %s configured twice: %s and %s", v.BrokerAddress, v.ListenerAddress, la)
			}
			continue
		}
		brokerToListenerAddresses[v.BrokerAddress] = v.ListenerAddress
	}

	return &Listeners{
		defaultListenerIP:         defaultListenerIP,
		connSrc:                   make(chan Conn, 1),
		brokerToListenerAddresses: brokerToListenerAddresses,
		tcpConnOptions:            tcpConnOptions,
		listenFunc:                listenFunc,
		disableDynamicListeners:   cfg.Proxy.DisableDynamicListeners,
	}, nil
}

func (p *Listeners) GetNetAddressMapping(brokerHost string, brokerPort int32) (listenerHost string, listenerPort int32, err error) {
	if brokerHost == "" || brokerPort <= 0 {
		return "", 0, fmt.Errorf("broker address '%s:%d' is invalid", brokerHost, brokerPort)
	}

	brokerAddress := net.JoinHostPort(brokerHost, fmt.Sprint(brokerPort))

	p.lock.RLock()
	listenerAddress := p.brokerToListenerAddresses[brokerAddress]
	p.lock.RUnlock()

	if listenerAddress != "" {
		return config.SplitHostPort(listenerAddress)
	}
	if !p.disableDynamicListeners {
		return p.ListenDynamicInstance(brokerAddress)
	}
	return "", 0, fmt.Errorf("net address mapping for %s:%d was not found", brokerHost, brokerPort)
}

func (p *Listeners) ListenDynamicInstance(brokerAddress string) (string, int32, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	// double check
	if v := p.brokerToListenerAddresses[brokerAddress]; v != "" {
		return v, 0, nil
	}

	defaultListenerAddress := net.JoinHostPort(p.defaultListenerIP, fmt.Sprint(0))

	cfg := config.ListenerConfig{ListenerAddress: defaultListenerAddress, BrokerAddress: brokerAddress}
	l, err := listenInstance(p.connSrc, cfg, p.tcpConnOptions, p.listenFunc)
	if err != nil {
		return "", 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	p.brokerToListenerAddresses[brokerAddress] = net.JoinHostPort(p.defaultListenerIP, fmt.Sprint(port))
	return p.defaultListenerIP, int32(port), nil
}

func (p *Listeners) ListenInstances(cfgs []config.ListenerConfig) (<-chan Conn, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// allows multiple local addresses to point to the remote
	for _, v := range cfgs {
		_, err := listenInstance(p.connSrc, v, p.tcpConnOptions, p.listenFunc)
		if err != nil {
			return nil, err
		}
		p.brokerToListenerAddresses[v.BrokerAddress] = v.ListenerAddress
	}
	return p.connSrc, nil
}

func listenInstance(dst chan<- Conn, cfg config.ListenerConfig, opts TCPConnOptions, listenFunc ListenFunc) (net.Listener, error) {
	l, err := listenFunc(cfg)
	if err != nil {
		return nil, err
	}
	go withRecover(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				logrus.Infof("Error in accept for %q on %v: %v", cfg, cfg.ListenerAddress, err)
				l.Close()
				return
			}
			if tcpConn, ok := c.(*net.TCPConn); ok {
				if err := opts.setTCPConnOptions(tcpConn); err != nil {
					logrus.Infof("WARNING: Error while setting TCP options for accepted connection %q on %v: %v", cfg, l.Addr().String(), err)
				}
			}
			logrus.Infof("New connection for %s", cfg.BrokerAddress)
			dst <- Conn{BrokerAddress: cfg.BrokerAddress, LocalConnection: c}
		}
	})

	logrus.Infof("Listening on %s (%s) for %s", cfg.ListenerAddress, l.Addr().String(), cfg.BrokerAddress)
	return l, nil
}