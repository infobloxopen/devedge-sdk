package deploy

import "embed"

// chartFS holds the framework-owned Helm chart (F038, AC-2). It is the SINGLE
// source of truth for a devedge-sdk service's k8s objects: developers never
// author or even see it — the k8s target emits only a Flux HelmRelease +
// OCIRepository source + a thin values overlay into the service repo. The chart
// itself is published by the framework (OCI/Helm repo) and referenced by version.
//
// The embed keeps the chart inside the tooling binary so it travels with the CLI
// and can be lint/template-validated in tests; it never enters the service
// runtime (AC-6).
//
//go:embed helm/chart/Chart.yaml helm/chart/values.yaml helm/chart/templates/*
var chartFS embed.FS
