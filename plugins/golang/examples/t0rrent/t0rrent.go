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
// TODO benchmark with rusage and ram graph tui

// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾
// 💾💾 Storage
// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾

// Storage struct
type StorLinuxCache struct {
	ts.ClientImpl

	nbdmount string
	torrs   map[metainfo.Hash]*TorrStorLinuxCache
	mu      sync.Mutex
}

func NewStorage(nbdmount string) *StorLinuxCache {
	stor := new(StorLinuxCache)
	stor.nbdmount = nbdmount
	stor.torrs = make(map[metainfo.Hash]*TorrStorLinuxCache)
	return stor
}

func (s *StorLinuxCache) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (ts.TorrentImpl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	torr := NewTorrStor(s, info, infoHash)
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
	isClosed bool

	mu     sync.RWMutex
	memPieces map[int]*TorrPieceLinuxCache // Pieces that are being downloaded
	maybeInLinuxCache map[int]bool // Store the id of pieces that maybe in the linux cache
}

func NewTorrStor(storage *StorLinuxCache, info *metainfo.Info, hash metainfo.Hash) *TorrStorLinuxCache {
	return &TorrStorLinuxCache{
		pieceLength: info.PieceLength,
		hash: hash,
		memPieces: make(map[int]*TorrPieceLinuxCache),
		maybeInLinuxCache: make(map[int]bool),
	}
}

func (t *TorrStorLinuxCache) Piece(m metainfo.Piece) ts.PieceImpl {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := m.Index()
	if p, ok := t.memPieces[id]; ok {
		return p
	}
	if isIn, ok := t.maybeInLinuxCache[id]; ok && isIn {
		return nil // TODO
	}
	size := m.Length()
	p := &TorrPieceLinuxCache{
		id: id,
		size: size,
		buf: make([]byte, size, size),
	}
	log.Println("Piece(", id, m.Offset(), m.Length(), ")")
	t.memPieces[id] = p
	return p
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

type TorrPieceLinuxCache struct {
	id int
	size int64
	complete bool

	buf []byte
	mu     sync.RWMutex
}

func (p *TorrPieceLinuxCache) ReadAt(b []byte, off int64) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	log.Println("ReadAt(", p.id, ")", off, " ", len(b))
	return copy(b, p.buf[off:int(off) + len(b)]), nil
}

func (p *TorrPieceLinuxCache) WriteAt(b []byte, off int64) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// log.Println("WriteAt(", p.id, ")", off, " ", len(b))
	return copy(p.buf[off:], b), nil
}

func (p *TorrPieceLinuxCache) MarkComplete() error {
	p.complete = true
	return nil
}

func (p *TorrPieceLinuxCache) MarkNotComplete() error {
	p.complete = false
	return nil
}

func (p *TorrPieceLinuxCache) Completion() ts.Completion {
	return ts.Completion{
		Complete: p.complete,
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
	nbdmount string
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
	} else if key == "nbdmount" {
		p.nbdmount = value
	} else {
		return nbdkit.PluginError{Errmsg: "unknown parameter"}
	}
	return nil
}

func (p *T0rrentPlugin) ConfigComplete() error {
	if len(p.magnet) == 0 {
		return nbdkit.PluginError{Errmsg: "magnet parameter is required"}
	}
	if len(p.nbdmount) == 0 {
		return nbdkit.PluginError{Errmsg: "nbdmount parameter is required"}
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
	torrentCfg.DefaultStorage = NewStorage(p.nbdmount)
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
		reader: p.t.NewReader(), // One reader per connections
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

func (c *T0rrentConnection) PWrite(buf []byte, offset uint64,
	flags uint32) error {
	return nil
}

func (c *T0rrentConnection) PRead(buf []byte, offset uint64, flags uint32) error {
	bsz := len(buf)
	// log.Println("PRead(", offset, bsz, flags)

	newPos, err := c.reader.Seek(int64(offset), io.SeekStart)
	if err != nil || newPos != int64(offset) {
		if err == nil {
			err = errors.New("Seek failed")
		}
		return err
	}
	copied := 0
	for copied < bsz {
		if copied != 0 {
			log.Println("second read!", copied, len(buf))
		}
		r, err := c.reader.Read(buf[copied:])
		if err != nil {
			return err
		}
		copied += r
	}
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
