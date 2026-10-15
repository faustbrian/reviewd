package acceptancefixture

// Percentage returns part as a percentage of total.
// total must be positive; fractional percentages must be retained.
func Percentage(part, total int) float64 {
	return float64(part/total) * 100
}
