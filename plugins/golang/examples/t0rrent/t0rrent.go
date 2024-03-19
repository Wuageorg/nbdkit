package main

import (
	"C"
	"unsafe"

	"runtime"
	"runtime/debug"

	"log"
	"io"
	"sync"
	"errors"
	"strings"

	"libguestfs.org/nbdkit"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	ts "github.com/anacrolix/torrent/storage"
)

// Needs
// 14 local peers discovery missing
// 54 idonthave missing
// 52 bittorentv2 missing (is on master)
// torrent modifications

// Have
// 19 webseeds
// 29 uTorrent transport protocol
// 27 private torrents
// 7 ipv6
// 5 dht


// FIXME how to not compile anacrolix webrtc stuff

// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾
// 💾💾 Storage
// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾

// Storage struct
type StorLinuxCache struct {
	ts.ClientImpl

	torrs   map[metainfo.Hash]*TorrStorLinuxCache
	mu      sync.Mutex
}

func NewStorage() *StorLinuxCache {
	stor := new(StorLinuxCache)
	stor.torrs = make(map[metainfo.Hash]*TorrStorLinuxCache)
	return stor
}

func (s *StorLinuxCache) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (ts.TorrentImpl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	torr := NewTorrent(s)
	torr.Init(info, infoHash)
	s.torrs[infoHash] = torr
	return ts.TorrentImpl{Piece: torr.Piece, Close: torr.Close, Flush: torr.Flush}, nil //	OE
}

func (s *StorLinuxCache) CloseHash(hash metainfo.Hash) {
	if s.torrs == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if torr, ok := s.torrs[hash]; ok {
		torr.Close()
		delete(s.torrs, hash)
	}

	runtime.GC()
	debug.FreeOSMemory()
}

func (s *StorLinuxCache) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, torr := range s.torrs {
		torr.Close()
	}
	return nil
}

func (s *StorLinuxCache) GetCache(hash metainfo.Hash) *TorrStorLinuxCache {
	s.mu.Lock()
	defer s.mu.Unlock()
	if torr, ok := s.torrs[hash]; ok {
		return torr
	}
	return nil
}

type TorrStorLinuxCache struct {
	// iface ts.TorrentImpl
	// stor *StorLinuxCache
	Capacity ts.TorrentCapacity

	hash     metainfo.Hash
	pieceLength int64
	pieceCount  int
	isClosed bool

	// seenPieces map[int]*Bool // TODO check kernel cache if the piece as already been seen
}

func NewTorrent(storage *StorLinuxCache) *TorrStorLinuxCache {
	return &TorrStorLinuxCache{}
}

func (t *TorrStorLinuxCache) Init(info *metainfo.Info, hash metainfo.Hash) {
	t.pieceLength = info.PieceLength
	t.pieceCount = info.NumPieces()
	t.hash = hash
}

func (t *TorrStorLinuxCache) Piece(m metainfo.Piece) ts.PieceImpl {
	return &PieceFake{}
	// idx := m.Index()
	// if idx >= t.pieceCount {
	// 	return &PieceFake{}
	// }
	// t.muPieces.Lock()
	// defer t.muPieces.Unlock()
	// if val, ok := t.inFlightPiecesMap[idx]; ok {
	// 	return t.inFlightPieces[val]
	// } else {
	// 	p := NewMemPiece(i, c.pieceLength)
	// 	append(t.inFlightPieces, p)
	// 	t.inFlightPiecesMap[idx] = t.inFlightPieces.len - 1
	// 	return p
	// }
}

// func (t *TorrStorLinuxCache) AdjustRA(readahead int64) {
// 	if t.Readers() > 0 {
// 		t.muReaders.Lock()
// 		for r := range t.readers {
// 			r.SetReadahead(readahead)
// 		}
// 		t.muReaders.Unlock()
// 	}
// }

func (t *TorrStorLinuxCache) Close() error {
	t.isClosed = true
	// t.stor.CloseHash(t.hash)
	return nil
}

func (t *TorrStorLinuxCache) Flush() error {
	return nil
}

type PieceFake struct{}

