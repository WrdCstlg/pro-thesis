package cluster

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// extract.go is the Layer 1 feature contract: cluster_features.csv (the
// frozen column list below) and cluster_manifest.json. Both are derived,
// regenerable artifacts.

// FeaturesCSVHeader is the frozen column contract of cluster_features.csv.
const FeaturesCSVHeader = "world_id,component,exit_code,has_baseline,world_fixture,verdict_type,duration_ms,reason_token_count,reason_unique_words"

// ExtractManifest is cluster_manifest.json.
type ExtractManifest struct {
	CorpusSize          int    `json:"corpus_size"` // attributed + unattributed outcomes
	AttributedCount     int    `json:"attributed_count"`
	UnattributedCount   int    `json:"unattributed_count"`
	ExtractionTimestamp string `json:"extraction_timestamp"`
}

// WriteFeaturesCSV writes items in the given (deterministic) order.
func WriteFeaturesCSV(path string, items []Item) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.Write([]string{
		"world_id", "component", "exit_code", "has_baseline", "world_fixture",
		"verdict_type", "duration_ms", "reason_token_count", "reason_unique_words",
	}); err != nil {
		f.Close()
		return err
	}
	for _, it := range items {
		v := it.Features
		rec := []string{
			it.ID,
			v.Component,
			strconv.Itoa(v.ExitCode),
			strconv.FormatBool(v.HasBaseline),
			v.WorldFixture,
			v.VerdictType,
			strconv.FormatFloat(v.DurationMs, 'f', -1, 64),
			strconv.Itoa(v.ReasonTokenCount),
			strconv.Itoa(v.ReasonUniqueWords),
		}
		if err := w.Write(rec); err != nil {
			f.Close()
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadFeaturesCSV reads a CSV written by WriteFeaturesCSV. Reasons are not
// part of the CSV contract and come back empty (see EnrichReasons).
func ReadFeaturesCSV(path string) ([]Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: empty file, no header", path)
	}
	var items []Item
	for i, row := range rows[1:] {
		if len(row) != 9 {
			return nil, fmt.Errorf("%s row %d: %d columns, want 9", path, i+2, len(row))
		}
		var v VerdictFeatures
		v.Component = row[1]
		if v.ExitCode, err = strconv.Atoi(row[2]); err != nil {
			return nil, fmt.Errorf("%s row %d: exit_code: %w", path, i+2, err)
		}
		if v.HasBaseline, err = strconv.ParseBool(row[3]); err != nil {
			return nil, fmt.Errorf("%s row %d: has_baseline: %w", path, i+2, err)
		}
		v.WorldFixture = row[4]
		v.VerdictType = row[5]
		if v.DurationMs, err = strconv.ParseFloat(row[6], 64); err != nil {
			return nil, fmt.Errorf("%s row %d: duration_ms: %w", path, i+2, err)
		}
		if v.ReasonTokenCount, err = strconv.Atoi(row[7]); err != nil {
			return nil, fmt.Errorf("%s row %d: reason_token_count: %w", path, i+2, err)
		}
		if v.ReasonUniqueWords, err = strconv.Atoi(row[8]); err != nil {
			return nil, fmt.Errorf("%s row %d: reason_unique_words: %w", path, i+2, err)
		}
		level := "world"
		bundle := row[0]
		if v.Component == "bundle" {
			level = "bundle"
		} else if idx := lastSlash(row[0]); idx >= 0 {
			bundle = row[0][:idx]
		}
		items = append(items, Item{ID: row[0], Bundle: bundle, Level: level, Features: v})
	}
	return items, nil
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// WriteManifest writes cluster_manifest.json.
func WriteManifest(path string, m ExtractManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ReadManifest reads cluster_manifest.json.
func ReadManifest(path string) (ExtractManifest, error) {
	var m ExtractManifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}
