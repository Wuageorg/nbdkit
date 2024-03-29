// Package main defines an NBD (Network Block Device) server plugin that serves data from a torrent file.
package main

import (
	"C"
	"unsafe"

	"runtime"
	"runtime/debug"

	"fmt"
	"io"
	"os"
	// "errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"syscall"

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

const TIMEOUT = 10

// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾
// 💾💾 Storage
// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾

// StorLinuxCache represents an in-memory storage implementation for torrents.
type StorLinuxCache struct {
	ts.ClientImpl

	device string
	torrs  map[string]*TorrStorLinuxCache
	mu     sync.Mutex
}

// NewStorage creates a new StorLinuxCache instance.
func NewStorage(device string) *StorLinuxCache {
	stor := new(StorLinuxCache)
	stor.device = device // eg: "/dev/nbd0"
	stor.torrs = make(map[string]*TorrStorLinuxCache)
	return stor
}

// OpenTorrent opens a torrent for reading.
func (s *StorLinuxCache) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (ts.TorrentImpl, error) {
	h := infoHash.AsString()
	s.mu.Lock()
	defer s.mu.Unlock()
	torr := NewTorrStor(s, info, infoHash, s.device)
	s.torrs[h] = torr
	return ts.TorrentImpl{Piece: torr.Piece, Close: torr.Close, Flush: torr.Flush}, nil
}

// CloseTorrent closes a torrent.
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

// Close closes the storage and releases associated resources.
func (s *StorLinuxCache) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, torr := range s.torrs {
		torr.Close()
	}
	return nil
}

// GetTorrent retrieves a torrent by its hash.
func (s *StorLinuxCache) GetTorrent(hash metainfo.Hash) *TorrStorLinuxCache {
	s.mu.Lock()
	defer s.mu.Unlock()
	if torr, ok := s.torrs[hash.AsString()]; ok {
		return torr
	}
	return nil
}

// TorrStorLinuxCache represents a torrent data pieces.
type TorrStorLinuxCache struct {
	// iface ts.TorrentImpl
	hash        metainfo.Hash
	pieceLength int64
	isClosed    bool
	device      string

	mu        sync.RWMutex
	memPieces []TorrPieceLinuxCache // Pieces that are being downloaded
}

// // NewTorrStor creates a new TorrStorLinuxCache instance.
func NewTorrStor(storage *StorLinuxCache, info *metainfo.Info, hash metainfo.Hash, device string) *TorrStorLinuxCache {
	pcnt := info.NumPieces()
	return &TorrStorLinuxCache{
		pieceLength: info.PieceLength,
		hash:        hash,
		memPieces:   make([]TorrPieceLinuxCache, pcnt, pcnt),
		device: device,
	}
}

// Piece returns a piece of the torrent.
func (t *TorrStorLinuxCache) Piece(m metainfo.Piece) ts.PieceImpl {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := m.Index()
	p := &t.memPieces[id]
	if p.size == 0 {
		size := m.Length()
		p.id = id
		p.offset = m.Offset()
		p.size = size
		p.buf = make([]byte, size, size)
		p.device = &t.device
	}
	return p
}

// Close closes the torrent.
func (t *TorrStorLinuxCache) Close() error {
	t.isClosed = true
	return nil
}

// Flush flushes any pending changes to the torrent.
func (t *TorrStorLinuxCache) Flush() error {
	return nil
}


type slot struct {
	start, stop int64
}

type slots []slot

func (ss slots) merge(lo int64, hi int64) slots {
	min := func(a, b int64) int64 {
		if a < b {
			return a
		}
		return b
	}

	max := func(a, b int64) int64 {
		if a > b {
			return a
		}
		return b
	}

	xs := slots{{lo, hi}}
	if len(ss) == 0 {
		return xs
	}

	merged := make(slots, 0)

	is, ix := 0, 0
	for is < len(ss) && ix < len(xs) {
		vs, vx := ss[is], xs[ix]
		overlap12 := (vx.start >= vs.start) && (vx.start <= vs.stop)
		overlap21 := (vs.start >= vx.start) && (vs.start <= vx.stop)

		if overlap12 || overlap21 {
			merged := slot{
				start: min(vs.start, vx.start),
				stop:  max(vs.stop, vx.stop),
			}
			if vs.stop < vx.stop {
				xs[ix] = merged
				is++
			} else {
				ss[is] = merged
				ix++
			}
			continue
		}

		if vs.stop < vx.stop {
			merged = append(merged, vs)
			is++
		} else {
			merged = append(merged, vx)
			ix++
		}
	}

	merged = append(merged, ss[is:]...)
	merged = append(merged, xs[ix:]...)

	return merged
}

