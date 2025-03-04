package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/medialogutils"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
	sipconfig "github.com/livekit/sip/pkg/config"
)

const NodeIDPrefix = "NSPX_"

type Config struct {
	*SIPProxyConfig                       `yaml:",inline"`
	*utils.ConfigObserver[SIPProxyConfig] `yaml:"-"`
}

type SIPProxyConfig struct {
	ServiceName string `yaml:"-"`
	NodeID      string // Do not provide, will be overwritten

	Logging        logger.Config        `yaml:"logging"`
	ListenIP       netip.Addr           `yaml:"listen_ip"`
	InternalIPMask netip.Prefix         `yaml:"internal_ip_mask"`
	InternalIP     netip.Addr           `yaml:"internal_ip"`
	ExternalIP     netip.Addr           `yaml:"external_ip"`
	SIPPort        int                  `yaml:"sip_port"`        // announced SIP signaling port
	SIPPortListen  int                  `yaml:"sip_port_listen"` // SIP signaling port to listen on
	SIPHostname    string               `yaml:"sip_hostname"`
	TLS            *sipconfig.TLSConfig `yaml:"tls"`

	Region string `yaml:"region"`

	Destination string `yaml:"sip_proxy_dest"`
}

func (c *SIPProxyConfig) Init() error {
	if c.NodeID == "" {
		c.NodeID = guid.New(NodeIDPrefix)
	}

	if c.SIPPort == 0 {
		c.SIPPort = sipconfig.DefaultSIPPort
	}
	if c.SIPPortListen == 0 {
		c.SIPPortListen = c.SIPPort
	}
	if tc := c.TLS; tc != nil {
		if tc.Port == 0 {
			tc.Port = sipconfig.DefaultSIPPortTLS
		}
		if tc.ListenPort == 0 {
			tc.ListenPort = tc.Port
		}
	}
	if !c.InternalIP.IsValid() {
		addr, err := getLocalIP(c.InternalIPMask)
		if err != nil {
			return err
		}
		c.InternalIP = addr
	}
	if !c.ExternalIP.IsValid() {
		addr, err := getPublicIP()
		if err != nil {
			return err
		}
		c.ExternalIP = addr
	}

	if err := c.InitLogger(); err != nil {
		return err
	}

	return nil
}

func (c *SIPProxyConfig) InitLogger(values ...interface{}) error {
	zl, err := logger.NewZapLogger(&c.Logging)
	if err != nil {
		return err
	}

	values = append(c.GetLoggerValues(), values...)
	l := zl.WithValues(values...)
	logger.SetLogger(l, c.ServiceName)
	lksdk.SetLogger(medialogutils.NewOverrideLogger(nil))

	return nil
}

func (c *SIPProxyConfig) GetLoggerValues() []interface{} {
	if c.NodeID == "" {
		return nil
	}
	return []interface{}{"nodeID", c.NodeID}
}

var _ utils.ConfigDefaulter[SIPProxyConfig] = (*configBuilder)(nil)

type configBuilder struct {
	*cli.Context
	NodeIDRand string
	loaded     atomic.Bool
}

func (b *configBuilder) New() (*SIPProxyConfig, error) {
	conf := &SIPProxyConfig{
		Logging: logger.Config{
			Level: "debug",
		},
		ServiceName: "cloud-sip-proxy",
		NodeID:      NodeIDPrefix + b.NodeIDRand,
	}

	if b.IsSet("config") {
		return conf, yaml.Unmarshal([]byte(b.String("config")), conf)
	}
	return conf, nil
}

func (b *configBuilder) InitDefaults(conf *SIPProxyConfig) error {
	conf.NodeID = NodeIDPrefix + strings.ReplaceAll(strings.ToUpper(conf.Region), "-", "_") + "_" + b.NodeIDRand
	if err := conf.Init(); err != nil {
		return err
	}

	if conf.SIPPort == 0 {
		conf.SIPPort = config.DefaultSIPPort
	}

	if b.loaded.Swap(true) {
		return nil
	}

	return nil
}

func NewConfig(c *cli.Context) (*Config, error) {
	obs, conf, err := utils.NewConfigObserver[SIPProxyConfig](c.String("config-file"), &configBuilder{
		NodeIDRand: guid.New(""),
		Context:    c,
	})
	if err != nil {
		return nil, err
	}
	return &Config{
		SIPProxyConfig: conf,
		ConfigObserver: obs,
	}, nil
}

func getLocalIP(mask netip.Prefix) (netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, nil
	}
	type Iface struct {
		Name string
		Addr netip.Addr
	}
	var candidates []Iface
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagRunning == 0 {
			continue
		}
		if ifc.Flags&(net.FlagPointToPoint|net.FlagLoopback) != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				ip, _ := netip.AddrFromSlice(ip4)
				if mask.IsValid() && !mask.Contains(ip) {
					continue
				}
				candidates = append(candidates, Iface{
					Name: ifc.Name, Addr: ip,
				})
				logger.Debugw("considering interface", "iface", ifc.Name, "ip", ip)
			}
		}
	}
	if len(candidates) == 0 {
		return netip.Addr{}, fmt.Errorf("no local IP found")
	}
	return candidates[0].Addr, nil
}

func getPublicIP() (netip.Addr, error) {
	req, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return netip.Addr{}, err
	}
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return netip.Addr{}, err
	}

	ip := struct {
		Query string
	}{}
	if err = json.Unmarshal(body, &ip); err != nil {
		return netip.Addr{}, err
	}

	if ip.Query == "" {
		return netip.Addr{}, fmt.Errorf("query entry was not populated")
	}

	return netip.ParseAddr(ip.Query)
}
