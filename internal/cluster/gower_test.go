// Package cluster tests cover the two-layer clustering engine: Gower
// distance, DBSCAN, HDBSCAN, the consensus rules, and the Ward hierarchy.
// Every test in this package was seen failing (build-fail) before the
// implementation existed, and the four spec mutations were each shown red
// and reverted; see the session report.
package cluster

import (
	"math"
	"testing"
)

// rangesForTests fixes the numeric ranges so known-value cases are exact.
func rangesForTests() Ranges {
	return Ranges{ExitCode: 4, DurationMs: 100, ReasonTokenCount: 10, ReasonUniqueWords: 5}
}

func TestGowerIdenticalIsZero(t *testing.T) {
	a := VerdictFeatures{
		Component: "no_crash", ExitCode: 3, HasBaseline: true,
		WorldFixture: "linear", VerdictType: "inconclusive",
		DurationMs: 42, ReasonTokenCount: 7, ReasonUniqueWords: 5,
	}
	if d := Gower(a, a, rangesForTests()); d != 0 {
		t.Fatalf("identical features must be distance 0, got %v", d)
	}
}

func TestGowerMaximalIsOne(t *testing.T) {
	r := rangesForTests()
	a := VerdictFeatures{Component: "x", ExitCode: 0, HasBaseline: false, WorldFixture: "a", VerdictType: "p", DurationMs: 0, ReasonTokenCount: 0, ReasonUniqueWords: 0}
	b := VerdictFeatures{Component: "y", ExitCode: 4, HasBaseline: true, WorldFixture: "b", VerdictType: "q", DurationMs: 100, ReasonTokenCount: 10, ReasonUniqueWords: 5}
	if d := Gower(a, b, r); d != 1 {
		t.Fatalf("maximally different features must be distance 1, got %v", d)
	}
}

func TestGowerMixedPartial(t *testing.T) {
	r := rangesForTests()
	base := VerdictFeatures{Component: "x", ExitCode: 0, HasBaseline: false, WorldFixture: "a", VerdictType: "p"}
	other := base
	other.Component = "y"       // 1.0 on one categorical
	other.ExitCode = 2          // |2-0|/4 = 0.5
	other.ReasonUniqueWords = 1 // 1/5 = 0.2
	want := (1.0 + 0.5 + 0.2) / 8
	if d := Gower(base, other, r); math.Abs(d-want) > 1e-12 {
		t.Fatalf("mixed partial distance: got %v, want %v", d, want)
	}
}

// A zero range (every observation shares one numeric value) must contribute
// 0, never divide by zero.
func TestGowerZeroRangeContributesZero(t *testing.T) {
	r := Ranges{} // all zero
	a := VerdictFeatures{ExitCode: 7, DurationMs: 9}
	b := VerdictFeatures{ExitCode: 7, DurationMs: 9}
	if d := Gower(a, b, r); d != 0 {
		t.Fatalf("zero ranges must contribute zero, got %v", d)
	}
}

func TestComputeRanges(t *testing.T) {
	fs := []VerdictFeatures{
		{ExitCode: 1, DurationMs: 10, ReasonTokenCount: 2, ReasonUniqueWords: 1},
		{ExitCode: 5, DurationMs: 60, ReasonTokenCount: 8, ReasonUniqueWords: 4},
		{ExitCode: 3, DurationMs: 30, ReasonTokenCount: 5, ReasonUniqueWords: 2},
	}
	r := ComputeRanges(fs)
	if r.ExitCode != 4 || r.DurationMs != 50 || r.ReasonTokenCount != 6 || r.ReasonUniqueWords != 3 {
		t.Fatalf("ranges: %+v", r)
	}
}

func TestDistanceMatrixIsSymmetricWithZeroDiagonal(t *testing.T) {
	fs := []VerdictFeatures{
		{Component: "a", ExitCode: 1},
		{Component: "b", ExitCode: 2},
		{Component: "a", ExitCode: 5},
	}
	m := DistanceMatrix(fs)
	if len(m) != 3 {
		t.Fatalf("matrix size %d", len(m))
	}
	for i := 0; i < 3; i++ {
		if m[i][i] != 0 {
			t.Fatalf("diagonal [%d][%d] = %v", i, i, m[i][i])
		}
		for j := 0; j < 3; j++ {
			if m[i][j] != m[j][i] {
				t.Fatalf("asymmetric at %d,%d: %v vs %v", i, j, m[i][j], m[j][i])
			}
		}
	}
}
