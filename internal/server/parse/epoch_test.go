package parse

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden rewrites the committed golden snapshots from the current parser
// output. Run `go test ./internal/server/parse -run TestGoldenProjection -update`
// after an intentional parser change, in the same commit that bumps parse.Epoch.
var updateGolden = flag.Bool("update", false, "rewrite parser golden snapshots")

// goldenFixtures are the representative raw sessions whose parsed projection is
// snapshotted. Each names a raw JSONL input under testdata/golden and the agent
// whose reducer parses it. The identities in the fixtures are women in computing
// history (Grace Hopper, Ada Lovelace), matching the rest of the suite.
var goldenFixtures = []struct {
	name  string
	agent string
}{
	{name: "claude", agent: "claude"},
	{name: "codex", agent: "codex"},
}

// TestGoldenProjection is the guardrail that makes the parse.Epoch bump impossible
// to forget. It parses each fixture through the exact reducer + pricing path the
// live and reparse code use (reduceFunc), snapshots the resulting projection
// delta, and compares it to a committed golden file. Any change to parser or
// reducer output, or to the pricing the projection carries, makes a snapshot
// differ and fails this test by name, so a developer cannot change what the parser
// produces without being told to bump parse.Epoch (the fleet-wide reparse signal)
// and refresh the goldens. It needs no database: the reducer and pricing are pure.
func TestGoldenProjection(t *testing.T) {
	for _, f := range goldenFixtures {
		t.Run(f.name, func(t *testing.T) {
			rawPath := filepath.Join("testdata", "golden", f.name+".jsonl")
			raw, err := os.ReadFile(rawPath)
			if err != nil {
				t.Fatalf("read fixture %s: %v", rawPath, err)
			}

			// Drive the same seam the store calls: decode empty state, reduce the whole
			// session in one region from offset 0, and price the usage. The returned
			// delta is exactly what would be written to the projection.
			_, delta, err := reduceFunc(f.agent)(nil, raw, 0)
			if err != nil {
				t.Fatalf("reduce %s fixture: %v", f.name, err)
			}
			if len(delta.Messages) == 0 {
				t.Fatalf("%s fixture produced no messages; the fixture or parser is broken", f.name)
			}

			got, err := json.MarshalIndent(delta, "", "  ")
			if err != nil {
				t.Fatalf("marshal delta: %v", err)
			}
			got = append(got, '\n')

			goldenPath := filepath.Join("testdata", "golden", f.name+".json")
			if *updateGolden {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update to create it)", goldenPath, err)
			}
			if string(got) != string(want) {
				t.Errorf(`parser output for the %q fixture changed.

If this change is intentional, bump the parse.Epoch constant in epoch.go (it is
the fleet-wide reparse signal; bumping it makes deployed servers rebuild the
stored projection) and refresh the golden snapshots:

    go test ./internal/server/parse -run TestGoldenProjection -update

If it is NOT intentional, your change altered the parsed projection by accident.

--- want (golden)
%s
--- got (current)
%s`, f.name, string(want), string(got))
			}
		})
	}
}
