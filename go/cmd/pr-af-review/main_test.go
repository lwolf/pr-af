package main

import (
	"strings"
	"testing"

	"github.com/Agent-Field/pr-af/go/internal/schemas"
)

// The regression this guard exists for: a review whose model backend was
// rejected still returns a well-formed APPROVE with zero findings, which is
// byte-shaped like a clean review. Only dimensions_run tells them apart.
func TestCheckDimensionsRun(t *testing.T) {
	result := func(dimensions, findings int) schemas.ReviewResult {
		return schemas.ReviewResult{
			Summary: schemas.ReviewSummary{DimensionsRun: dimensions, TotalFindings: findings},
		}
	}

	cases := []struct {
		name    string
		result  any
		minimum int
		wantErr string
	}{
		{
			name:   "a clean review that actually ran dimensions passes",
			result: result(6, 0),
			// A genuinely clean PR has zero findings too, so findings must not
			// be part of the verdict.
			minimum: 1,
		},
		{
			name:    "a review with findings passes",
			result:  result(6, 4),
			minimum: 1,
		},
		{
			name:    "zero dimensions fails",
			result:  result(0, 0),
			minimum: 1,
			wantErr: "reviewed nothing",
		},
		{
			name:    "the guard can be switched off",
			result:  result(0, 0),
			minimum: 0,
		},
		{
			name:    "a higher floor is enforced",
			result:  result(2, 0),
			minimum: 3,
			wantErr: "review ran 2 dimensions, want at least 3",
		},
		{
			name:    "an unexpected payload type is an error, not a pass",
			result:  map[string]any{"summary": map[string]any{"dimensions_run": 9}},
			minimum: 1,
			wantErr: "unexpected review result type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDimensionsRun(tc.result, tc.minimum)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkDimensionsRun = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkDimensionsRun = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
