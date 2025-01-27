package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/anywherelan/awl/awldns"
	"github.com/anywherelan/awl/awlevent"
	"github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	"github.com/multiformats/go-multiaddr"
)

const (
	filesPerm = 0600
	dirsPerm  = 0700
)

// TODO: move to Config struct?
var logger = log.Logger("awl/config")

var DefaultBootstrapPeers []multiaddr.Multiaddr

func init() {
	for _, s := range []string{
		"/dnsaddr/42.194.178.234/p2p/12D3KooWMQiKT3uRwTMymUbZibyj4CJW7jHAQrkLUuB4AdLykyyb",
		"/ip4/42.194.178.234/tcp/6150/p2p/12D3KooWMQiKT3uRwTMymUbZibyj4CJW7jHAQrkLUuB4AdLykyyb",
		"/ip4/42.194.178.234/udp/6150/quic-v1/p2p/12D3KooWMQiKT3uRwTMymUbZibyj4CJW7jHAQrkLUuB4AdLykyyb",
	} {
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			logger.DPanicf("parse multiaddr: %v", err)
			continue
		}
		DefaultBootstrapPeers = append(DefaultBootstrapPeers, ma)
	}
}

func CalcAppDataDir() string {
	if envDir := os.Getenv(AppDataDirEnvKey); envDir != "" {
		err := os.MkdirAll(envDir, dirsPerm)
		if err != nil {
			logger.Warnf("could not create data directory from env: %v", err)
		}
		return envDir
	}

	var executableDir string
	ex, err := os.Executable()
	if err != nil {
		logger.Errorf("find executable path: %v", err)
	} else {
		executableDir = filepath.Dir(ex)
	}
	if executableDir != "" {
		configPath := filepath.Join(executableDir, AppConfigFilename)
		if _, err := os.Stat(configPath); err == nil {
			return executableDir
		}
	}

	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		logger.Warnf("could not get user config directory: %v", err)
		return ""
	}
	userDataDir := filepath.Join(userConfigDir, AppDataDirectory)
	err = os.MkdirAll(userDataDir, dirsPerm)
	if err != nil {
		logger.Warnf("could not create data directory in user dir: %v", err)
		return ""
	}

	return userDataDir
}

func NewConfig(bus awlevent.Bus) *Config {
	conf := &Config{}
	setDefaults(conf, bus)
	return conf
}

func LoadConfig(bus awlevent.Bus) (*Config, error) {
	dataDir := CalcAppDataDir()
	configPath := filepath.Join(dataDir, AppConfigFilename)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	// TODO: config migration
	conf := new(Config)
	err = json.Unmarshal(data, conf)
	if err != nil {
		return nil, err
	}
	conf.dataDir = dataDir
	setDefaults(conf, bus)
	return conf, nil
}

func ImportConfig(data []byte, directory string) error {
	conf := new(Config)
	err := json.Unmarshal(data, conf)
	if err != nil {
		return fmt.Errorf("invalid format: %v", err)
	}

	path := filepath.Join(directory, AppConfigFilename)
	err = os.WriteFile(path, data, filesPerm)
	if err != nil {
		return fmt.Errorf("save file: %v", err)
	}

	logger.Infof("Imported new config to %s", path)
	return nil
}

func setDefaults(conf *Config, bus awlevent.Bus) {
	// P2pNode
	if conf.P2pNode.ListenAddresses == nil {
		conf.P2pNode.ListenAddresses = make([]string, 0)
	}
	if conf.P2pNode.BootstrapPeers == nil {
		conf.P2pNode.BootstrapPeers = make([]string, 0)
	}
	if conf.P2pNode.ReconnectionIntervalSec == 0 {
		conf.P2pNode.ReconnectionIntervalSec = 10
	}

	// Other
	if conf.LoggerLevel == "" {
		conf.LoggerLevel = "info"
	}
	if conf.HttpListenAddress == "" {
		conf.HttpListenAddress = "127.0.0.1:" + strconv.Itoa(DefaultHTTPPort)
	}
	conf.Version = Version

	if conf.VPNConfig.IPNet == "" {
		conf.VPNConfig.IPNet = defaultNetworkSubnet
	}
	if ip, _ := conf.VPNLocalIPMask(); ip == nil {
		conf.VPNConfig.IPNet = defaultNetworkSubnet
	}
	if conf.VPNConfig.InterfaceName == "" {
		conf.VPNConfig.InterfaceName = defaultInterfaceName
	}

	uniqAliases := make(map[string]struct{}, len(conf.KnownPeers))
	if conf.KnownPeers == nil {
		conf.KnownPeers = make(map[string]KnownPeer)
	}
	for peerID := range conf.KnownPeers {
		peer := conf.KnownPeers[peerID]
		newAlias := conf.genUniqPeerAlias(peer.Name, peer.Alias, uniqAliases)
		if newAlias != peer.Alias {
			logger.Warnf("incorrect config: peer (id: %s) alias %s is not unique, updated automaticaly to %s", peerID, peer.Alias, newAlias)
			peer.Alias = newAlias
		}
		if peer.IPAddr == "" {
			peer.IPAddr = conf.GenerateNextIpAddr()
		}
		if peer.DomainName == "" {
			peer.DomainName = awldns.TrimDomainName(peer.DisplayName())
		}
		conf.KnownPeers[peerID] = peer
	}

	if conf.BlockedPeers == nil {
		conf.BlockedPeers = make(map[string]BlockedPeer)
	}

	if conf.dataDir == "" {
		conf.dataDir = CalcAppDataDir()
	}

	// Create dirs
	// TODO: currently PeerstoreDataDir is not used
	// peerstoreDir := filepath.Join(conf.dataDir, DhtPeerstoreDataDirectory)
	// err := os.MkdirAll(peerstoreDir, dirsPerm)
	// if err != nil {
	//	logger.Warnf("could not create peerstore directory: %v", err)
	// }

	emitter, err := bus.Emitter(new(awlevent.KnownPeerChanged), eventbus.Stateful)
	if err != nil {
		panic(err)
	}
	conf.emitter = emitter

	if u := conf.Update.UpdateServerURL; u == "" || u == "http://example/example.json" {
		conf.Update.UpdateServerURL = "https://build.anywherelan.com/repository/releases.json"
	} else {
		if _, err := url.Parse(conf.Update.UpdateServerURL); err != nil {
			logger.Warnf("incorrect update server url. err:%v", err)
		}
	}
	if !conf.Update.TrayAutoCheckEnabled && conf.Update.TrayAutoCheckInterval == "" {
		conf.Update.TrayAutoCheckEnabled = true
	}
	if i := conf.Update.TrayAutoCheckInterval; i == "" || i == "24h" {
		conf.Update.TrayAutoCheckInterval = "8h"
	}
}
