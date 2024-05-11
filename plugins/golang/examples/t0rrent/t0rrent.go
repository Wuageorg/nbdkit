// Package main defines an NBD (Network Block Device) server plugin that serves data from a torrent file.
package main

import (
	"C"
	"unsafe"

	"bufio"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"crypto/sha1"

	"libguestfs.org/nbdkit"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	// "github.com/anacrolix/torrent/peer_protocol"
	ts "github.com/anacrolix/torrent/storage"

	gofibmap "github.com/frostschutz/go-fibmap"
	lru "github.com/hashicorp/golang-lru/v2"
	gommap "github.com/tysonmote/gommap"
)
import "reflect"

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

	mountpoint string
	torrs      map[metainfo.Hash]*TorrStor
	mu         sync.Mutex
}

// NewStorage creates a new Stor instance.
func NewStorage(mountpoint string) *Stor {
	stor := new(Stor)
	stor.mountpoint = mountpoint // eg: "/mnt"
	stor.torrs = make(map[metainfo.Hash]*TorrStor)
	return stor
}

// this will remove the log when a piece is anaivailable for a peer
// https://github.com/anacrolix/torrent/blob/967dc8b0d3680744a8f8872a30d5f249e320c755/peerconn.go#L678
func (s *Stor) StorCapacity() (int64, bool) { return 1<<63 - 1, true }

// OpenTorrent opens a torrent for reading.
func (s *Stor) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (ts.TorrentImpl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	torr := NewTorrStor(s, info, s.mountpoint)
	s.torrs[infoHash] = torr
	capaFun := s.StorCapacity
	return ts.TorrentImpl{Piece: torr.Piece, Close: torr.Close, Flush: torr.Flush, Capacity: &capaFun}, nil
}

func (s *Stor) SetTorrentsPtrs(client *torrent.Client) {
	for hash, st := range s.torrs {
		if ctorr, ok := client.Torrent(hash); !ok {
			panic(nil)
		} else {
			st.torrent = ctorr
		}
	}
}

// CloseTorrent closes a torrent.
func (s *Stor) CloseHash(hash metainfo.Hash) {
	if s.torrs == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if torr, ok := s.torrs[hash]; ok {
		torr.Close()
		delete(s.torrs, hash)
	}
}

// Close closes the storage and releases associated resources.
func (s *Stor) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, torr := range s.torrs {
		torr.torrent = nil // avoid reference loop
		torr.Close()
	}
	return nil
}

// GetTorrent retrieves a torrent by its hash.
func (s *Stor) GetTorrStor(hash metainfo.Hash) *TorrStor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if torr, ok := s.torrs[hash]; ok {
		return torr
	}
	return nil
}

type CachedFile struct {
	inter Interval
	path  *string
}

type MountCacheInfos struct {
	files []CachedFile
}

// TorrStor represents a torrent data pieces.
type TorrStor struct {
	// iface ts.TorrentImpl
	torrent     *torrent.Torrent
	stor        *Stor
	pieceLength int64
	isClosed    bool
	mountpoint  string

	mu       sync.Mutex
	preading []*Interval // Intervals being pread
	inFlight *lru.Cache[int, *InFlightPiece]

	mumci sync.Mutex
	mci   *MountCacheInfos
}

// NewTorrStor creates a new TorrStor instance.
func NewTorrStor(storage *Stor, info *metainfo.Info, mountpoint string) *TorrStor {
	inFlight, err := lru.New[int, *InFlightPiece](8)
	if err != nil {
		panic(err)
	}
	ret := &TorrStor{
		stor:        storage,
		pieceLength: info.PieceLength,
		mountpoint:  mountpoint,
		preading:    make([]*Interval, 0),
		inFlight:    inFlight,
	}

	go func() {
		for {
			if ret.mci == nil {
				// TODO use a gorountine
				// FIXME cannot exit nbdkit if stuck in lock
				log.Print("Locked")
				ret.mumci.Lock()
				log.Print("Locked inside")
				if ret.mci == nil {
					log.Printf("Alllocating !!!!\n")
					ret.mci = NewMountCacheInfo(&ret.mountpoint)
					log.Printf(".... Alllocated %p\n", ret.mci)
				}
				log.Print("UnLocked before")
				ret.mumci.Unlock()
				log.Print("UnLocked")
			}
			time.Sleep(2 * time.Second) // Check every 2seconds
		}
	}()

	return ret
}

