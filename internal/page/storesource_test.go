package page

import (
	"os"
	"testing"
)

// readStoreSource returns store.go's text. A comment is a claim about the file it sits
// in, so the guard that checks the claim reads the file rather than a copy of it.
func readStoreSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	return string(b)
}
