package main

import (
	"testing"
	"testing/fstest"
)

func TestLoadMigrations_SortsByVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/010_third.sql":  {Data: []byte("SELECT 3;")},
		"migrations/001_first.sql":  {Data: []byte("SELECT 1;")},
		"migrations/002_second.sql": {Data: []byte("SELECT 2;")},
	}

	migrations, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}

	if len(migrations) != 3 {
		t.Fatalf("expected 3 migrations, got %d", len(migrations))
	}
	if migrations[0].Version != 1 || migrations[1].Version != 2 || migrations[2].Version != 10 {
		t.Fatalf("unexpected migration ordering: %#v", migrations)
	}
}

func TestLoadMigrations_InvalidNameFails(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/invalid.sql": {Data: []byte("SELECT 1;")},
	}

	if _, err := loadMigrations(fsys); err == nil {
		t.Fatal("expected invalid migration filename to fail")
	}
}
