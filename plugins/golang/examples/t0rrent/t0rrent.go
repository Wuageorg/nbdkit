package main

import (
	"C"
	"unsafe"

	"fmt"
	"io"
	"strings"
	"sync"

	"libguestfs.org/nbdkit"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
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
type RAMStorage struct {
	storage.ClientImpl
	sync.Mutex
	device string // "/dev/nbd0"
	ramtos map[string]*RAMTorrent
}

func NewRAMStorage(nbdmount string) *RAMStorage {
	rs := new(RAMStorage)
	rs.device = nbdmount
	rs.ramtos = make(map[string]*RAMTorrent)
	return rs
}

func (rs *RAMStorage) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	h := infoHash.AsString()
	t := NewRAMTorrent(info)

	rs.Lock()
	rs.ramtos[h] = t
	rs.Unlock()

	return storage.TorrentImpl{
		Piece: t.Piece,
		Close: t.Close,
		Flush: t.Flush,
	}, nil //	OE
}

func (rs *RAMStorage) CloseTorrent(hash metainfo.Hash) {
	// defer debug.FreeOSMemory()
	// defer runtime.GC()

	rs.Lock()
	defer rs.Unlock()

	if len(rs.ramtos) == 0 {
		return
	}

	h := hash.AsString()
	if t, ok := rs.ramtos[h]; ok {
		t.Close() // close RAMTorrent
		delete(rs.ramtos, h)
	}
}

func (rs *RAMStorage) Close() {
	// defer debug.FreeOSMemory()
	// defer runtime.GC()

	rs.Lock()
	for _, t := range rs.ramtos {
		t.Close()
	}
	clear(rs.ramtos)
	rs.Unlock()

	return
}

func (rs *RAMStorage) GetTorrent(hash metainfo.Hash) *RAMTorrent {
	h := hash.AsString()

	rs.Lock()
	defer rs.Unlock()

	if t, ok := rs.ramtos[h]; ok {
		return t
	}
	return nil
}

type RAMTorrent struct {
	storage.TorrentImpl
	sync.RWMutex

	pieces []RAMPiece // Pieces that are being downloaded
}

func NewRAMTorrent(mi *metainfo.Info) *RAMTorrent {
	return &RAMTorrent{
		pieces: make([]RAMPiece, mi.NumPieces()),
	}
}

func (rt *RAMTorrent) Piece(mp metainfo.Piece) storage.PieceImpl {
	id, plen := mp.Index(), mp.Length()

	rt.Lock()
	p := &rt.pieces[id]

	// Allocate storage on demand
	if len(p.data) == 0 {
		p.data = make([]byte, plen)
	}
	rt.Unlock()

	return p
}

func (rt *RAMTorrent) Close() error {
	return nil
}

func (rt *RAMTorrent) Flush() error {
	return nil
}

type RAMPiece struct {
	storage.PieceImpl
	sync.RWMutex
	done bool
	data []byte
}

func (rp *RAMPiece) ReadAt(buf []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(buf))

	rp.Lock()
	n := copy(buf, rp.data[lo:hi:hi])
	rp.Unlock()

	// TODO if complete, check if in linux cache
	return n, nil
}

func (rp *RAMPiece) WriteAt(buf []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(buf))

	rp.Lock()
	n := copy(rp.data[lo:hi:hi], buf)
	rp.Unlock()

	return n, nil
}

func (rp *RAMPiece) MarkComplete() error {
	rp.done = true
	return nil
}

func (rp *RAMPiece) MarkNotComplete() error {
	rp.done = false
	return nil
}

