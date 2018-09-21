// Package util has some utility functions.
package util

func Min(a, b uint32) uint32 {
	if a <= b {
		return a
	}
	return b
}

func Max(a, b uint32) uint32 {
	if a >= b {
		return a
	}
	return b
}

func Bound(lower, middle, upper uint32) uint32 {
	return Min(Max(lower, middle), upper)
}

func TimeDiff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}
