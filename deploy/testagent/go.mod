module github.com/maritimeconnectivity/MMS/deploy/testagent

go 1.26.0

require (
	github.com/coder/websocket v1.8.15
	github.com/google/uuid v1.6.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	google.golang.org/protobuf v1.36.11
)

replace github.com/maritimeconnectivity/MMS/mmtp => ../../mmtp
