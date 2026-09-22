package cluster

// gower.go implements Gower's general similarity coefficient over
// VerdictFeatures, standalone (no ML library): categorical fields (Component,
// WorldFixture, VerdictType, and HasBaseline treated as categorical) use
// simple matching (0 equal, 1 different); numeric fields (ExitCode,
// DurationMs, ReasonTokenCount, ReasonUniqueWords) use range-normalized
// Manhattan distance. The coefficient is the mean of the eight per-variable
// distances, so identical vectors are 0 and maximally different vectors 1.
//
// Ranges come from the population being compared (the unattributed pool for
// Layer 1, the centroid set for Layer 2). A variable whose range is zero
// contributes 0 to every pair: it carries no information, and normalizing by
// zero must never divide.

// Ranges holds the numeric normalization ranges (max - min) of one
// population.
type Ranges struct {
	ExitCode          float64
	DurationMs        float64
	ReasonTokenCount  float64
	ReasonUniqueWords float64
}

// ComputeRanges derives the ranges of a feature population.
func ComputeRanges(fs []VerdictFeatures) Ranges {
	var r Ranges
	if len(fs) == 0 {
		return r
	}
	minX, maxX := float64(fs[0].ExitCode), float64(fs[0].ExitCode)
	minD, maxD := fs[0].DurationMs, fs[0].DurationMs
	minT, maxT := float64(fs[0].ReasonTokenCount), float64(fs[0].ReasonTokenCount)
	minU, maxU := float64(fs[0].ReasonUniqueWords), float64(fs[0].ReasonUniqueWords)
	for _, f := range fs[1:] {
		x, d := float64(f.ExitCode), f.DurationMs
		t, u := float64(f.ReasonTokenCount), float64(f.ReasonUniqueWords)
		minX, maxX = min(minX, x), max(maxX, x)
		minD, maxD = min(minD, d), max(maxD, d)
		minT, maxT = min(minT, t), max(maxT, t)
		minU, maxU = min(minU, u), max(maxU, u)
	}
	return Ranges{
		ExitCode:          maxX - minX,
		DurationMs:        maxD - minD,
		ReasonTokenCount:  maxT - minT,
		ReasonUniqueWords: maxU - minU,
	}
}

func cat(a, b string) float64 {
	if a == b {
		return 0
	}
	return 1
}

func num(a, b, rng float64) float64 {
	if rng == 0 {
		return 0
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d / rng
}

// Gower is the Gower distance between two feature vectors under r.
func Gower(a, b VerdictFeatures, r Ranges) float64 {
	sum := cat(a.Component, b.Component) +
		cat(a.WorldFixture, b.WorldFixture) +
		cat(a.VerdictType, b.VerdictType)
	if a.HasBaseline != b.HasBaseline {
		sum++
	}
	sum += num(float64(a.ExitCode), float64(b.ExitCode), r.ExitCode)
	sum += num(a.DurationMs, b.DurationMs, r.DurationMs)
	sum += num(float64(a.ReasonTokenCount), float64(b.ReasonTokenCount), r.ReasonTokenCount)
	sum += num(float64(a.ReasonUniqueWords), float64(b.ReasonUniqueWords), r.ReasonUniqueWords)
	return sum / FeatureDimensions
}

// DistanceMatrix computes the symmetric Gower matrix over a population, with
// ranges derived from that same population.
func DistanceMatrix(fs []VerdictFeatures) [][]float64 {
	r := ComputeRanges(fs)
	n := len(fs)
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			d := Gower(fs[i], fs[j], r)
			m[i][j] = d
			m[j][i] = d
		}
	}
	return m
}