// TorrPieceLinuxCache represents a piece of a torrent.
type TorrPieceLinuxCache struct {
	id       int
	offset   int64
	size     int64
	device   *string
	buf []byte
	readslots slots
	mu  sync.RWMutex
	// piece state
	// 0 - incomplete
	// 1 - complete in memory
	// 2 - only in linux cache
	// 3 - Read loopback, cache miss
	state    atomic.Uint32
}

// ReadAt reads data from a piece at the specified offset.
func (p *TorrPieceLinuxCache) ReadAt(b []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(b))

	// if p.state.Load() > 2 {
	// 	log.Println("--------------------------- ReadAt Piece ", p.id, p.state.Load(), off, len(b))
	// }

	beforeState := p.state.Load()
	if beforeState > 2 { // before locking the piece, we check the read loopback situation
		// cache miss
		log.Println("!!! READ LOOPBACK", p.id)
		p.state.Store(0) // set incomplete so caller doesn't loop
		return 0, syscall.EIO
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state.Load()
	log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d\n", p.id, state, p.offset, lo, hi, beforeState)
	if beforeState > state { // state changed while waiting for lock
		log.Println("!!! State change while waiting lock -> redo", p.id)
		return 0, syscall.EIO
	}
	switch state {
	case 0: // use membuf
		return copy(b, p.buf[lo:hi:hi]), nil
	case 1: // not entirely in linux cache
		n := copy(b, p.buf[lo:hi:hi])
		if p.readslots == nil {
			p.readslots = slots{}
		}
		p.readslots = p.readslots.merge(lo, hi) // TODO only merge if the call comes from PRead
		// log.Println(p.readslots)
		if len(p.readslots) == 1 && p.readslots[0].start == 0 && p.readslots[0].stop == p.size { // completly read
			log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d\n", p.id, 2, p.offset, 0, p.size, 2)
			p.buf = nil // remove buf
			p.readslots = nil
			p.state.Store(2)
		}
		return n, nil
	case 2: // ReadAt from another peer
		// read in linux cache
		p.state.Store(3)
		var err error
		n := 0
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d\n", p.id, 3, p.offset, lo, hi, 3)
		f, err := os.Open(*p.device)
		if err != nil {
			goto cachemiss;
		}
		if n, err = f.ReadAt(b, p.offset + off); err != nil {
			goto cachemiss;
		}
		p.state.Store(2)
		return n, nil

		// err != nil -> cache miss
		cachemiss: // goto label
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d\n", p.id, 4, p.offset, lo, hi, 4)
		log.Println("@@@Device cache read fail, marking incomplete", p.id)
		p.buf = make([]byte, p.size, p.size)
		p.readslots = nil
		p.state.Store(0)
		return n, err
	default:
		panic(state)
	}
}

// WriteAt writes data to a piece at the specified offset.
func (p *TorrPieceLinuxCache) WriteAt(b []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(b))
	p.mu.Lock()
	defer p.mu.Unlock()
	return copy(p.buf[lo:hi:hi], b), nil
}

// MarkComplete marks a piece as complete.
func (p *TorrPieceLinuxCache) MarkComplete() error {
	p.state.Store(1)
	return nil
}

// MarkNotComplete marks a piece as not complete.
func (p *TorrPieceLinuxCache) MarkNotComplete() error {
	p.state.Store(0)
	return nil
}

