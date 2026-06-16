package persistence

import "fmt"

// FieldViolationError signals that a specific proto field failed validation.
// The ErrorMapper middleware converts this to codes.InvalidArgument with a
// google.rpc.BadRequest.FieldViolation detail (AIP-193).
type FieldViolationError struct {
	// Field is the proto field name (snake_case) that failed validation.
	Field string
	// Description is a human-readable explanation of the violation.
	Description string
}

func (e *FieldViolationError) Error() string {
	return fmt.Sprintf("field violation: %s: %s", e.Field, e.Description)
}

// NewFieldViolation constructs a FieldViolationError for the given proto field.
func NewFieldViolation(field, description string) *FieldViolationError {
	return &FieldViolationError{Field: field, Description: description}
}
