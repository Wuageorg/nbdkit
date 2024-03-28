// Package main defines an NBD (Network Block Device) server plugin that serves data from a torrent file.
package main

import (
	"C"
	"unsafe"

	// Standard library imports
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	// Third-party package imports
	"libguestfs.org/nbdkit" // NBD server library

	"github.com/anacrolix/torrent"          // Torrent library
	"github.com/anacrolix/torrent/metainfo" // Metadata handling for torrents
	"github.com/anacrolix/torrent/storage"  // Torrent storage interfaces
)

var pluginName = "t0rrent"

// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️ Plugin
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️

// T0rrentPlugin represents the NBD server plugin for serving torrent data.
type T0rrentPlugin struct {
	nbdkit.Plugin                  // NBD plugin interface
	magnet        string           // Magnet link for the torrent
	device        string           // NBD device path
	client        *torrent.Client  // Torrent client
	torrent       *torrent.Torrent // Torrent object
}

// Config processes the plugin configuration parameters.
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

// ConfigComplete checks if the plugin configuration is complete.
func (tp *T0rrentPlugin) ConfigComplete() error {
	switch {
	case len(tp.magnet) == 0:
		return nbdkit.PluginError{Errmsg: "magnet parameter is required"}
	case len(tp.device) == 0:
		return nbdkit.PluginError{Errmsg: "nbdmount parameter is required"}
	}
	return nil
}

// GetReady prepares the plugin for serving torrent data.
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

	select {
	case <-tp.torrent.GotInfo():
		nbdkit.Debug(fmt.Sprint("Got torrent %s infos", tp.torrent.InfoHash()))
	case <-time.After(TIMEOUT * time.Second):
		return fmt.Errorf("Did not got torrent %s infos in %d seconds", tp.torrent.InfoHash(), TIMEOUT)
	}

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

// Open prepares the plugin for serving a client connection.
func (tp *T0rrentPlugin) Open(readonly bool) (nbdkit.ConnectionInterface, error) {
	// TODO wait for GoTInfo here
	return &T0rrentConnection{
		size:   uint64(tp.torrent.Info().TotalLength()),
		reader: tp.torrent.NewReader,
	}, nil
}

// Unload releases resources used by the plugin.
func (tp *T0rrentPlugin) Unload() {
	if tp.client != nil {
		tp.client.Close()
	}
}

// T0rrentConnection represents a client connection for serving torrent data.
type T0rrentConnection struct {
	nbdkit.Connection // connection interface
	size              uint64
	reader            func() torrent.Reader
}

// GetSize retrieves the size of the torrent data.
func (tc *T0rrentConnection) GetSize() (uint64, error) {
	return tc.size, nil
}

// Clients are allowed to make multiple connections safely.
func (tc *T0rrentConnection) CanMultiConn() (bool, error) {
	return true, nil
}

// Clients are NOT allowed to flush.
func (tc *T0rrentConnection) CanFlush() (bool, error) {
	return false, nil
}

// Clients are NOT allowed to trim.
func (tc *T0rrentConnection) CanTrim() (bool, error) {
	return false, nil
}

// Clients are NOT allowed to write.
func (tc *T0rrentConnection) CanWrite() (bool, error) {
	return false, nil
}

// Clients are NOT allowed to zero.
func (tc *T0rrentConnection) CanZero() (bool, error) {
	return false, nil
}

// Close termitates the client connection.
func (tc *T0rrentConnection) Close() {
	tc.size = 0
	return
}