func NewMountCacheInfo(mountpoint *string) *MountCacheInfos {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		log.Printf("Err: %v\n", err)
		return nil
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)
	mounted := false
	isNbd := false
	for scanner.Scan() {
		l := scanner.Text()
		if index := strings.Index(l, *mountpoint); index > 0 {
			mounted = true
			if strings.Contains(l[index:], "/dev/nbd") {
				isNbd = true
			}
		}
	}
	if err = scanner.Err(); err != nil {
		log.Printf("Err: %v\n", err)
		return nil
	}
	if !mounted {
		return nil // not mounted yet
	}
	if !isNbd {
		log.Printf("Err: %s\n", "Mountpoint is mounted but is not an NBD")
		return nil
	}
	log.Printf("Walkdir\n")
	ret := MountCacheInfos{files: make([]CachedFile, 0)}
	filepath.WalkDir(*mountpoint, func(path string, _d fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("Err walkdir: %v\n", err)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		fmf := gofibmap.NewFibmapFile(f)
		extents, err := fmf.FibmapExtents()
		for _, extent := range extents {
			ret.files = append(ret.files, CachedFile{path: &path, inter: Interval{extent.Physical, extent.Physical + extent.Length}})

		}
		return nil
	})
	slices.SortFunc(ret.files, func(a, b CachedFile) int {
		return int(a.inter.Lo - b.inter.Lo)
	})
	log.Printf("Neeeeew %d\n", len(ret.files))
	return &ret
}

func (t *TorrStor) cacheMmap(inter Interval) *CacheHitPiece {
	if t.mci == nil {
		return nil
	}

	idx, found := slices.BinarySearchFunc(t.mci.files, inter, func(cf CachedFile, inter Interval) int {
		if inter.Lo < cf.inter.Hi && inter.Lo >= cf.inter.Lo {
			return 0
		}
		if inter.Lo < cf.inter.Lo {
			return 1
		}
		return -1
	})
	if !found {
		log.Printf("Notound %v\n", inter)
		return nil
	}

	pagesize := uint64(syscall.Getpagesize()) // https://github.com/golang/go/commit/1b9499b06989d2831e5b156161d6c07642926ee1
	bRemaining := int(inter.Hi - inter.Lo)
	ret := new(CacheHitPiece)
	ret.buf = make([]byte, inter.Hi-inter.Lo)
	for bRemaining > 0 && idx < len(t.mci.files) {
		cf := t.mci.files[idx]
		foff, length := inter.Lo-cf.inter.Lo, min(uint64(bRemaining), cf.inter.Hi-cf.inter.Lo)
		moff, flength := uint64(0), length
		// Align on pagesize
		if foff%pagesize != 0 {
			nfoff := (foff - min(foff, pagesize)) & ^(pagesize - 1) // align down
			moff = (foff - nfoff)
			flength += moff
			foff = nfoff
		}
		if flength%pagesize != 0 {
			flength = (flength + pagesize) & ^(pagesize - 1) // align up
		}
		log.Printf("%v %s lo hi %d %d %d %d %d %d\n", inter, *cf.path, foff, length, foff%pagesize, length%pagesize, moff, flength)

		f, err := os.Open(*cf.path)
		if err != nil {
			log.Printf("Err opening %s %v\n", *cf.path, err)
			return nil
		}
		mmap, err := gommap.MapAt(0, f.Fd(), int64(foff), int64(flength), gommap.PROT_READ, gommap.MAP_SHARED)
		f.Close() // close file
		if err != nil {
			log.Printf("MapAt Err %v\n", err)
			return nil
		}
		defer func() {
			if err := mmap.UnsafeUnmap(); err != nil {
				log.Printf("UnsafeUnmap Err: %v\n", err)
			}
		}()
		residentPages, err := mmap.IsResident()
		if err != nil {
			log.Printf("IsResident Err %v\n", err)
			return nil
		}
		for i, r := range residentPages {
			if !r {
				i = i
				log.Printf("%v page %d not resident", inter, i)
				return nil
			}
		}
		n := copy(ret.buf[len(ret.buf)-bRemaining:], mmap[moff:(moff+length)])
		bRemaining -= n
		idx += 1
	}
	return ret
}

const (
	HonorReq int = iota
	DropReq  int = iota
	CacheHit int = iota
)

func logPiece(index int, color string) {
	log.Printf("|Piece piece=%d color=%s\n", index, color)
}

