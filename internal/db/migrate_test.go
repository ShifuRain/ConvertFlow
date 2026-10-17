package db

import (
	"strings"
	"testing"
	"testing/fstest"
)

func fsOf(names ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for _, n := range names {
		m["migrations/"+n] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	}
	return m
}

func TestLoadMigrationsUsesNumericPrefix(t *testing.T) {
	got, err := loadMigrations(fsOf("002_b.sql", "001_a.sql", "010_z.sql", "003_c.sql", "004_d.sql", "005_e.sql", "006_f.sql", "007_g.sql", "008_h.sql", "009_i.sql"), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 || got[0].version != 1 || got[0].name != "001_a.sql" || got[9].version != 10 || got[9].name != "010_z.sql" {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadMigrationsRejectsBadSets(t *testing.T) {
	cases := map[string][]string{
		"duplicate":   {"001_a.sql", "001_b.sql"},
		"gap":         {"001_a.sql", "003_c.sql"},
		"starts at 2": {"002_b.sql"},
		"zero":        {"000_a.sql", "001_b.sql"},
		"no prefix":   {"001_a.sql", "init.sql"},
		"bad name":    {"001_a.sql", "002 b.sql"},
		"other ext":   {"001_a.sql", "002_b.txt"},
	}
	for name, files := range cases {
		if _, err := loadMigrations(fsOf(files...), "migrations"); err == nil {
			t.Errorf("%s: want error for %v", name, files)
		}
	}
	if _, err := loadMigrations(fsOf("001_a.sql", "001_b.sql"), "migrations"); err == nil || !strings.Contains(err.Error(), "version 1") {
		t.Errorf("duplicate error text: %v", err)
	}
}

func TestEmbeddedMigrationsAreValid(t *testing.T) {
	got, err := loadMigrations(migrationFS, "migrations")
	if err != nil || len(got) == 0 || got[0].version != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}
