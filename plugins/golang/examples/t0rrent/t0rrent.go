// Package main defines an NBD (Network Block Device) server plugin that serves data from a torrent file.
package main

import (
	"C"
	"unsafe"

	"runtime"
	"runtime/debug"

	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"libguestfs.org/nbdkit"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/peer_protocol"
	ts "github.com/anacrolix/torrent/storage"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Needs
// 14 local peers discovery missing
// 52 bittorentv2 missing (is on master)
// torrent modifications

// Have
// 19 webseeds
// 29 uTorrent transport protocol
// 27 private torrents
// 7 ipv6
// 6 fast extension (Have All/Have None, *Reject Request*: <len=0x000D><op=0x10><index><begin><length>)
// 5 dht

// FIXME how to not compile anacrolix webrtc stuff
// TODO benchmark with rusage and ram graph tui
// TODO webseed?

const TIMEOUT = 10

// 🫸🫷🫸🫷🫸🫷🫸🫷
// 🫸🫷🫸🫷🫸🫷🫸🫷
// 🫸 interval 🫷
// 🫸🫷🫸🫷🫸🫷🫸🫷
// 🫸🫷🫸🫷🫸🫷🫸🫷

type Interval struct {
	Lo, Hi uint64
}

// True if two interval overlaps
func (a Interval) IsOverlapping(b Interval) bool {
	overlap12 := (b.Lo >= a.Lo) && (b.Lo <= a.Hi)
	overlap21 := (a.Lo >= b.Lo) && (a.Lo <= b.Hi)
	return overlap12 || overlap21
}

// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾
// 💾💾 Storage
// 💾💾💾💾💾💾💾💾
// 💾💾💾💾💾💾💾💾

// Stor represents an in-memory storage implementation for torrents.
type Stor struct {
	ts.ClientImpl

	device string
	torrs  map[string]*TorrStor
	mu     sync.Mutex
}

// NewStorage creates a new Stor instance.
func NewStorage(device string) *Stor {
	stor := new(Stor)
	stor.device = device // eg: "/dev/nbd0"
	stor.torrs = make(map[string]*TorrStor)
	return stor
}

// OpenTorrent opens a torrent for reading.
func (s *Stor) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (ts.TorrentImpl, error) {
	h := infoHash.AsString()
	s.mu.Lock()
	defer s.mu.Unlock()
	torr := NewTorrStor(s, info, infoHash, s.device)
	s.torrs[h] = torr
	// TODO add torrent capacity https://pkg.go.dev/github.com/anacrolix/torrent@v1.54.1/storage#TorrentCapacity
	// this will remove the log when a piece is anaivailable for a peer
	// https://github.com/anacrolix/torrent/blob/967dc8b0d3680744a8f8872a30d5f249e320c755/peerconn.go#L678
	return ts.TorrentImpl{Piece: torr.Piece, Close: torr.Close, Flush: torr.Flush}, nil
}

// CloseTorrent closes a torrent.
func (s *Stor) CloseHash(hash metainfo.Hash) {
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
func (s *Stor) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, torr := range s.torrs {
		torr.Close()
	}
	return nil
}

// GetTorrent retrieves a torrent by its hash.
func (s *Stor) GetTorrStor(hash metainfo.Hash) *TorrStor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if torr, ok := s.torrs[hash.AsString()]; ok {
		return torr
	}
	return nil
}

const (
	HonorReq int = iota
	DropReq  int = iota
	CacheHit int = iota
)

// TorrStor represents a torrent data pieces.
type TorrStor struct {
	// iface ts.TorrentImpl
	hash        metainfo.Hash
	stor        *Stor
	pieceLength int64
	isClosed    bool
	device      string

	mu       sync.Mutex
	preading []*Interval // Intervals being pread
	inFlight *lru.Cache[int, *InFlightPiece]
}

// // NewTorrStor creates a new TorrStor instance.
func NewTorrStor(storage *Stor, info *metainfo.Info, hash metainfo.Hash, device string) *TorrStor {
	inFlight, err := lru.New[int, *InFlightPiece](8)
	if err != nil {
		panic(err)
	}
	return &TorrStor{
		hash:        hash,
		stor:        storage,
		pieceLength: info.PieceLength,
		device:      device,
		preading:    make([]*Interval, 0),
		inFlight:    inFlight,
	}
}