// Piece returns a piece of the torrent.
func (t *TorrStor) Piece(m metainfo.Piece) ts.PieceImpl {
	index := m.Index()
	off := m.Offset()
	plen := m.Length()

	logPiece(index, "grey")
	if p, ok := t.inFlight.Get(index); ok {
		logPiece(index, "lightgreen")
		return p
	}

	typ, chp := t.CanTryCache(index, Interval{uint64(off), uint64(off + plen)})
	switch typ {
	case CacheHit:
		logPiece(index, "lightblue")
		a := m.Hash().Bytes()
		b := sha1.Sum(chp.buf)
		if !reflect.DeepEqual(a[:], b[:]) {
			log.Printf("%d\n%x\n%x\n", index, a, b)
		}
		return chp
	case DropReq:
		logPiece(index, "salmon")
		return &DropReqPiece{}
	case HonorReq:
		if p, ok := t.inFlight.Get(index); ok {
			logPiece(index, "violet")
			return p
		} else {
			logPiece(index, "orange")
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

func (t *TorrStor) CanTryCache(index int, inter Interval) (int, *CacheHitPiece) {
	// log.Println("lock")
	t.mu.Lock()
	for _, interPtr := range t.preading {
		if inter.IsOverlapping(*interPtr) {
			t.mu.Unlock()
			return HonorReq, nil // preading right now
		}
	}
	t.mu.Unlock()

	// Check anacrolix internal state
	// log.Println("statein")
	// if t.torrent == nil || !t.torrent.Piece(index).State().Complete {
	// 	// piece incomplete and not preading -> no cache
	// 	return DropReq, nil
	// }
	// log.Println("stateout")

	// make sure it is in linux cache by asking the oracle function (aka kernel syscall),
	// if not in cache DropReq
	chp := t.cacheMmap(inter)
	return map[bool]int{true: CacheHit, false: DropReq}[chp != nil], chp
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
	buf []byte
}

func (p *CacheHitPiece) MarkComplete() error                      { return nil }
func (p *CacheHitPiece) MarkNotComplete() error                   { return nil }
func (p *CacheHitPiece) WriteAt(b []byte, off int64) (int, error) { panic(nil) }
func (p *CacheHitPiece) Completion() ts.Completion {
	return ts.Completion{Complete: true, Ok: true, Err: nil}
}

func (p *CacheHitPiece) ReadAt(b []byte, poff int64) (n int, err error) {
	return copy(b, p.buf[poff:poff+int64(len(b))]), nil
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
	mountpoint    string
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
	case "mountpoint":
		p.mountpoint = value
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
	case len(p.mountpoint) == 0:
		return nbdkit.PluginError{Errmsg: "mountpoint parameter is required"}
	}
	return nil
}

// GetReady prepares the plugin for serving torrent data.
func (p *T0rrentPlugin) GetReady() error {
	p.stor = NewStorage(p.mountpoint)

	conf := torrent.NewDefaultClientConfig()
	conf.Seed = true
	conf.AcceptPeerConnections = true
	conf.DisableIPv6 = false
	conf.DisableIPv4 = false
	conf.DisableTCP = true
	conf.DisableUTP = false
	// extensions := peer_protocol.NewPeerExtensionBytes(peer_protocol.ExtensionBitDht | peer_protocol.ExtensionBitFast | peer_protocol.ExtensionBitAzureusMessagingProtocol | peer_protocol.ExtensionBitAzureusExtensionNegotiation1 | peer_protocol.ExtensionBitAzureusExtensionNegotiation2 | peer_protocol.ExtensionBitLtep | peer_protocol.ExtensionBitLocationAwareProtocol) // ExtensionBitV2Upgrade
	// extensions := peer_protocol.NewPeerExtensionBytes(peer_protocol.ExtensionBitDht | peer_protocol.ExtensionBitFast) // ExtensionBitV2Upgrade
	// extensions = extensions
	// conf.Extensions = extensions
	// conf.MinPeerExtensions = extensions
	conf.DefaultStorage = p.stor
	// conf.Debug = true

	var err error
	if p.tManager, err = torrent.NewClient(conf); err != nil {
		return err
	}
	if p.t, err = p.tManager.AddMagnet(p.magnet); err != nil {
		return err
	}
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
	p.stor.SetTorrentsPtrs(p.tManager)
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

// Close terminates the client connection.
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
