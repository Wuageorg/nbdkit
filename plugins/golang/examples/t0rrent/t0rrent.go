package main

import (
	"C"
	"unsafe"

	// "log"

	"libguestfs.org/nbdkit"

	"github.com/anacrolix/torrent"
	// "github.com/anacrolix/torrent/storage"
)

// Future Needs
// 14 local peers discovery missing
// 54 idonthave missing
// 52 bittorentv2 missing
// 19 webseeds
// 29 uTorrent transport protocol
// 27 private torrents
// 7 ipv6
// 5 dht
// torrent modifications
// storage backend

var pluginName = "t0rrent"

// The plugin global struct.
type T0rrentPlugin struct {
	nbdkit.Plugin
	magnet string
	client *torrent.Client
}

// The per-nbd-client struct.
type T0rrentConnection struct {
	nbdkit.Connection
	plugin 	*T0rrentPlugin
}

func (p *T0rrentPlugin) Config(key string, value string) error {
	if key == "magnet" {
		p.magnet = value
		return nil
	} else {
		return nbdkit.PluginError{Errmsg: "unknown parameter"}
	}
}

func (p *T0rrentPlugin) ConfigComplete() error {
	if len(p.magnet) == 0 {
		return nbdkit.PluginError{Errmsg: "magnet parameter is required"}
	}
	return nil
}

func (p *T0rrentPlugin) GetReady() error {
	torrentCfg:= torrent.NewDefaultClientConfig()
	torrentCfg.Seed = true
	torrentCfg.AcceptPeerConnections = true
	torrentCfg.DisableIPv6 = false
	torrentCfg.DisableIPv4 = false
	torrentCfg.DisableTCP = true
	torrentCfg.DisableUTP = false
	torrentCfg.Debug = true
	// torrentCfg.DefaultStorage = st

	var err error
	p.client, err = torrent.NewClient(torrentCfg)
	if err != nil {
		return err
	}
	p.client.AddMagnet(p.magnet);
	return nil
}

func (p *T0rrentPlugin) Open(readonly bool) (nbdkit.ConnectionInterface, error) {
	return &T0rrentConnection{
		plugin: p,
	}, nil
}

func (c *T0rrentConnection) GetSize() (uint64, error) {
	return 100, nil
}

// Clients are allowed to make multiple connections safely.
func (c *T0rrentConnection) CanMultiConn() (bool, error) {
	return true, nil
}

func (c *T0rrentConnection) PRead(buf []byte, offset uint64,
	flags uint32) error {
	// copy(buf, disk[offset:int(offset)+len(buf)])
	return nil
}




func (c *T0rrentConnection) CanWrite() (bool, error) {
	return false, nil
}

func (c *T0rrentConnection) PWrite(buf []byte, offset uint64,
	flags uint32) error {
	return nil
}

//----------------------------------------------------------------------
//
// The boilerplate below this line is required by all golang plugins,
// as well as importing "C" and "unsafe" modules at the top of the
// file.

//export plugin_init
func plugin_init() unsafe.Pointer {
	// If your plugin needs to do any initialization, you can
	// either put it here or implement a Load() method.
	// ...

	// Then you must call the following function.
	return nbdkit.PluginInitialize(pluginName, &T0rrentPlugin{})
}

// This is never(?) called, but must exist.
func main() {}
