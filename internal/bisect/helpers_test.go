package bisect

import "os"
import "path/filepath"

// writeFileRaw is a test helper kept out of bisect_test.go so the test file
// reads as assertions rather than plumbing.
func writeFileRaw(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
