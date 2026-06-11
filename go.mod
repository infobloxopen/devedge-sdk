module github.com/infobloxopen/devedge-sdk

go 1.25.5

require (
	github.com/google/uuid v1.6.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.27.7
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require google.golang.org/genproto/googleapis/api v0.0.0-20260226221140-a57be14db171 // indirect

require (
	github.com/infobloxopen/apis/proto/infoblox/authz v1.0.0-alpha.2
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
)
