package deploy

func init() { Register(composeTarget{}) }

// composeTarget is the Docker Compose adapter — a pure-local/lightweight runtime
// that proves the seam with a SECOND real adapter (AC-1). It wires the service to
// the SAME operational surface as the chart: the config env, a /healthz
// healthcheck, OTEL_* export (+ an optional otel-collector), the declared deps
// (e.g. Postgres), and a stop_grace_period matching the graceful-shutdown window.
type composeTarget struct{}

func (composeTarget) Name() string { return "compose" }

func (composeTarget) Render(svc ServiceView, opts Options) ([]Artifact, error) {
	opts = opts.withDefaults(svc)
	pg, hasPG := postgresDep(svc)
	data := composeData{
		Service:            svc.Name,
		EnvPrefix:          svc.EnvPrefix,
		GRPCPort:           svc.GRPCPort,
		HTTPPort:           svc.HTTPPort,
		GRPCAddr:           ":" + svc.GRPCPort,
		HTTPAddr:           ":" + svc.HTTPPort,
		GracePeriodSeconds: opts.GracePeriodSeconds,
		HasPostgres:        hasPG,
		PostgresImage:      pg.Image,
	}
	b, err := renderText("docker-compose.yml", composeTmpl, data)
	if err != nil {
		return nil, err
	}
	return []Artifact{{Path: "deploy/compose/docker-compose.yml", Contents: b}}, nil
}

type composeData struct {
	Service            string
	EnvPrefix          string
	GRPCPort           string
	HTTPPort           string
	GRPCAddr           string
	HTTPAddr           string
	GracePeriodSeconds int
	HasPostgres        bool
	PostgresImage      string
}

func postgresDep(svc ServiceView) (Dependency, bool) {
	for _, d := range svc.Deps {
		if d.Kind == "postgres" {
			if d.Image == "" {
				d.Image = "postgres:16-alpine"
			}
			return d, true
		}
	}
	return Dependency{}, false
}

const composeTmpl = `# Docker Compose deploy for {{.Service}} — a local/lightweight runtime wired to the
# SAME operational surface as the Helm chart: the config.ServerOptions env, a
# /healthz healthcheck, OTEL_* export, and a stop_grace_period matching the
# service's graceful shutdown (signal.NotifyContext on SIGTERM).
#
#   docker compose -f deploy/compose/docker-compose.yml up --build
services:
  {{.Service}}:
    build:
      # Build context is the repo root; supply a Dockerfile or override 'image:'.
      context: ../..
    image: {{.Service}}:dev
    ports:
      - "{{.GRPCPort}}:{{.GRPCPort}}"   # gRPC
      - "{{.HTTPPort}}:{{.HTTPPort}}"   # HTTP/JSON gateway
    environment:
      # config.ServerOptions env (#93), namespaced by the service's config prefix —
      # the same names the service main loads via config.Env.
      {{.EnvPrefix}}GRPC_ADDR: "{{.GRPCAddr}}"
      {{.EnvPrefix}}HTTP_ADDR: "{{.HTTPAddr}}"
      {{.EnvPrefix}}LOG_LEVEL: "info"
{{- if .HasPostgres}}
      {{.EnvPrefix}}DSN: "postgres://{{.Service}}:{{.Service}}@postgres:5432/{{.Service}}?sslmode=disable"
{{- else}}
      # No external DB declared: the service uses its built-in default DSN.
      # {{.EnvPrefix}}DSN: ""
{{- end}}
      # Observability (#90): point at the collector below to activate export.
      # Uncomment to send traces/metrics to the otel-collector service.
      # OTEL_EXPORTER_OTLP_ENDPOINT: "otel-collector:4317"
      # OTEL_EXPORTER_OTLP_PROTOCOL: "grpc"
    # Liveness via the HTTP /healthz probe (#91) — the same path the chart uses.
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:{{.HTTPPort}}/healthz"]
      interval: 10s
      timeout: 3s
      retries: 5
      start_period: 5s
    # Graceful shutdown: matches the chart's terminationGracePeriodSeconds and the
    # service's SIGTERM drain + OTel flush.
    stop_grace_period: {{.GracePeriodSeconds}}s
{{- if .HasPostgres}}
    depends_on:
      postgres:
        condition: service_healthy

  postgres:
    image: {{.PostgresImage}}
    environment:
      POSTGRES_USER: {{.Service}}
      POSTGRES_PASSWORD: {{.Service}}
      POSTGRES_DB: {{.Service}}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U {{.Service}}"]
      interval: 5s
      timeout: 3s
      retries: 10
    volumes:
      - pgdata:/var/lib/postgresql/data
{{- end}}

  # Optional collector — uncomment OTEL_EXPORTER_OTLP_ENDPOINT above to use it.
  # otel-collector:
  #   image: otel/opentelemetry-collector-contrib:latest
  #   command: ["--config=/etc/otelcol/config.yaml"]
  #   ports:
  #     - "4317:4317"   # OTLP gRPC
  #     - "4318:4318"   # OTLP HTTP
{{- if .HasPostgres}}

volumes:
  pgdata:
{{- end}}
`
