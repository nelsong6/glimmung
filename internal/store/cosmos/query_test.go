package cosmos

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
)

// rejectOrderingClauses is the core safety check for crossPartitionQuery.
// Anything that requires the client-side query plan handshake must be
// rejected at the API boundary; the failure mode otherwise is a Cosmos
// 400 surfaced as a 5xx in the handler layer (the production bug the
// query contract migration fixed). The test below codifies the allow
// and deny shapes; see docs/cosmos-partition-strategy.md for the why.
func TestRejectOrderingClauses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		query     string
		wantError bool
	}{
		{
			name:      "plain select where",
			query:     "SELECT * FROM c WHERE c.id = @id",
			wantError: false,
		},
		{
			name:      "select scalar where",
			query:     "SELECT c.leaseNumber FROM c WHERE c.project = @p",
			wantError: false,
		},
		{
			name:      "lowercase order by",
			query:     "SELECT * FROM c WHERE c.state = @s order by c.updated_at desc",
			wantError: true,
		},
		{
			name:      "uppercase order by",
			query:     "SELECT * FROM c WHERE c.state = @s ORDER BY c.updated_at DESC",
			wantError: true,
		},
		{
			name:      "group by",
			query:     "SELECT c.project, COUNT(1) FROM c GROUP BY c.project",
			wantError: true,
		},
		{
			name:      "distinct",
			query:     "SELECT DISTINCT c.project FROM c",
			wantError: true,
		},
		{
			name:      "offset",
			query:     "SELECT * FROM c WHERE c.state = @s OFFSET 10 LIMIT 10",
			wantError: true,
		},
		{
			name:      "top",
			query:     "SELECT TOP 5 * FROM c WHERE c.state = @s",
			wantError: true,
		},
		{
			name:      "identifier containing order_by_field is allowed",
			query:     "SELECT * FROM c WHERE c.order_by_field = @v",
			wantError: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectOrderingClauses(tc.query)
			if tc.wantError && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.query)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("expected no error for %q, got %v", tc.query, err)
			}
			if tc.wantError && err != nil {
				// Error message must name the disallowed clause so the
				// caller can fix it without spelunking through the SDK.
				upper := strings.ToUpper(err.Error())
				if !strings.Contains(upper, "QUERY PLAN") {
					t.Fatalf("error %q should mention 'query plan'", err.Error())
				}
			}
		})
	}
}

// crossPartitionQuery is the only legitimate consumer of an empty
// partition key. The runtime guard in the primitive itself rejects
// queries whose shape the gateway cannot serve directly; we exercise
// that path here so a regression that loosens the contract is caught
// in unit tests rather than at the next Cosmos call.
func TestCrossPartitionQueryRejectsOrdering(t *testing.T) {
	t.Parallel()
	var dst []map[string]any
	err := crossPartitionQuery(nil, nil,
		"SELECT * FROM c WHERE c.state = @s ORDER BY c.updated_at DESC",
		nil, &dst)
	if err == nil {
		t.Fatal("expected error from crossPartitionQuery with ORDER BY, got nil")
	}
	if !strings.Contains(err.Error(), "crossPartitionQuery") {
		t.Fatalf("error %q should mention crossPartitionQuery", err.Error())
	}
}

// fanOutByProject binds @project per iteration; the caller must not
// pre-bind it (would either double-bind in the parameter list or
// silently use the caller's value for every project) and the query
// must reference @project at all (otherwise the per-iteration partition
// key is meaningless). Both guards are tested here so the API contract
// is enforced without round-tripping to Cosmos.
func TestFanOutByProjectGuardsParameterBinding(t *testing.T) {
	t.Parallel()
	t.Run("query missing @project", func(t *testing.T) {
		t.Parallel()
		var dst []map[string]any
		err := fanOutByProject(nil, nil, []string{"p1"},
			"SELECT * FROM c WHERE c.state = @s",
			nil, &dst)
		if err == nil {
			t.Fatal("expected error when query does not reference @project")
		}
		if !strings.Contains(err.Error(), "@project") {
			t.Fatalf("error %q should mention @project", err.Error())
		}
	})
	t.Run("parameters pre-bind @project", func(t *testing.T) {
		t.Parallel()
		var dst []map[string]any
		err := fanOutByProject(nil, nil, []string{"p1"},
			"SELECT * FROM c WHERE c.project = @project",
			[]azcosmos.QueryParameter{{Name: "@project", Value: "shouldnt-be-here"}},
			&dst)
		if err == nil {
			t.Fatal("expected error when @project is pre-bound in parameters")
		}
		if !strings.Contains(err.Error(), "@project") {
			t.Fatalf("error %q should mention @project", err.Error())
		}
	})
	t.Run("empty project list is a no-op", func(t *testing.T) {
		t.Parallel()
		var dst []map[string]any
		err := fanOutByProject(nil, nil, nil,
			"SELECT * FROM c WHERE c.project = @project",
			nil, &dst)
		if err != nil {
			t.Fatalf("expected nil error for empty project list, got %v", err)
		}
		if len(dst) != 0 {
			t.Fatalf("expected empty destination, got %d rows", len(dst))
		}
	})
}
