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
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
	// TODO add torrent capacity https://pkg.go.dev/github.com/anacrolix/torrent@v1.54.1/storage#TorrentCapacity
	// this will remove the log when a piece is anaivailable
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
	pieces	  []TorrPieceLinuxCache // Pieces
	piecesPread map[int]bool
	piecesToThrash map[int]bool
}

// // NewTorrStor creates a new TorrStorLinuxCache instance.
func NewTorrStor(storage *StorLinuxCache, info *metainfo.Info, hash metainfo.Hash, device string) *TorrStorLinuxCache {
	pcnt := info.NumPieces()
	return &TorrStorLinuxCache{
		hash:        hash,
		pieceLength: info.PieceLength,
		device:      device,
		pieces:      make([]TorrPieceLinuxCache, pcnt, pcnt),
		piecesPread: make(map[int]bool, 0),
		piecesToThrash: make(map[int]bool, 0),
	}
}

func (t *TorrStorLinuxCache) markPread(offset int64) {
	pieceId := int(offset / t.pieceLength)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.piecesPread[pieceId] = true
}



func (t *TorrStorLinuxCache) isPread(pieceId int, pieceState uint32) bool {
	if pieceState == 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.piecesPread[pieceId]; ok {
		return true
	}
	return false
}

func (t *TorrStorLinuxCache) unmarkPread(offset int64) {
	pieceId := int(offset / t.pieceLength)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.piecesPread, pieceId)
}

func (t *TorrStorLinuxCache) shouldThrash(offset int64) bool {
	pieceId := int(offset / t.pieceLength)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.piecesToThrash[pieceId]; ok {
		log.Println("------- shouldThrash true", pieceId, offset)
		return true
	}
	return false
}

// Piece returns a piece of the torrent.
func (t *TorrStorLinuxCache) Piece(m metainfo.Piece) ts.PieceImpl {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := m.Index()
	p := &t.pieces[id]
	if p.size == 0 {
		size := m.Length()
		p.id = id
		p.offset = m.Offset()
		p.size = size
		p.device = &t.device
		p.torrent = t
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

// TorrPieceLinuxCache represents a piece of a torrent.
type TorrPieceLinuxCache struct {
	id        int
	offset    int64
	size      int64
	device    *string
	buf       []byte
	readslots slots
	torrent   *TorrStorLinuxCache
	mu        sync.RWMutex
	// piece state
	// 0 - incomplete
	// 1 - complete in memory
	// 2 - only in linux cache
	// 3 - Read loopback, cache miss
	state atomic.Uint32
}

// ReadAt reads data from a piece at the specified offset.
func (p *TorrPieceLinuxCache) ReadAt(b []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(b))

	beforeState := p.state.Load()
	isPread := p.torrent.isPread(p.id, beforeState)
	reterr := func() error {
		if isPread {
			return syscall.EAGAIN // read from nbdkit, honor the read
		} else {
			return syscall.EIO // read from another peer don't honor the read
		}
	}
	if beforeState > 2 { // before locking the piece, we check the read loopback situation
		// cache miss
		p.torrent.mu.Lock()
		defer p.torrent.mu.Unlock()
		p.state.Store(0) // set incomplete so caller doesn't loop
		p.torrent.piecesToThrash[p.id] = true
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, 0, p.offset, lo, hi, beforeState, "purple")
		return 0, reterr()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state.Load()
	if beforeState > state { // state changed while waiting for lock
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, state, p.offset, lo, hi, beforeState, "violet")
		return 0, reterr()
	}
	switch state {
	case 0: // use membuf
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, state, p.offset, lo, hi, beforeState, "green")
		if p.buf == nil {
			return 0, reterr()
		}
		return copy(b, p.buf[lo:hi:hi]), nil
	case 1: // not entirely in linux cache
		n := copy(b, p.buf[lo:hi:hi])
		if p.readslots == nil {
			p.readslots = slots{}
		}
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, 1, p.offset, lo, hi, beforeState, map[bool]string{true: "aquamarine", false: "yellow"} [isPread])
		if isPread {
			p.readslots = p.readslots.merge(lo, hi)
			if len(p.readslots) == 1 && p.readslots[0].start == 0 && p.readslots[0].stop == p.size && p.id != 0 && p.id != 1 && p.id != 8027 && p.id != 8026 && p.id != 8025  { // completly read
				p.buf = nil // remove buf
				p.readslots = nil
				p.state.Store(2)
				log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, 2, p.offset, 0, p.size, 2, "cyan")
			}
		}
		return n, nil
	case 2: // ReadAt from another peer
		// read from linux cache
		p.state.Store(3)
		var err error
		n := 0
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, 3, p.offset, lo, hi, 3, "teal")
		f, err := os.Open(*p.device)
		if err != nil {
			goto cachemiss
		}
		defer f.Close()
		if n, err = f.ReadAt(b, p.offset+off); err != nil {
			goto cachemiss
		}
		p.state.Store(2)
		if err == nil && n != len(b) {
			panic(nil)
		}
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, 2, p.offset, lo, hi, 2, "blue")
		return n, nil

	cachemiss: // goto label // err != nil -> cache miss
		p.readslots = nil
		p.state.Store(0)
		p.torrent.mu.Lock()
		defer p.torrent.mu.Unlock()
		delete(p.torrent.piecesToThrash, p.id)
		log.Printf("|ReadAt piece=%d state=%d poff=%d lo=%d hi=%d bstate=%d color=%s\n", p.id, 0, p.offset, lo, hi, 3, "red")
		return 0, err
	default:
		panic(state)
	}
}

