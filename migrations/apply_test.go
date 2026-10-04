package migrations_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/rcpassos/mergeyard/migrations"
	_ "modernc.org/sqlite"
)

func TestRejectInvalidMigrationSequenceBeforeApplyingSQL(t *testing.T) {
	cases := map[string][]string{
		"gap":                         {"001_first.sql", "003_third.sql"},
		"duplicate from two branches": {"001_first.sql", "002_left.sql", "002_right.sql"},
		"stray SQL file":              {"001_first.sql", "README.sql"},
		"renamed version":             {"002_first.sql"},
		"inserted version":            {"001_first.sql", "001_inserted.sql", "002_second.sql"},
		"missing name":                {"001_.sql"},
		"signed version":              {"+1_first.sql"},
	}
	for i := 1; i <= 10; i++ {
		cases["unpadded alphabetical order"] = append(cases["unpadded alphabetical order"], fmt.Sprintf("%d_step.sql", i))
	}
	for name, filenames := range cases {
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for _, version := range []int{0, 1} {
				if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
					t.Fatal(err)
				}
				source := fstest.MapFS{}
				for _, filename := range filenames {
					source[filename] = &fstest.MapFile{Data: []byte("CREATE TABLE IF NOT EXISTS must_not_exist (id INTEGER);")}
				}
				if err := migrations.Apply(context.Background(), db, source); err == nil {
					t.Fatalf("accepted invalid filenames at existing version %d: %v", version, filenames)
				}
				var gotVersion, tableCount int
				if err := db.QueryRow("PRAGMA user_version").Scan(&gotVersion); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tableCount); err != nil {
					t.Fatal(err)
				}
				if gotVersion != version || tableCount != 0 {
					t.Fatalf("invalid source changed database: version %d, tables %d", gotVersion, tableCount)
				}
			}
		})
	}
}

func TestApplyNewVersionAndRestart(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	source := fstest.MapFS{
		"001_create.sql": {Data: []byte("CREATE TABLE history (entry TEXT); INSERT INTO history VALUES ('first');")},
	}
	ctx := context.Background()
	if err := migrations.Apply(ctx, db, source); err != nil {
		t.Fatal(err)
	}
	source["002_append.sql"] = &fstest.MapFile{Data: []byte("INSERT INTO history VALUES ('second');")}
	for range 2 {
		if err := migrations.Apply(ctx, db, source); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("version = %d, err = %v", version, err)
	}
	var entries string
	if err := db.QueryRow("SELECT group_concat(entry, ',') FROM (SELECT entry FROM history ORDER BY rowid)").Scan(&entries); err != nil || entries != "first,second" {
		t.Fatalf("migration replayed or skipped: entries %q, err %v", entries, err)
	}
}

func TestFailedMigrationRollsBackSchemaAndVersion(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	source := fstest.MapFS{
		"001_create.sql": {Data: []byte("CREATE TABLE history (entry TEXT);")},
		"002_fail.sql":   {Data: []byte("INSERT INTO missing_table VALUES (1);")},
	}
	if err := migrations.Apply(context.Background(), db, source); err == nil {
		t.Fatal("broken SQL applied successfully")
	}
	var version, tableCount int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if version != 0 || tableCount != 0 {
		t.Fatalf("failed migration left partial state: version %d, tables %d", version, tableCount)
	}
}
