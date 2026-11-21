// Package textdiff renders a small line-based unified-style diff for plan output.
package textdiff

import "strings"

// Lines returns a diff of a and b with "-"/"+"/" " prefixes (LCS based; fine for small files).
func Lines(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var sb strings.Builder
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			sb.WriteString("  " + x[i] + "\n")
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			sb.WriteString("+ " + y[j] + "\n")
			j++
		default:
			sb.WriteString("- " + x[i] + "\n")
			i++
		}
	}
	return sb.String()
}
