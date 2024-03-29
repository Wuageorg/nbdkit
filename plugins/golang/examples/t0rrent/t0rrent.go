// Package main defines an NBD (Network Block Device) server plugin that serves data from a torrent file.
package main

import (
	"C"
	"unsafe"

	// Standard library imports
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	// Third-party package imports
	"libguestfs.org/nbdkit" // NBD server library

	"github.com/anacrolix/torrent"          // Torrent library
	"github.com/anacrolix/torrent/metainfo" // Metadata handling for torrents
	"github.com/anacrolix/torrent/storage"  // Torrent storage interfaces
)

var pluginName = "t0rrent"
var nbdDevice string

// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️ Plugin
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️
// 🎛️🎛️🎛️🎛️🎛️🎛️🎛️🎛️

// T0rrentPlugin represents the NBD server plugin for serving torrent data.
type T0rrentPlugin struct {
	nbdkit.Plugin                  // NBD plugin interface
	magnet        string           // Magnet link for the torrent
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
	case "device":
		nbdDevice = value
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
	case len(nbdDevice) == 0:
		return nbdkit.PluginError{Errmsg: "device parameter is required"}
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
	conf.DefaultStorage = NewRAMStorage()
	//conf.Debug = true

	var err error

	if tp.client, err = torrent.NewClient(conf); err != nil {
		return err
	}

	if tp.torrent, err = tp.client.AddMagnet(tp.magnet); err != nil {
		return err
	}

	tp.torrent.SetDisplayName("Downloading torrent metadata")

	select {
	case <-tp.torrent.GotInfo():
		nbdkit.Debug(fmt.Sprintf("Got torrent %s infos", tp.torrent.InfoHash()))
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
	// ignore readonly
	_ = readonly

	// TODO wait for GoTInfo here
	return &T0rrentConnection{
		size:   tp.torrent.Info().TotalLength(),
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
	size              int64
	reader            func() torrent.Reader
}

// GetSize retrieves the size of the torrent data.
func (tc *T0rrentConnection) GetSize() (uint64, error) {
	return uint64(tc.size), nil
}

// Clients are allowed to make multiple connections safely.
func (tc *T0rrentConnection) CanMultiConn() (bool, error) {
	return true, nil
}

// cannot returns false, nil
var cannot = func() (b bool, err error) { return }

// Clients are NOT allowed to flush.
func (tc *T0rrentConnection) CanFlush() (bool, error) {
	return cannot()
}

// Clients are NOT allowed to trim.
func (tc *T0rrentConnection) CanTrim() (bool, error) {
	return cannot()
}

// Clients are NOT allowed to write.
func (tc *T0rrentConnection) CanWrite() (bool, error) {
	return cannot()
}

// Clients are NOT allowed to zero.
func (tc *T0rrentConnection) CanZero() (bool, error) {
	return cannot()
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

	// convert offset once
	off := int64(offset)

	// acquire a torrent file reader
	torrent := tc.reader()
	defer torrent.Close()

	// seek to the specified offset in the torrent file
	pos, err := torrent.Seek(off, io.SeekStart)

	// ensure the seek operation landed at the correct position
	switch {
	case err != nil:
		// seeking raised an error
		return err
	case pos != off:
		// seeking failed to reach the expected position
		return nbdkit.PluginError{
			Errmsg: fmt.Sprintf("Seek failed got: %x expected: %x", pos, offset),
			Errno:  29, // ESPIPE
		}
	}

	// loop until the buffer is filled or an error occurs
	for nread := 0; nread < len(buf); {
		// read data into the buffer from the torrent file
		n, err := torrent.Read(buf[nread:])
		switch err {
		case nil:
			// nothing to do
		case syscall.EAGAIN:
			// prepare retry
			nread += n
			offset += uint64(nread)

			// retry read
			return tc.PRead(buf[nread:], offset, flags)
		default:
			// filter EOF on last block and raise
			if err != io.EOF || off+int64(len(buf)) != tc.size {
				return err
			}
		}
		// update read count
		nread += n
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

const TIMEOUT = 10

// RAMStorage represents an in-memory storage implementation for torrents.
type RAMStorage struct {
	storage.ClientImpl
	torrent *RAMTorrent
}

// NewRAMStorage creates a new RAMStorage instance.
func NewRAMStorage() (rs *RAMStorage) {
	rs = new(RAMStorage)
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
	pieces []RAMPiece // Pieces that are being downloaded
}

// NewRAMTorrent creates a new RAMTorrent instance and preallocate memory.
func NewRAMTorrent(mi *metainfo.Info) *RAMTorrent {
	tlen, plen := mi.TotalLength(), mi.PieceLength

	// preallocate pieces storage
	pieces := make([]RAMPiece, mi.NumPieces())
	base, last := int64(0), len(pieces)-1
	for i := range pieces[:last] {
		pieces[i].mkpiece(base, plen)
		base += plen
	}
	pieces[last].mkpiece(base, tlen%plen)

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

type RAMPieceState uint32

const (
	PARTIAL RAMPieceState = iota
	COMPLETE
	CACHED
	INVALID
)

// RAMPiece represents a piece of a torrent stored in memory.
type RAMPiece struct {
	storage.PieceImpl
	sync.Mutex
	stat   atomic.Uint32
	base   int64
	size   int64
	data   []byte
	cached slots
}

func (rp *RAMPiece) mkpiece(base int64, size int64) {
	rp.base = base
	rp.size = size

	rp.data = make([]byte, size)
	rp.cached = make(slots, 0)

	rp.stat.Store(uint32(PARTIAL))
}

func (rp *RAMPiece) mkcached() {
	rp.data = nil
	rp.cached = nil

	rp.stat.Store(uint32(CACHED))
}

// ReadAt reads data from a piece at the specified offset.
func (rp *RAMPiece) ReadAt(buf []byte, off int64) (int, error) {
	lo, hi := off, off+int64(len(buf))

	// prevent read loop
	stat0 := RAMPieceState(rp.stat.Load())
	if stat0 == INVALID {
		return 0, syscall.EIO
	}

	rp.Lock()
	defer rp.Unlock()

	stat1 := RAMPieceState(rp.stat.Load())
	if stat1 < stat0 {
		// the piece has been deprecated while waiting for it
		return 0, syscall.EAGAIN
	}

	switch stat1 {
	case PARTIAL:
		return copy(buf, rp.data[lo:hi:hi]), nil
	case COMPLETE:
		nelem := copy(buf, rp.data[lo:hi:hi])
		rp.cached = rp.cached.merge(off, len(buf))

		all := slot{0, int64(len(rp.data) - 1)}
		if len(rp.cached) == 1 && rp.cached[0] == all {
			// free storage and mark cached
			rp.mkcached()
		}

		return nelem, nil
	case CACHED:
		var (
			err   error
			dev   *os.File
			nelem int
		)

		if dev, err = os.Open(nbdDevice); err != nil {
			return 0, syscall.EIO
		}

		rp.stat.Store(uint32(INVALID))
		if nelem, err = dev.ReadAt(buf, rp.base+off); err != nil {
			// cache miss, prepare anew for a retry
			rp.mkpiece(rp.base, rp.size)
			return 0, syscall.EAGAIN
		}
		rp.stat.Store(uint32(CACHED))

		return nelem, nil
	}

	panic("unreachable")
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
	rp.stat.Store(uint32(COMPLETE))
	return nil
}

// MarkNotComplete marks a piece as not complete.
func (rp *RAMPiece) MarkNotComplete() error {
	rp.stat.Store(uint32(PARTIAL))
	return nil
}

// Completion returns the completion status of a piece.
func (rp *RAMPiece) Completion() storage.Completion {
	return storage.Completion{
		Complete: rp.stat.Load() >= uint32(COMPLETE),
		Ok:       true,
		Err:      nil,
	}
}

// Void resets the RAMPiece, marking it as missing and voiding its data.
func (rp *RAMPiece) Void() {
	rp.stat.Store(uint32(PARTIAL))
	rp.data = nil
}

type slot struct {
	start, stop int64
}

type slots []slot

func (L slots) merge(off int64, size int) slots {
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

	R := slots{{off, off + int64(size)}}
	if len(L) == 0 {
		return R
	}

	merged := make(slots, 0)

	il, ir := 0, 0
	for il < len(L) && ir < len(R) {
		l, r := L[il], R[ir]
		overlapLR := (r.start >= l.start) && (r.start <= l.stop)
		overlapRL := (l.start >= r.start) && (l.start <= r.stop)

		if overlapLR || overlapRL {
			merged := slot{
				start: min(l.start, r.start),
				stop:  max(l.stop, r.stop),
			}
			if l.stop < r.stop {
				R[ir] = merged
				il++
			} else {
				L[il] = merged
				ir++
			}
			continue
		}

		if l.stop < r.stop {
			merged = append(merged, l)
			il++
		} else {
			merged = append(merged, r)
			ir++
		}
	}

	merged = append(merged, L[il:]...)
	merged = append(merged, R[ir:]...)

	return merged
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
