package pg

import "testing"

func TestOperationFromSQLRecognizesEverySchemaTable(t *testing.T) {
	for _, table := range knownTables {
		sql := "SELECT * FROM " + table + " WHERE project = $1"
		want := "select_" + table
		if got := operationFromSQL(sql); got != want {
			t.Errorf("operationFromSQL(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestOperationFromSQLUsesBoundedSpecialCases(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{name: "insert", sql: "INSERT INTO runs (id) VALUES ($1)", want: "insert_runs"},
		{name: "update", sql: "UPDATE touchpoints SET updated_at = now()", want: "update_touchpoints"},
		{name: "delete", sql: "DELETE FROM run_events WHERE created_at < now()", want: "delete_run_events"},
		{name: "migration", sql: "CREATE TABLE IF NOT EXISTS projects (name text)", want: "migration"},
		{name: "cron", sql: "SELECT cron.schedule('run_events_ttl', '0 4 * * *', $$DELETE FROM run_events$$)", want: "cron_schedule"},
		{name: "ping", sql: "SELECT 1", want: "ping"},
		{name: "unknown", sql: "SELECT * FROM caller_controlled_table", want: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := operationFromSQL(tt.sql); got != tt.want {
				t.Fatalf("operationFromSQL(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}