func (rp *RAMPiece) Completion() storage.Completion {
	return storage.Completion{
		Complete: rp.done,
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
	magnet        string
	device        string
	client        *torrent.Client
	torrent       *torrent.Torrent
}

func (tp *T0rrentPlugin) Config(key string, value string) error {
	switch key {
	case "magnet":
		tp.magnet = value
		if !strings.HasPrefix(value, "magnet:") {
			tp.magnet = fmt.Sprintf("magnet:?xt=urn:btih:%s", value)
		}
	case "nbdmount":
		tp.device = value
	default:
		return nbdkit.PluginError{Errmsg: fmt.Sprintf("unknown parameter %s", key)}
	}

	return nil
}

func (tp *T0rrentPlugin) ConfigComplete() error {
	switch {
	case len(tp.magnet) == 0:
		return nbdkit.PluginError{Errmsg: "magnet parameter is required"}
	case len(tp.device) == 0:
		return nbdkit.PluginError{Errmsg: "nbdmount parameter is required"}
	}
	return nil
}

func (tp *T0rrentPlugin) GetReady() error {
	conf := torrent.NewDefaultClientConfig()
	conf.Seed = true
	conf.AcceptPeerConnections = true
	conf.DisableIPv6 = false
	conf.DisableIPv4 = false
	conf.DisableTCP = true
	conf.DisableUTP = false
	conf.DefaultStorage = NewRAMStorage(tp.device)
	conf.Debug = true

	var err error

	if tp.client, err = torrent.NewClient(conf); err != nil {
		return err
	}

	if tp.torrent, err = tp.client.AddMagnet(tp.magnet); err != nil {
		return err
	}

	tp.torrent.SetDisplayName("Downloading torrent metadata")
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
	<-tp.torrent.GotInfo() // wait till with get torrent infos // TODO timeout and fail

	nbdkit.Debug(fmt.Sprint("InfoHash ", tp.torrent.InfoHash()))
	nbdkit.Debug(fmt.Sprint("Name ", tp.torrent.Info().BestName()))
	nbdkit.Debug(fmt.Sprint("Pieces Length ", tp.torrent.Info().PieceLength))
	nbdkit.Debug(fmt.Sprint("CreationDate ", tp.torrent.Metainfo().CreationDate))
	nbdkit.Debug(fmt.Sprint("CreatedBy ", tp.torrent.Metainfo().CreatedBy))
	nbdkit.Debug(fmt.Sprint("Comment ", tp.torrent.Metainfo().Comment))

	for _, f := range tp.torrent.Files() {
		nbdkit.Debug(fmt.Sprintf("File %s (%d)", f.Path(), f.Length()))
	}
	return nil
}

func (tp *T0rrentPlugin) Open(readonly bool) (nbdkit.ConnectionInterface, error) {
	// TODO wait for GoTInfo here
	return &T0rrentConnection{
		size:   uint64(tp.torrent.Length()),
		reader: tp.torrent.NewReader(), // One reader per connection
	}, nil
}

func (tp *T0rrentPlugin) Unload() {
	if tp.client != nil {
		tp.client.Close()
	}
}

// The per-nbd-client struct.
type T0rrentConnection struct {
	nbdkit.Connection // iface
	size              uint64
	reader            torrent.Reader
}

func (tc *T0rrentConnection) GetSize() (uint64, error) {
	return tc.size, nil
}

// Clients are allowed to make multiple connections safely.
func (tc *T0rrentConnection) CanMultiConn() (bool, error) {
	return true, nil
}

func (tc *T0rrentConnection) CanWrite() (bool, error) {
	return false, nil
}

func (tc *T0rrentConnection) PWrite(buf []byte, offset uint64, flags uint32) error {
	return nil
}

func (tc *T0rrentConnection) PRead(buf []byte, offset uint64, flags uint32) error {
	pos, err := tc.reader.Seek(int64(offset), io.SeekStart)
	switch {
	case err != nil:
		return err
	case pos != int64(offset):
		return nbdkit.PluginError{
			Errmsg: fmt.Sprintf("Seek failed got: %x expected: %x", pos, offset),
			Errno:  29, // ESPIPE
		}
	}

	nread := 0
	for nread < len(buf) {
		n, err := tc.reader.Read(buf[nread:])
		if err != nil {
			return err
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
