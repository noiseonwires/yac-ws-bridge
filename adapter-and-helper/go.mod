module github.com/bridge-to-freedom/adapter

go 1.23.1

toolchain go1.24.5

require (
	github.com/gorilla/websocket v1.5.3
	golang.zx2c4.com/wireguard v0.0.0-20250521234502-f333402bd9cb
	google.golang.org/grpc v1.66.2
	gopkg.in/yaml.v3 v3.0.1
)

require golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect

require (
	github.com/yandex-cloud/go-genproto v0.62.0
	golang.org/x/net v0.39.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
	golang.org/x/text v0.24.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240903143218-8af14fe29dc1 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240903143218-8af14fe29dc1 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)
