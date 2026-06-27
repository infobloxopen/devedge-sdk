package config

// ServerOptions holds the canonical per-service configuration that the SDK
// scaffold loads through config.Load. Fields are tagged with their lookup key
// and a sane default.
//
// The scaffold loads this with:
//
//	var opts ServerOptions
//	config.Load(&opts, config.Flags(fs), config.Env("MYSVC_"), config.DotEnv(".env"))
type ServerOptions struct {
	// GRPCAddr is the TCP listen address for the gRPC endpoint.
	GRPCAddr string `config:"GRPC_ADDR" default:":9090"`
	// HTTPAddr is the TCP listen address for the HTTP/JSON gateway.
	// Empty disables the gateway.
	HTTPAddr string `config:"HTTP_ADDR" default:":8080"`
	// LogLevel is the minimum log level (debug, info, warn, error).
	LogLevel string `config:"LOG_LEVEL" default:"info"`
	// OTLPEndpoint is the OpenTelemetry collector endpoint (host:port).
	// Empty means honor OTEL_EXPORTER_OTLP_ENDPOINT or no-op.
	OTLPEndpoint string `config:"OTLP_ENDPOINT" default:""`
	// DSN is the database connection string.
	// Empty means the service manages its own default (e.g. in-memory sqlite).
	DSN string `config:"DSN" default:""`
}
