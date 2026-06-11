module github.com/infobloxopen/devedge-sdk/testdata/toy

go 1.25.5

require (
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.27.7
	github.com/infobloxopen/apis/proto/infoblox/authz v1.0.0-alpha.2
	github.com/infobloxopen/devedge-sdk v0.0.0
	google.golang.org/genproto/googleapis/api v0.0.0-20260226221140-a57be14db171
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
	gorm.io/gorm v1.31.1
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
)

replace github.com/infobloxopen/devedge-sdk => ../../
