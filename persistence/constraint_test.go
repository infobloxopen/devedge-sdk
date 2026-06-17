package persistence

import (
	"errors"
	"testing"
)

func TestConstraintError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"nil", nil, nil},
		{"sqlite unique", errors.New("UNIQUE constraint failed: destination_models.name"), ErrConflict},
		{"postgres unique", errors.New(`pq: duplicate key value violates unique constraint "ux_dest"`), ErrConflict},
		{"mysql unique", errors.New("Error 1062: Duplicate entry 'x' for key 'name'"), ErrConflict},
		{"postgres sqlstate unique", errors.New("ERROR: ... (SQLSTATE 23505)"), ErrConflict},
		{"sqlite fk", errors.New("FOREIGN KEY constraint failed"), ErrPreconditionFailed},
		{"postgres fk", errors.New("violates foreign key constraint"), ErrPreconditionFailed},
		{"sqlite not null", errors.New("NOT NULL constraint failed: x.y"), ErrPreconditionFailed},
		{"unrelated", errors.New("connection refused"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConstraintError(tc.err)
			if !errors.Is(got, tc.want) {
				t.Errorf("ConstraintError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