// Piece returns a piece of the torrent.
func (t *TorrStor) Piece(m metainfo.Piece) ts.PieceImpl {
	index := m.Index()
	off := m.Offset()
	plen := m.Length()

	log.Printf("|ReadAt piece=%d lo=%d hi=%d color=%s\n", index, 0, plen, "grey")
	if p, ok := t.inFlight.Get(index); ok {
		log.Printf("|ReadAt piece=%d lo=%d hi=%d color=%s\n", index, 0, plen, "purple")
		return p
	}

	switch t.CanTryCache(Interval{uint64(off), uint64(off + plen)}, func(inter Interval) bool {
		// TODO before doing an expensive syscall, check if anacrolix has the piece completed?, especially if it want it for writing
		// TODO check actual linux cache
		return false
	}) {
	case CacheHit:
		log.Printf("|ReadAt piece=%d lo=%d hi=%d color=%s\n", index, 0, plen, "blue")
		return &CacheHitPiece{device: &t.device, off: off}
	case DropReq:
		log.Printf("|ReadAt piece=%d lo=%d hi=%d color=%s\n", index, 0, plen, "red")
		return &DropReqPiece{}
	case HonorReq:
		if p, ok := t.inFlight.Get(index); ok {
			log.Printf("|ReadAt piece=%d lo=%d hi=%d color=%s\n", index, 0, plen, "violet")
			return p
		} else {
			log.Printf("|ReadAt piece=%d lo=%d hi=%d color=%s\n", index, 0, plen, "green")
			p := &InFlightPiece{complete: false, written: false, buf: make([]byte, plen)}
			t.inFlight.Add(index, p)
			return p
		}
	default:
		panic(nil)
	}
}

// Close closes the torrent.
func (t *TorrStor) Close() error {
	t.isClosed = true
	return nil
}

// Flush flushes any pending changes to the torrent.
func (t *TorrStor) Flush() error {
	return nil
}

func (t *TorrStor) PreadStart(inter Interval) *Interval {
	t.mu.Lock()
	defer t.mu.Unlock()

	ownedInter := new(Interval)
	ownedInter.Lo = inter.Lo
	ownedInter.Hi = inter.Hi

	t.preading = append(t.preading, ownedInter)
	return ownedInter
}

func (t *TorrStor) PreadEnd(ownedInter *Interval) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// remove from currently preading
	copyFiltered := make([]*Interval, 0, len(t.preading)-1)
	for _, interPtr := range t.preading {
		if interPtr != ownedInter {
			copyFiltered = append(copyFiltered, interPtr)
		}
	}
	t.preading = copyFiltered
}

func (t *TorrStor) CanTryCache(inter Interval, isInCacheFunc func(inter Interval) bool) int {
	{
		t.mu.Lock()
		defer t.mu.Unlock()

		for _, interPtr := range t.preading {
			if inter.IsOverlapping(*interPtr) {
				return HonorReq // preading right now
			}
		}
	} // unlocked after this point
	// make sure it is in cache by asking the oracle function (aka kernel syscall),
	// if not in cache dropreq
	inCache := isInCacheFunc(inter)
	return map[bool]int{true: CacheHit, false: DropReq}[inCache]
}

type DropReqPiece struct{}

func (p *DropReqPiece) MarkComplete() error    { return nil }
func (p *DropReqPiece) MarkNotComplete() error { return nil }
func (p *DropReqPiece) ReadAt(b []byte, off int64) (int, error) {
	return 0, fmt.Errorf("DropReqPiece ReadAt")
}
func (p *DropReqPiece) WriteAt(b []byte, off int64) (int, error) { return len(b), nil }
func (p *DropReqPiece) Completion() ts.Completion {
	return ts.Completion{Complete: false, Ok: true, Err: nil}
}

type CacheHitPiece struct {
	device *string
	off    int64
}

func (p *CacheHitPiece) MarkComplete() error                      { return nil }
func (p *CacheHitPiece) MarkNotComplete() error                   { return nil }
func (p *CacheHitPiece) WriteAt(b []byte, off int64) (int, error) { panic(nil) }
func (p *CacheHitPiece) Completion() ts.Completion {
	return ts.Completion{Complete: true, Ok: true, Err: nil}
}

