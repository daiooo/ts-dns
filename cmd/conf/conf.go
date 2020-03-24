package conf

import (
	"fmt"
	"github.com/BurntSushi/toml"
	log "github.com/Sirupsen/logrus"
	"github.com/janeczku/go-ipset/ipset"
	"github.com/wolf-joe/ts-dns/cache"
	"github.com/wolf-joe/ts-dns/hosts"
	"github.com/wolf-joe/ts-dns/inbound"
	"github.com/wolf-joe/ts-dns/matcher"
	"github.com/wolf-joe/ts-dns/outbound"
	"golang.org/x/net/proxy"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Group 配置文件中每个groups section对应的结构
type Group struct {
	Socks5     string
	IPSet      string
	IPSetTTL   int `toml:"ipset_ttl"`
	DNS        []string
	DoT        []string
	DoH        []string
	Concurrent bool
	Rules      []string
}

// GenIPSet 读取ipset配置并打包成IPSet对象
func (conf *Group) GenIPSet() (ipSet *ipset.IPSet, err error) {
	if conf.IPSet != "" {
		param := &ipset.Params{Timeout: conf.IPSetTTL}
		ipSet, err = ipset.New(conf.IPSet, "hash:ip", param)
		if err != nil {
			return nil, err
		}
		return ipSet, nil
	}
	return nil, nil
}

// GenCallers 读取dns配置并打包成Caller对象
func (conf *Group) GenCallers() (callers []outbound.Caller) {
	// 读取socks5代理地址
	var dialer proxy.Dialer
	if conf.Socks5 != "" {
		dialer, _ = proxy.SOCKS5("tcp", conf.Socks5, nil, proxy.Direct)
	}
	// 为每个出站dns服务器创建对应Caller对象
	for _, addr := range conf.DNS { // TCP/UDP服务器
		network := "udp"
		if strings.HasSuffix(addr, "/tcp") {
			addr, network = addr[:len(addr)-4], "tcp"
		}
		if addr != "" {
			if !strings.Contains(addr, ":") {
				addr += ":53"
			}
			callers = append(callers, outbound.NewDNSCaller(addr, network, dialer))
		}
	}
	for _, addr := range conf.DoT { // dns over tls服务器，格式为ip:port@serverName
		var serverName string
		if arr := strings.Split(addr, "@"); len(arr) != 2 {
			continue
		} else {
			addr, serverName = arr[0], arr[1]
		}
		if addr != "" && serverName != "" {
			if !strings.Contains(addr, ":") {
				addr += ":853"
			}
			callers = append(callers, outbound.NewDoTCaller(addr, serverName, dialer))
		}
	}
	dohReg := regexp.MustCompile(`^https://.+/dns-query$`)
	for _, addr := range conf.DoH { // dns over https服务器，格式为https://domain/dns-query
		if dohReg.MatchString(addr) {
			callers = append(callers, outbound.NewDoHCaller(addr, dialer))
		}
	}
	return
}

// Cache 配置文件中cache section对应的结构
type Cache struct {
	Size   int
	MinTTL int `toml:"min_ttl"`
	MaxTTL int `toml:"max_ttl"`
}

// Conf 配置文件总体结构
type Conf struct {
	Listen     string
	GFWList    string
	CNIP       string
	HostsFiles []string `toml:"hosts_files"`
	Hosts      map[string]string
	Cache      *Cache
	Groups     map[string]*Group
}

// SetDefault 为部分字段默认配置
func (conf *Conf) SetDefault() {
	if conf.Listen == "" {
		conf.Listen = ":53"
	}
	if conf.GFWList == "" {
		conf.GFWList = "gfwlist.txt"
	}
	if conf.CNIP == "" {
		conf.CNIP = "cnip.txt"
	}
}

// GenCache 根据cache section里的配置生成cache实例
func (conf *Conf) GenCache() *cache.DNSCache {
	if conf.Cache.Size == 0 {
		conf.Cache.Size = 4096
	}
	if conf.Cache.MinTTL == 0 {
		conf.Cache.MinTTL = 60
	}
	if conf.Cache.MaxTTL == 0 {
		conf.Cache.MaxTTL = 86400
	}
	minTTL := time.Duration(conf.Cache.MinTTL) * time.Second
	maxTTL := time.Duration(conf.Cache.MaxTTL) * time.Second
	return cache.NewDNSCache(conf.Cache.Size, minTTL, maxTTL)
}

// GenHostsReader 读取hosts section里的hosts记录、hosts_files里的hosts文件路径，生成hosts实例列表
func (conf *Conf) GenHostsReader() (readers []hosts.Reader) {
	// 读取Hosts列表
	var lines []string
	for hostname, ip := range conf.Hosts {
		lines = append(lines, ip+" "+hostname)
	}
	if len(lines) > 0 {
		text := strings.Join(lines, "\n")
		readers = append(readers, hosts.NewReaderByText(text))
	}
	// 读取Hosts文件列表。reloadTick为0代表不自动重载hosts文件
	for _, filename := range conf.HostsFiles {
		if reader, err := hosts.NewReaderByFile(filename, 0); err != nil {
			log.WithField("file", filename).Warnf("read hosts error: %v", err)
		} else {
			readers = append(readers, reader)
		}
	}
	return
}

// NewHandler 从toml文件里读取ts-dns的配置并打包为Handler。如err不为空，则在返回前会输出相应错误信息
func NewHandler(filename string) (handler *inbound.Handler, err error) {
	var config Conf
	if _, err = toml.DecodeFile(filename, &config); err != nil {
		log.WithField("file", filename).Errorf("read config error: %v", err)
		return nil, err
	}
	config.SetDefault()
	// 初始化handler
	handler = &inbound.Handler{Mux: new(sync.RWMutex), Groups: map[string]*inbound.Group{}}
	handler.Listen = config.Listen
	// 读取gfwlist
	if handler.GFWMatcher, err = matcher.NewABPByFile(config.GFWList, true); err != nil {
		log.WithField("file", config.GFWList).Errorf("read gfwlist error: %v", err)
		return nil, err
	}
	// 读取cnip
	if handler.CNIP, err = cache.NewRamSetByFile(config.CNIP); err != nil {
		log.WithField("file", config.CNIP).Errorf("read cnip error: %v", err)
		return nil, err
	}
	handler.HostsReaders = config.GenHostsReader()
	handler.Cache = config.GenCache()
	// 读取每个域名组的配置信息
	for name, group := range config.Groups {
		handlerGroup := &inbound.Group{Callers: group.GenCallers(), Concurrent: group.Concurrent}
		if handlerGroup.Concurrent {
			log.Warnln("enable dns concurrent in group " + name)
		}
		// 读取匹配规则
		handlerGroup.Matcher = matcher.NewABPByText(strings.Join(group.Rules, "\n"))
		// 读取IPSet配置
		if handlerGroup.IPSet, err = group.GenIPSet(); err != nil {
			log.Errorf("create ipset error: %v", err)
			return nil, err
		}
		handler.Groups[name] = handlerGroup
	}
	// 检测配置有效性
	if len(handler.Groups) <= 0 || len(handler.Groups["clean"].Callers) <= 0 || len(handler.Groups["dirty"].Callers) <= 0 {
		log.Errorf("dns of clean/dirty group cannot be empty")
		return nil, fmt.Errorf("dns of clean/dirty group cannot be empty")
	}
	return
}