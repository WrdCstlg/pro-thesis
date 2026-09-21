package recorder

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// TestCodecsAgreeOnEveryStringByte closes the seam that TestRecorderAndSchemaCodecsAgree
// leaves open.
//
// Phase 0 definition of done (b) is that a world round-trips through `.thesis`
// serialization byte-identically. That statement is only meaningful if the tree
// has ONE canonical form. It briefly had two: internal/recorder/cjson (a
// reflective encoder) and pkg/schema's stdlib canonicalizer. They agree on every
// world any realistic run produces, which is why the existing agreement tests
// pass: but they are not the same function.
//
// The witnesses below are strings for which encoding/json and cjson provably
// disagree. U+2028 and U+2029 are escaped unconditionally by encoding/json,
// including with SetEscapeHTML(false), and emitted raw by cjson. The consequence
// is a genuine split brain rather than a cosmetic difference: recorder.StoreWorld
// writes a file that schema.UnmarshalWorld then refuses to load as
// non-canonical, and the two codecs disagree about what a valid `.thesis` file
// even is.
//
// This test asserts the property Phase 0 actually needs: the two entry points
// are the same function on every input, not merely on the corpus someone
// happened to sample.
func TestCodecsAgreeOnEveryStringByte(t *testing.T) {
	witnesses := []struct {
		name  string
		value string
	}{
		{"line_separator", "topo" + "\u2028" + "variant"},
		{"paragraph_separator", "topo" + "\u2029" + "variant"},
		{"edge_target", "n1<->n2"},
		{"ampersand", "a&b"},
		{"quote_and_backslash", `he said "\" ok`},
		{"literal_backslash_u_2028", `a\u2028b`},
		{"tab_and_newline", "a\tb\nc"},
		{"del_and_high_plane", "x\U0001F600y"},
		{"nul", "a\x00b"},
	}

	for _, w := range witnesses {
		t.Run(w.name, func(t *testing.T) {
			world := schema.NewWorld(7, w.value, "gate")
			world.SUT.Images = []schema.SUTImage{{
				Service: "kv_n1",
				Image:   w.value,
				Digest:  "sha256:" + "ab",
			}}
			world = world.Normalized()

			mine, err := EncodeWorld(&world)
			if err != nil {
				t.Fatalf("recorder EncodeWorld: %v", err)
			}
			theirs, err := schema.MarshalWorld(&world)
			if err != nil {
				t.Fatalf("schema MarshalWorld: %v", err)
			}
			if !bytes.Equal(mine, theirs) {
				off := cjson.FirstDiff(mine, theirs)
				t.Fatalf("the tree has two canonical forms; encoders disagree at byte %d\nrecorder %q\nschema   %q",
					off, mine, theirs)
			}

			// One canonical form means one identity. If the encoders diverge the
			// hash diverges with them, and a world stored under one code path
			// would be a different world when loaded by the other.
			hRec, err := HashWorld(&world)
			if err != nil {
				t.Fatalf("HashWorld: %v", err)
			}
			hSchema, err := world.Hash()
			if err != nil {
				t.Fatalf("World.Hash: %v", err)
			}
			if hRec != hSchema {
				t.Fatalf("world identity differs by code path: recorder %s, schema %s", hRec, hSchema)
			}

			// The end-to-end statement: bytes written by the recorder, read back
			// from disk, must load through the schema decoder. This is the check
			// that fails loudly when the two codecs' acceptance languages drift
			// apart, because schema.UnmarshalWorld re-encodes and byte-compares.
			path := filepath.Join(t.TempDir(), "w.thesis")
			if err := StoreWorld(path, &world); err != nil {
				t.Fatalf("StoreWorld: %v", err)
			}
			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(onDisk, mine) {
				t.Fatalf("bytes on disk differ from the encoding\n disk %q\n want %q", onDisk, mine)
			}
			back, err := schema.UnmarshalWorld(onDisk)
			if err != nil {
				t.Fatalf("schema.UnmarshalWorld rejected the file the recorder wrote: %v", err)
			}
			if back.TopologyVariant != w.value {
				t.Fatalf("topology_variant round-tripped to %q, want %q", back.TopologyVariant, w.value)
			}
			if _, err := DecodeWorld(onDisk); err != nil {
				t.Fatalf("recorder DecodeWorld rejected its own output: %v", err)
			}
		})
	}
}