func (p *CacheHitPiece) ReadAt(b []byte, poff int64) (n int, err error) {
	f, err := os.Open(*p.device)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if n, err = f.ReadAt(b, p.off+poff); err != nil {
		return 0, err
	}
	return n, nil
}

type InFlightPiece struct {
	complete bool
	written  bool
	buf      []byte
}

func (p *InFlightPiece) ReadAt(b []byte, off int64) (int, error) {
	if !p.written {
		// annacrolix think the piece is complete and tries to read data
		// but it was never written, send an error and anacrolix will re-download this piece
		return 0, fmt.Errorf("InFlightPiece ReadAt")
	}
	return copy(b, p.buf[off:off+int64(len(b))]), nil
}
func (p *InFlightPiece) WriteAt(b []byte, off int64) (int, error) {
	p.written = true
	return copy(p.buf[off:off+int64(len(b))], b), nil
}
func (p *InFlightPiece) Completion() ts.Completion {
	return ts.Completion{Complete: p.complete, Ok: true, Err: nil}
}

// MarkComplete marks a piece as complete.
func (p *InFlightPiece) MarkComplete() error {
	p.complete = true
	return nil
}

// MarkNotComplete marks a piece as not complete.
func (p *InFlightPiece) MarkNotComplete() error {
	p.complete = false
	return nil
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
	stor          *Stor
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
	// extensions := peer_protocol.NewPeerExtensionBytes(peer_protocol.ExtensionBitDht | peer_protocol.ExtensionBitFast | peer_protocol.ExtensionBitAzureusMessagingProtocol | peer_protocol.ExtensionBitAzureusExtensionNegotiation1 | peer_protocol.ExtensionBitAzureusExtensionNegotiation2 | peer_protocol.ExtensionBitLtep | peer_protocol.ExtensionBitLocationAwareProtocol) // ExtensionBitV2Upgrade
	extensions := peer_protocol.NewPeerExtensionBytes(peer_protocol.ExtensionBitDht | peer_protocol.ExtensionBitFast) // ExtensionBitV2Upgrade
	extensions = extensions
	// conf.Extensions = extensions
	conf.MinPeerExtensions = extensions
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
	p.t.SetOnWriteChunkError(func(err error) {
		log.Fatalf("writeChunkError %s\n", err)
	})

	return nil
}

// Open prepares the plugin for serving a client connection.
func (p *T0rrentPlugin) Open(readonly bool) (nbdkit.ConnectionInterface, error) {
	select {
	case <-p.t.GotInfo():
		nbdkit.Debug(fmt.Sprint("Got torrent %s infos", p.t.InfoHash()))
	case <-time.After(TIMEOUT * time.Second):
		return nil, nbdkit.PluginError{Errmsg: fmt.Sprint("Timeout, did not got torrent %s infos in %d seconds", p.t.InfoHash(), TIMEOUT)}
	}
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
		plugin: p,
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
	plugin            *T0rrentPlugin
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
				Errno:  syscall.EBADF,
			}
		}
		return nil
	}

	lenBuf := int64(len(buf))
	read := func(reader torrent.Reader, offset int64, nread int64) (n int64, err error) {
		var nI int
		if nI, err = reader.Read(buf[int(nread):]); err != nil {
			n = int64(nI)
			// reader will return a io.EOF when reading the last bytes
			// but we don't want to err out nbdkit if it is indeed the
			// last block of data
			if !(err == io.EOF && n != 0) {
				return
			}
			err = nil // remove io.EOF
		}
		n = int64(nI)
		return
	}

	var n int64
	var err error
	offset := int64(offseta)
	reader := c.plugin.t.NewReader()
	defer reader.Close()

	torrStor := c.plugin.stor.GetTorrStor(c.plugin.t.InfoHash())
	handle := torrStor.PreadStart(Interval{offseta, offseta + uint64(lenBuf)})
	defer torrStor.PreadEnd(handle)

	err = seek(reader, offset)
	// loop until the buffer is filled or an error occurs
	for nread := int64(0); err == nil && nread < lenBuf; {
		n, err = read(reader, offset, nread)
		nread += n
	}
	return err
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
