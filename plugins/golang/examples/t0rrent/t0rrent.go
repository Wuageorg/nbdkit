package main

import (
	"C"
	"unsafe"

	"runtime"
	"runtime/debug"

	"log"
	"fmt"
	"io"
	"sync"
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
	torrs   map[string]*TorrStorLinuxCache
	mu      sync.Mutex
}

func NewStorage(nbdmount string) *StorLinuxCache {
	stor := new(StorLinuxCache)
	stor.nbdmount = nbdmount
	stor.torrs = make(map[string]*TorrStorLinuxCache)
	return stor
}

func (s *StorLinuxCache) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (ts.TorrentImpl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	torr := NewTorrStor(s, info, infoHash)
	s.torrs[infoHash.AsString()] = torr
	return ts.TorrentImpl{Piece: torr.Piece, Close: torr.Close, Flush: torr.Flush}, nil
}

func (s *StorLinuxCache) CloseHash(hash metainfo.Hash) {
	if s.torrs == nil {
		return
	}
	h := hash.AsString()
	s.mu.Lock()
	defer s.mu.Unlock()
	if torr, ok := s.torrs[h]; ok {
		torr.Close()
		delete(s.torrs, h)
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
	if torr, ok := s.torrs[hash.AsString()]; ok {
		return torr
	}
	return nil
}

type TorrStorLinuxCache struct {
	// iface ts.TorrentImpl
	hash     metainfo.Hash
	pieceLength int64
	isClosed bool

	mu     sync.RWMutex
	memPieces []TorrPieceLinuxCache // Pieces that are being downloaded
}

func NewTorrStor(storage *StorLinuxCache, info *metainfo.Info, hash metainfo.Hash) *TorrStorLinuxCache {
	pcnt := info.NumPieces()
	return &TorrStorLinuxCache{
		pieceLength: info.PieceLength,
		hash: hash,
		memPieces: make([]TorrPieceLinuxCache, pcnt, pcnt),
	}
}

func (t *TorrStorLinuxCache) Piece(m metainfo.Piece) ts.PieceImpl {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := m.Index()
	size := m.Length()
	p := &t.memPieces[id]
	if p.size == 0 {
		p.id = id
		p.size = size
		p.buf = make([]byte, size, size)
	}
	return p
}

func (t *TorrStorLinuxCache) Close() error {
	t.isClosed = true
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

func (p *TorrPieceLinuxCache) ReadAt(b []byte, off int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// TODO find a way to know if read comes from nbdkit or another torrent client
	// if nbdkit, and the whole piece has been read and is complete, we can free the buffer and
	// resolve future readAt with linux cache
	return copy(b, p.buf[off:int(off) + len(b)]), nil
}

func (p *TorrPieceLinuxCache) WriteAt(b []byte, off int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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

func writeChunkError(err error) {
	log.Fatalf("writeChunkError %s\n", err)
	panic(err)
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
	tsize uint64
	reader torrent.Reader
	mu sync.Mutex
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

func (p *T0rrentPlugin) Unload() {
	if p.tManager != nil {
		p.tManager.Close()
	}
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
	p.t.SetOnWriteChunkError(writeChunkError)

	return nil
}

func (p *T0rrentPlugin) Open(readonly bool) (nbdkit.ConnectionInterface, error) {
	// p.t.PieceState()
	// func (t *Torrent) AddWebSeeds(urls []string, opts ...AddWebSeedsOpt)
	// func (t *Torrent) AddTrackers(announceList [][]string)
	// func (t *Torrent) AddPeers(pp []PeerInfo) (n int)
	// func (t *Torrent) CancelPieces(begin, end pieceIndex)
	// func (t *Torrent) DownloadPieces(begin, end pieceIndex)
	// func (t *Torrent) Length() int64
	// func (t *Torrent) NumPieces() pieceIndex
	// func (t *Torrent) Piece(i pieceIndex) *Piece
	// func (t *Torrent) SubscribePieceStateChanges() *pubsub.Subscription[PieceStateChange]

	<-p.t.GotInfo() // wait till with get torrent infos // TODO timeout and fail
	nbdkit.Debug(fmt.Sprint("InfoHash ", p.t.InfoHash()))
	nbdkit.Debug(fmt.Sprint("Name ", p.t.Info().BestName()))
	nbdkit.Debug(fmt.Sprint("Pieces Length ", p.t.Info().PieceLength))
	nbdkit.Debug(fmt.Sprint("Total Length ", p.t.Length()))
	nbdkit.Debug(fmt.Sprint("CreationDate ",p.t.Metainfo().CreationDate))
	nbdkit.Debug(fmt.Sprint("CreatedBy ", p.t.Metainfo().CreatedBy))
	nbdkit.Debug(fmt.Sprint("Comment ", p.t.Metainfo().Comment))
	for _, f := range p.t.Files() {
		nbdkit.Debug(fmt.Sprint("File ", f.Path(), " ", f.Length()))
	}
	return &T0rrentConnection{
		tsize: uint64(p.t.Length()),
		reader: p.t.NewReader(), // One reader per connections
	}, nil
}

func (c *T0rrentConnection) GetSize() (uint64, error) {
	return c.tsize, nil
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
	c.mu.Lock()
	defer c.mu.Unlock()

	bsz := len(buf)
	newPos, err := c.reader.Seek(int64(offset), io.SeekStart)
	copied := 0
	if err != nil || newPos != int64(offset) {
		if err == nil {
			err = nbdkit.PluginError{
				Errmsg: "Seek failed",
				Errno: 29, // ESPIPE
			}
		}
		return err
	}

	for copied < bsz {
		r, err := c.reader.Read(buf[copied:])
		if err != nil {
			s, _ := c.GetSize()
			if err == io.EOF && (offset + uint64(len(buf))) != s {
				return err
			}
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
