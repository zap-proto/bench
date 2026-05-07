module github.com/zap-proto/bench

go 1.23

require github.com/zap-proto/http v0.0.0

require (
	capnproto.org/go/capnp/v3 v3.0.1-alpha.2 // indirect
	github.com/colega/zeropool v0.0.0-20230505084239-6fb4a4f75381 // indirect
	golang.org/x/sync v0.7.0 // indirect
)

replace github.com/zap-proto/http => ../http
