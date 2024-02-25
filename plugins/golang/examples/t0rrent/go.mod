module main

go 1.18

replace libguestfs.org/nbdkit => ../../src/libguestfs.org/nbdkit

require libguestfs.org/nbdkit v1.0.0

require github.com/anacrolix/torrent v1.54.1

require (
	bazil.org/fuse v0.0.0-20180421153158-65cc252bf669 // indirect
	github.com/RoaringBitmap/roaring v1.2.3 // indirect
	github.com/ajwerner/btree v0.0.0-20211221152037-f427b3e689c0 // indirect
	github.com/alecthomas/atomic v0.1.0-alpha2 // indirect
	github.com/anacrolix/chansync v0.3.0 // indirect
	github.com/anacrolix/dht v0.0.0-20181129074040-b09db78595aa // indirect
	github.com/anacrolix/dht/v2 v2.19.2-0.20221121215055-066ad8494444 // indirect
	github.com/anacrolix/envpprof v1.3.0 // indirect
	github.com/anacrolix/generics v0.0.0-20230816105729-c755655aee45 // indirect
	github.com/anacrolix/go-libutp v1.3.1 // indirect
	github.com/anacrolix/log v0.14.6-0.20231202035202-ed7a02cad0b4 // indirect
	github.com/anacrolix/missinggo v1.3.0 // indirect
	github.com/anacrolix/missinggo/perf v1.0.0 // indirect
	github.com/anacrolix/missinggo/v2 v2.7.2-0.20230527121029-a582b4f397b9 // indirect
	github.com/anacrolix/mmsg v1.0.0 // indirect
	github.com/anacrolix/multiless v0.3.0 // indirect
	github.com/anacrolix/stm v0.4.0 // indirect
	github.com/anacrolix/sync v0.5.1 // indirect
	github.com/anacrolix/tagflag v1.3.0 // indirect
	github.com/anacrolix/upnp v0.1.3-0.20220123035249-922794e51c96 // indirect
	github.com/anacrolix/utp v0.1.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/benbjohnson/immutable v0.3.0 // indirect
	github.com/bits-and-blooms/bitset v1.2.2 // indirect
	github.com/boltdb/bolt v1.3.1 // indirect
	github.com/bradfitz/iter v0.0.0-20191230175014-e8f45d346db8 // indirect
	github.com/cespare/xxhash v1.1.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815 // indirect
	github.com/dustin/go-humanize v1.0.0 // indirect
	github.com/edsrzf/mmap-go v1.1.0 // indirect
	github.com/elgatito/upnp v0.0.0-20180711183757-2f244d205f9a // indirect
	github.com/fsnotify/fsnotify v1.5.4 // indirect
	github.com/glycerine/go-unsnap-stream v0.0.0-20190901134440-81cf024a9e0a // indirect
	github.com/go-llsqlite/adapter v0.0.0-20230927005056-7f5ce7f0c916 // indirect
	github.com/go-llsqlite/crawshaw v0.4.0 // indirect
	github.com/go-logr/logr v1.2.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang/snappy v0.0.1 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/gosuri/uilive v0.0.0-20170323041506-ac356e6e42cd // indirect
	github.com/gosuri/uiprogress v0.0.0-20170224063937-d0567a9d84a1 // indirect
	github.com/huandu/xstrings v1.3.2 // indirect
	github.com/ipfs/go-ipfs v0.4.18 // indirect
	github.com/jessevdk/go-flags v1.5.0 // indirect
	github.com/mattn/go-isatty v0.0.16 // indirect
	github.com/mattn/go-sqlite3 v1.14.16 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/op/go-logging v0.0.0-20160315200505-970db520ece7 // indirect
	github.com/philhofer/fwd v1.0.0 // indirect
	github.com/pion/datachannel v1.5.2 // indirect
	github.com/pion/dtls/v2 v2.2.4 // indirect
	github.com/pion/ice/v2 v2.2.6 // indirect
	github.com/pion/interceptor v0.1.11 // indirect
	github.com/pion/logging v0.2.2 // indirect
	github.com/pion/mdns v0.0.5 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.9 // indirect
	github.com/pion/rtp v1.7.13 // indirect
	github.com/pion/sctp v1.8.2 // indirect
	github.com/pion/sdp/v3 v3.0.5 // indirect
	github.com/pion/srtp/v2 v2.0.9 // indirect
	github.com/pion/stun v0.3.5 // indirect
	github.com/pion/transport v0.13.1 // indirect
	github.com/pion/transport/v2 v2.0.0 // indirect
	github.com/pion/turn/v2 v2.0.8 // indirect
	github.com/pion/udp v0.1.4 // indirect
	github.com/pion/webrtc/v3 v3.1.42 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/dnscache v0.0.0-20211102005908-e0241e321417 // indirect
	github.com/ryszard/goskiplist v0.0.0-20150312221310-2dfbae5fcf46 // indirect
	github.com/spaolacci/murmur3 v0.0.0-20180118202830-f09979ecbc72 // indirect
	github.com/stretchr/testify v1.8.1 // indirect
	github.com/syncthing/syncthing v0.14.48-rc.4 // indirect
	github.com/tidwall/btree v1.6.0 // indirect
	github.com/tinylib/msgp v1.1.2 // indirect
	github.com/willf/bitset v1.1.10 // indirect
	github.com/willf/bloom v0.0.0-20170505221640-54e3b963ee16 // indirect
	go.etcd.io/bbolt v1.3.6 // indirect
	go.opentelemetry.io/otel v1.8.0 // indirect
	go.opentelemetry.io/otel/trace v1.8.0 // indirect
	golang.org/x/crypto v0.17.0 // indirect
	golang.org/x/exp v0.0.0-20230626212559-97b1e661b5df // indirect
	golang.org/x/net v0.10.0 // indirect
	golang.org/x/sync v0.3.0 // indirect
	golang.org/x/sys v0.15.0 // indirect
	golang.org/x/time v0.0.0-20220609170525-579cf78fd858 // indirect
	modernc.org/libc v1.22.3 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.21.1 // indirect
	zombiezen.com/go/sqlite v0.13.1 // indirect
)