// Completion returns the completion status of a piece.
func (p *TorrPieceLinuxCache) Completion() ts.Completion {
	state := p.state.Load()
	return ts.Completion{
		Complete: state > 0,
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

// T0rrentPlugin represents the NBD server plugin.
type T0rrentPlugin struct {
	nbdkit.Plugin // iface
	magnet        string
	device        string
	tManager      *torrent.Client
	t             *torrent.Torrent
}

// Config processes the plugin configuration parameters.
func (p *T0rrentPlugin) Config(key string, value string) error {
	switch key {
	case "magnet":
		p.magnet = value
		if !strings.HasPrefix(value, "magnet:") {
			p.magnet = "magnet:?xt=urn:btih:" + value
		}
	case "device":
		p.device = value
	default:
		return nbdkit.PluginError{Errmsg: "unknown parameter " + key}
	}
	return nil
}

// ConfigComplete checks if the plugin configuration is complete.
func (p *T0rrentPlugin) ConfigComplete() error {
	switch {
	case len(p.magnet) == 0:
		return nbdkit.PluginError{Errmsg: "magnet parameter is required"}
	case len(p.device) == 0:
		return nbdkit.PluginError{Errmsg: "device parameter is required"}
	}
	return nil
}

// GetReady prepares the plugin for serving torrent data.
func (p *T0rrentPlugin) GetReady() error {
	conf := torrent.NewDefaultClientConfig()
	conf.Seed = true
	conf.AcceptPeerConnections = true
	conf.DisableIPv6 = false
	conf.DisableIPv4 = false
	conf.DisableTCP = true
	conf.DisableUTP = false
	conf.DefaultStorage = NewStorage(p.device)
	// conf.Debug = true

	var err error
	if p.tManager, err = torrent.NewClient(conf); err != nil {
		return err
	}
	if p.t, err = p.tManager.AddMagnet(p.magnet); err != nil {
		return err
	}
	p.t.SetDisplayName("Downloading torrent metadata")
	p.t.SetOnWriteChunkError(writeChunkError)

	return nil
}

// Open prepares the plugin for serving a client connection.
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

	select {
	case <-p.t.GotInfo():
		nbdkit.Debug(fmt.Sprint("Got torrent %s infos", p.t.InfoHash()))
	case <-time.After(TIMEOUT * time.Second):
		return nil, nbdkit.PluginError{Errmsg: fmt.Sprint("Timeout, did not got torrent %s infos in %d seconds", p.t.InfoHash(), TIMEOUT)}
	}
	<-p.t.GotInfo() // wait till with get torrent infos // TODO timeout and fail
	nbdkit.Debug(fmt.Sprint("InfoHash ", p.t.InfoHash()))
	nbdkit.Debug(fmt.Sprint("Name ", p.t.Info().BestName()))
	nbdkit.Debug(fmt.Sprint("Pieces Length ", p.t.Info().PieceLength))
	nbdkit.Debug(fmt.Sprint("Total Length ", p.t.Length()))
	nbdkit.Debug(fmt.Sprint("CreationDate ", p.t.Metainfo().CreationDate))
	nbdkit.Debug(fmt.Sprint("CreatedBy ", p.t.Metainfo().CreatedBy))
	nbdkit.Debug(fmt.Sprint("Comment ", p.t.Metainfo().Comment))
	for _, f := range p.t.Files() {
		nbdkit.Debug(fmt.Sprint("File ", f.Path(), " ", f.Length()))
	}
	return &T0rrentConnection{
		tsize:  uint64(p.t.Length()),
		readerfun: p.t.NewReader,
	}, nil
}

// Unload releases resources used by the plugin.
func (p *T0rrentPlugin) Unload() {
	if p.tManager != nil {
		p.tManager.Close()
	}
}

// T0rrentConnection represents a client connection for serving torrent data.
type T0rrentConnection struct {
	nbdkit.Connection // iface
	tsize             uint64
	readerfun         func() torrent.Reader
}

// GetSize retrieves the size of the torrent data.
func (c *T0rrentConnection) GetSize() (uint64, error) {
	return c.tsize, nil
}

// Clients are allowed to make multiple connections safely.
func (c *T0rrentConnection) CanMultiConn() (bool, error) {
	return true, nil
}

// Clients are NOT allowed to write.
func (c *T0rrentConnection) CanWrite() (bool, error) {
	return false, nil
}

// PWrite is a noop
func (c *T0rrentConnection) PWrite(buf []byte, offset uint64,
	flags uint32) error {
	return nil
}

// Close termitates the client connection.
func (c *T0rrentConnection) Close() {
	c.tsize = 0
}

// PRead reads data from the torrent file at the specified offset into the provided buffer.
func (c *T0rrentConnection) PRead(buf []byte, offset uint64, flags uint32) error {
	reader := c.readerfun()
	defer reader.Close()

	pos, err := reader.Seek(int64(offset), io.SeekStart)
	// ensure the seek operation landed at the correct position
	switch {
	case err != nil:
		return err
	case pos != int64(offset):
		return nbdkit.PluginError{
			Errmsg: "Seek failed",
			Errno:  29, // ESPIPE
		}
	}

	// loop until the buffer is filled or an error occurs
	for nread := 0; nread < len(buf); {
		var n int
		if n, err = reader.Read(buf[nread:]); err != nil {
			// reader will return a io.EOF when reading the last byte
			// but we don't want to err out nbdkit if it is indeed the
			// last block of data
			if err == io.EOF && (offset+uint64(len(buf))) != c.tsize {
				return err
			}
			// On cache miss we get syscall.EIO but we still want to
			// honor the PRead so try to read a second time
			if err == syscall.EIO {
				pos, err := reader.Seek(int64(offset) + int64(nread), io.SeekStart)
				// ensure the seek operation landed at the correct position
				switch {
				case err != nil:
					return err
				case pos != int64(offset):
					return nbdkit.PluginError{
						Errmsg: "Seek failed",
						Errno:  29, // ESPIPE
					}
				}

				if n, err = reader.Read(buf[nread:]); err != nil {
					if err == io.EOF && (offset+uint64(len(buf))) != c.tsize {
						return err
					}
				}
			}
		}
		nread += n
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