func (PieceFake) ReadAt(p []byte, off int64) (n int, err error) {
	err = errors.New("fake")
	return
}

func (PieceFake) WriteAt(p []byte, off int64) (n int, err error) {
	err = errors.New("fake")
	return
}

func (PieceFake) MarkComplete() error {
	return errors.New("fake")
}

func (PieceFake) MarkNotComplete() error {
	return errors.New("fake")
}

func (PieceFake) Completion() ts.Completion {
	return ts.Completion{
		Complete: false,
		Ok:       true,
		Err:      nil,
	}
}

// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️ Plugin
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️

// The plugin global struct.

type T0rrentPlugin struct {
	nbdkit.Plugin // iface
	magnet string
	tManager *torrent.Client
	t *torrent.Torrent
}

// The per-nbd-client struct.
type T0rrentConnection struct {
	nbdkit.Connection // iface
	plugin *T0rrentPlugin
	reader torrent.Reader
}

func (p *T0rrentPlugin) Config(key string, value string) error {
	if key == "magnet" {
		if strings.HasPrefix(value, "magnet:") {
			p.magnet = value
		} else {
			p.magnet = "magnet:?xt=urn:btih:" + value
		}
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
	torrentCfg.DefaultStorage = NewStorage()
	// torrentCfg.Debug = true

	var err error
	p.tManager, err = torrent.NewClient(torrentCfg)
	if err != nil {
		return err
	}

	p.t, err = p.tManager.AddMagnet(p.magnet);
	if err != nil {
		return err
	}
	p.t.SetDisplayName("Downloading torrent metadata");
	// func (t *Torrent) AddWebSeeds(urls []string, opts ...AddWebSeedsOpt)
	// func (t *Torrent) AddTrackers(announceList [][]string)
	// func (t *Torrent) AddPeers(pp []PeerInfo) (n int)
	// func (t *Torrent) CancelPieces(begin, end pieceIndex)
	// func (t *Torrent) DownloadPieces(begin, end pieceIndex)
	// func (t *Torrent) Length() int64
	// func (t *Torrent) NumPieces() pieceIndex
	// func (t *Torrent) Piece(i pieceIndex) *Piece
	// func (t *Torrent) SubscribePieceStateChanges() *pubsub.Subscription[PieceStateChange]
	<-p.t.GotInfo() // wait till with get torrent infos
	log.Println("InfoHash", p.t.InfoHash())
	log.Println("Name", p.t.Info().BestName())
	log.Println("Piece Length", p.t.Info().PieceLength)
	log.Println("CreationDate", p.t.Metainfo().CreationDate)
	log.Println("CreatedBy", p.t.Metainfo().CreatedBy)
	log.Println("Comment", p.t.Metainfo().Comment)
	for _, f := range p.t.Files() {
		log.Println("File", f.Path(), f.Length())
	}
	return nil
}

func (p *T0rrentPlugin) Open(readonly bool) (nbdkit.ConnectionInterface, error) {
	return &T0rrentConnection{
		plugin: p,
		reader: p.t.NewReader(),
	}, nil
}

func (c *T0rrentConnection) GetSize() (uint64, error) {
	return uint64(c.plugin.t.Length()), nil
}

// Clients are allowed to make multiple connections safely.
func (c *T0rrentConnection) CanMultiConn() (bool, error) {
	return true, nil
}

func (c *T0rrentConnection) CanWrite() (bool, error) {
	return false, nil
}

func (c *T0rrentConnection) PRead(buf []byte, offset uint64,
	flags uint32) error {
	log.Println("Read Offset: ", offset, " flags: ", flags)

	newPos, err := c.reader.Seek(int64(offset), io.SeekStart)
	if err != nil {
		return err
	}
	if newPos != int64(offset) {
		log.Println("SEEK FAILED OFFSET %d SEEKTO %d", offset, newPos)
	}
	_, err = c.reader.Read(buf)
	return nil
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
	return nbdkit.PluginInitialize("t0rrent", &T0rrentPlugin{})
}

// This is never(?) called, but must exist.
func main() {}