// WriteAt writes data to a piece at the specified offset.
func (p *TorrPieceLinuxCache) WriteAt(b []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(b))
	// log.Println("------------- WriteAt", p.id, p.offset, off, len(b))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buf == nil {
		p.buf = make([]byte, p.size, p.size)
	}
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
	stor          *StorLinuxCache
	torrentStor   *TorrStorLinuxCache
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
	p.stor = NewStorage(p.device)

	conf := torrent.NewDefaultClientConfig()
	conf.Seed = true
	conf.AcceptPeerConnections = true
	conf.DisableIPv6 = false
	conf.DisableIPv4 = false
	conf.DisableTCP = true
	conf.DisableUTP = false
	conf.DefaultStorage = p.stor
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
	p.torrentStor = p.stor.GetTorrent(p.t.InfoHash())
	if p.torrentStor == nil {
		panic(nil)
	}
	return &T0rrentConnection{
		tsize:     uint64(p.t.Length()),
		plugin:	   p,
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
	plugin 		 *T0rrentPlugin
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
func (c *T0rrentConnection) PRead(buf []byte, offseta uint64, flags uint32) error {
	seek := func(reader torrent.Reader, offset int64) error {
		pos, err := reader.Seek(offset, io.SeekStart)
		// ensure the seek operation landed at the correct position
		switch {
		case err != nil:
			return err
		case pos != offset:
			return nbdkit.PluginError{
				Errmsg: "Seek failed",
				Errno:  syscall.ESPIPE,
			}
		}
		return nil
	}

	read := func(reader torrent.Reader, offset int64, nread int64) (n int64, err error) {
		c.plugin.torrentStor.markPread(offset + nread)
		defer c.plugin.torrentStor.unmarkPread(offset + nread) // this will save the value of nread here
		var nI int
		if nI, err = reader.ReadContext(NewCacheMissContext(c.plugin.torrentStor.shouldThrash, offset), buf[int(nread):]); err != nil {
			n = int64(nI)
			// On cache miss we get syscall.EAGAIN but we still want to
			// honor the PRead so try to read a second time
			if err == syscall.EAGAIN {
				if err = seek(reader, offset + nread); err != nil {
					return
				}
				if nI, err = reader.ReadContext(NewCacheMissContext(c.plugin.torrentStor.shouldThrash, offset), buf[int(nread):]); err != nil {
					if !(err == io.EOF && n != 0) {
						return
					}
				}
			}

			// reader will return a io.EOF when reading the last bytes
			// but we don't want to err out nbdkit if it is indeed the
			// last block of data
			if !(err == io.EOF && n != 0) {
				return
			}
		}
		n = int64(nI)
		return
	}

	var n int64
	var err error
	offset := int64(offseta)
	reader := c.readerfun()
	defer reader.Close()

	seek(reader, offset)
	// loop until the buffer is filled or an error occurs
	for nread := int64(0); nread < int64(len(buf)); {
		if n, err = read(reader, offset, nread); err != nil {
			return err
		}
		nread += n
	}
	return nil
}

// Implement context for to thrash pieces
type CacheMissContext struct {
	checkFunc func(int64) bool
	offset int64
}

func NewCacheMissContext(checkFunc func(int64) bool, offset int64) CacheMissContext {
	return CacheMissContext{
		checkFunc: checkFunc,
		offset: offset,
	}
}

func (CacheMissContext) Deadline() (deadline time.Time, ok bool) {
	return
}

func (c CacheMissContext) Done()  <-chan struct{} {
	if c.checkFunc(c.offset) {
		var closedChan = make(chan struct{})
		close(closedChan)
		return closedChan
	} else {
		return nil
	}
}

// Called if Done() returns a value
func (CacheMissContext) Err() error {
	return syscall.EIO
}

func (CacheMissContext) Value(key any) any {
	return nil
}

func (*T0rrentConnection) String() string {
	return "T0rrentConnection.PRead"
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