// PRead reads data from the torrent file at the specified offset into the provided buffer.
func (tc *T0rrentConnection) PRead(buf []byte, offset uint64, flags uint32) error {
	// ignore flags
	_ = flags

	// acquire a torrent file reader
	torrent := tc.reader()
	defer torrent.Close()

	// seek to the specified offset in the torrent file
	pos, err := torrent.Seek(int64(offset), io.SeekStart)

	// ensure the seek operation landed at the correct position
	switch {
	case err != nil:
		// seeking raised an error
		return err
	case pos != int64(offset):
		// seeking failed to reach the expected position
		return nbdkit.PluginError{
			Errmsg: fmt.Sprintf("Seek failed got: %x expected: %x", pos, offset),
			Errno:  29, // ESPIPE
		}
	}

	// loop until the buffer is filled or an error occurs
	for nread := 0; nread < len(buf); {
		// read data into the buffer from the torrent file
		if n, err := torrent.Read(buf[nread:]); err != nil {
			// read error
			return err
		} else {
			// update the number of bytes read
			nread += n
		}
	}
	return nil
}

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

const TIMEOUT = 300

// RAMStorage represents an in-memory storage implementation for torrents.
type RAMStorage struct {
	storage.ClientImpl
	device  string // "/dev/nbd0"
	torrent *RAMTorrent
}

// NewRAMStorage creates a new RAMStorage instance.
func NewRAMStorage(nbdmount string) (rs *RAMStorage) {
	rs = new(RAMStorage)
	rs.device = nbdmount
	return
}

// OpenTorrent opens a torrent for reading.
func (rs *RAMStorage) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	t := NewRAMTorrent(info)
	rs.torrent = t

	return storage.TorrentImpl{
		Piece: t.Piece,
		Close: t.Close,
		Flush: t.Flush,
	}, nil
}

// RAMTorrent represents a torrent stored in memory.
type RAMTorrent struct {
	storage.TorrentImpl
	sync.RWMutex

	pieces []RAMPiece // Pieces that are being downloaded
}

// NewRAMTorrent creates a new RAMTorrent instance and preallocate memory.
func NewRAMTorrent(mi *metainfo.Info) *RAMTorrent {
	tlen, plen := mi.TotalLength(), mi.PieceLength

	// preallocate pieces storage
	pieces := make([]RAMPiece, mi.NumPieces())
	last := len(pieces) - 1
	for i := range pieces[:last] {
		pieces[i].data = make([]byte, plen)
	}
	pieces[last].data = make([]byte, tlen%plen)

	return &RAMTorrent{
		pieces: pieces,
	}
}

// Piece returns a piece of the torrent.
func (rt *RAMTorrent) Piece(mp metainfo.Piece) storage.PieceImpl {
	return &rt.pieces[mp.Index()]
}

// Close closes the torrent and releases the storage.
func (rt *RAMTorrent) Close() error {
	for i := range rt.pieces {
		rt.pieces[i].Void()
	}
	rt.pieces = nil
	return nil
}

// Flush flushes any pending changes to the torrent.
func (rt *RAMTorrent) Flush() error {
	// nothing to do
	return nil
}

// RAMPiece represents a piece of a torrent stored in memory.
type RAMPiece struct {
	storage.PieceImpl
	sync.RWMutex
	done bool
	data []byte
}

// ReadAt reads data from a piece at the specified offset.
func (rp *RAMPiece) ReadAt(buf []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(buf))

	// TODO if complete, check if in linux cache

	rp.RLock()
	defer rp.RUnlock()

	return copy(buf, rp.data[lo:hi:hi]), nil
}

// WriteAt writes data to a piece at the specified offset.
func (rp *RAMPiece) WriteAt(buf []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(buf))

	rp.Lock()
	defer rp.Unlock()

	return copy(rp.data[lo:hi:hi], buf), nil
}

// MarkComplete marks a piece as complete.
func (rp *RAMPiece) MarkComplete() error {
	rp.done = true
	return nil
}

// MarkNotComplete marks a piece as not complete.
func (rp *RAMPiece) MarkNotComplete() error {
	rp.done = false
	return nil
}

// Completion returns the completion status of a piece.
func (rp *RAMPiece) Completion() storage.Completion {
	return storage.Completion{
		Complete: rp.done,
		Ok:       true,
		Err:      nil,
	}
}

// Void resets the RAMPiece, marking it as not complete and voiding its data.
func (rp *RAMPiece) Void() {
	rp.done = false
	rp.data = nil
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
