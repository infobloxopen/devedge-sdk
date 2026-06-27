package deploy

import "fmt"

func init() { Register(ecsTarget{}) }

// ecsTarget is the AWS ECS seam STUB (F038 AC-1, scope gate). It satisfies the
// Target interface to prove the seam is open — adding a third runtime is adding
// an adapter, with NO core change — but renders nothing yet: ECS is intentionally
// out of scope for this feature (ship the seam + docs, not the implementation).
//
// To implement it, replace Render with the ECS task-definition + service
// rendering, wired to the same config/health/observability surface as k8s and
// compose. The registration here already makes `--deploy ecs` resolve.
type ecsTarget struct{}

func (ecsTarget) Name() string { return "ecs" }

func (ecsTarget) Render(ServiceView, Options) ([]Artifact, error) {
	return nil, fmt.Errorf(
		"deploy target %q is not implemented yet: it is a documented seam stub proving a "+
			"new runtime needs only an adapter (no core change). Use --deploy k8s,compose for now; "+
			"see docs/content/docs/guides/deploy.md (\"Future targets\")", "ecs")
}
