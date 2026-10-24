module rcd_manager

go 1.26

require (
	github.com/BurntSushi/toml v1.4.0
	github.com/awcullen/opcua v1.4.0
	github.com/gopcua/opcua v0.9.1
	golang.org/x/sys v0.47.0
	modernc.org/sqlite v1.57.0
)

require (
	github.com/djherbis/buffer v1.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gammazero/deque v1.0.0 // indirect
	github.com/gammazero/workerpool v1.1.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/awcullen/opcua => ./third_party/awcullen-opcua

replace github.com/gopcua/opcua => ./third_party/gopcua-opcua
