package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIndexRepoAndSearch(t *testing.T) {
	dir := t.TempDir()

	// create files
	f1 := filepath.Join(dir, "README.md")
	os.WriteFile(f1, []byte("This repository contains an example. Hello world example text."), 0644)

	f2 := filepath.Join(dir, "main.go")
	os.WriteFile(f2, []byte("package main\n// example function\nfunc main() { println(\"example\") }"), 0644)

	idx, err := IndexRepo(dir)
	assert.NoError(t, err)
	assert.NotNil(t, idx)
	assert.Greater(t, len(idx.Chunks), 0)

	// save and load
	p := filepath.Join(dir, "idx.json")
	err = idx.Save(p)
	assert.NoError(t, err)

	loaded, err := Load(p)
	assert.NoError(t, err)
	assert.Equal(t, len(idx.Chunks), len(loaded.Chunks))

	// search for a term we know exists
	res := loaded.Search("hello world", 10)
	assert.Greater(t, len(res), 0)
}
